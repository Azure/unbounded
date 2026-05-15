/*
 * Copyright (c) Microsoft Corporation.
 * Licensed under the MIT License
 */

// SPDX-License-Identifier: MIT
// unbounded_encap.c -- TC egress classifier for the eBPF tunnel dataplane.
//
// A single SEC("tc") program (unbounded_encap) runs on the egress hook of
// the underlay-facing interface. Both IPv4 and IPv6 overlay destinations
// are stored in one LPM trie (unb_endpts) keyed on a 16-byte address.
// IPv4 destinations are stored in IPv4-mapped IPv6 form (::ffff:<v4>), so
// the trie's longest-prefix-match naturally segregates v4 entries (which
// live under ::ffff:0:0/96) from native v6 entries.
//
// The underlay endpoint stored in each nexthop is also a 16-byte address.
// IPv4 underlay addresses live in IPv4-mapped form; native v6 underlay
// addresses are stored as-is. The BPF program inspects the first 12 bytes
// of remote_endpoint at runtime to decide whether to call
// bpf_skb_set_tunnel_key with BPF_F_TUNINFO_IPV6 or with remote_ipv4.
//
// Single-nexthop entries (the common endpoint-node case) forward
// unconditionally; the healthy flag only gates ECMP selection on
// multi-nexthop entries (where it lets us drain a known-bad gateway
// without dropping traffic destined to a singleton peer).

#include "vmlinux.h"

#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>
#include <bpf/bpf_core_read.h>

// vmlinux.h dumped from BTF carries struct definitions but neither numeric
// uAPI constants nor every typedef libbpf's bpf_helper_defs.h depends on.
// Re-declare the handful we use here as plain literals (the values are
// stable kernel ABI).
#define ETH_P_IP             0x0800
#define ETH_P_IPV6           0x86DD
#define IPPROTO_TCP          6
#define IPPROTO_UDP          17
#define IPPROTO_SCTP         132
#define TC_ACT_OK            0
#define BPF_F_TUNINFO_IPV6   (1ULL << 0)
#define BPF_F_NO_PREALLOC    (1U << 0)
#define BPF_MAP_TYPE_LPM_TRIE 11
#define BPF_MAP_TYPE_RINGBUF  27

// Maximum number of nexthops per CIDR prefix.
#define MAX_NEXTHOPS 4

// Protocol constants matching internal/net/ebpf.TunnelProto*.
#define PROTO_GENEVE    1
#define PROTO_VXLAN     2
#define PROTO_IPIP      3
#define PROTO_WIREGUARD 4
#define PROTO_NONE      5

// --- Map key / value types ---

struct lpm_key {
	__u32 prefixlen;
	__u8  addr[16];
};

struct tunnel_nexthop {
	__u8  remote_endpoint[16]; // ::ffff:<v4> for v4; native for v6 underlay
	__u32 vni;
	__u32 ifindex;
	__u32 healthy; // 0 = withdrawn; non-zero = eligible for selection
	__u32 protocol;
};

struct tunnel_endpoint {
	struct tunnel_nexthop nexthops[MAX_NEXTHOPS];
	__u32 count;
};

// trace_event is one record per packet processed by unbounded_encap. The
// userspace consumer (cmd/unroute --trace) reads these from the unb_trace
// ringbuf via cilium/ebpf's ringbuf.Reader. Field layout is part of the
// user-space ABI: keep it stable, append new fields at the end.
struct unb_trace_event {
	__u64 ts_ns;
	__u32 cpu;
	__u32 skb_len;
	__u16 eth_proto;
	__u8  ip_proto;       // L4 protocol (IPPROTO_TCP/UDP/ICMP/...); 0 if not parsed
	__u8  _pad0;
	__u16 sport;          // host byte order; 0 if no L4
	__u16 dport;          // host byte order; 0 if no L4
	__u8  saddr[16];
	__u8  daddr[16];
	__u32 lpm_prefixlen; // 0 = miss or non-IP
	__u32 nh_count;      // 0 = miss
	__s32 chosen_idx;    // -1 = no eligible nexthop
	__u8  remote[16];
	__u32 vni;
	__u32 ifindex;
	__u32 protocol;
	__u8  needs_key;
	__u8  _pad1[3];
	__s32 set_key_ret;
	__s32 redirect_ret;
	__s32 verdict;
};

// Force bpf2go to emit a Go binding for struct unb_trace_event by referencing
// it from a BTF-visible global. Marked volatile + __unused so the compiler
// keeps the type description but elides any code that depends on it.
volatile const struct unb_trace_event _unb_trace_event_layout __attribute__((unused));

// --- Map ---
//
// Single LPM trie keyed on a 16-byte address. The kernel limits map names
// to 15 chars (+ NUL), so we keep the name short for readable
// "bpftool map dump" output.

struct {
	__uint(type, BPF_MAP_TYPE_LPM_TRIE);
	__uint(max_entries, 16384);
	__type(key, struct lpm_key);
	__type(value, struct tunnel_endpoint);
	__uint(map_flags, BPF_F_NO_PREALLOC);
} unb_endpts SEC(".maps");

// unb_trace carries one trace_event per packet to userspace. The producer
// path is unconditional: we always populate the event on stack, then call
// bpf_ringbuf_reserve. When no consumer is attached the ring saturates
// quickly and reserve returns NULL; we drop the event and continue.
//
// Cost per packet with no consumer: one failed reserve (a single CAS in
// the kernel). With a consumer attached the full 256 KiB is available as
// burst headroom before reserve starts failing.

#define UNB_TRACE_RB_SIZE (256 * 1024)

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, UNB_TRACE_RB_SIZE);
} unb_trace SEC(".maps");

// --- Helpers ---

// derive_mac_from_ipv4_bytes fills a locally-administered destination MAC
// from a 4-byte IPv4 address in network byte order. Mirrors Go's
// TunnelMACFromIP. Taking the address as a byte pointer (not a u32)
// avoids any endianness confusion: the bytes are copied directly into
// the MAC in the order they appear.
static __always_inline void derive_mac_from_ipv4_bytes(__u8 *mac, const __u8 *ip4_be)
{
	mac[0] = 0x02;
	mac[1] = ip4_be[0];
	mac[2] = ip4_be[1];
	mac[3] = ip4_be[2];
	mac[4] = ip4_be[3];
	mac[5] = 0xFF;
}

// __jhash_final performs the final mixing round of the Jenkins hash. Used
// by jhash_3words for the per-(flow, nexthop, endpoint) weighting inside
// hrw_select. The flow hash itself comes from the kernel's skb->hash via
// bpf_get_hash_recalc, so we do not need the full multi-round jhash2 in
// this program.
#define __jhash_final(a, b, c) do {                     \
	c ^= b; c -= (b << 14) | (b >> 18);             \
	a ^= c; a -= (c << 11) | (c >> 21);             \
	b ^= a; b -= (a << 25) | (a >>  7);             \
	c ^= b; c -= (b << 16) | (b >> 16);             \
	a ^= c; a -= (c <<  4) | (c >> 28);             \
	b ^= a; b -= (a << 14) | (a >> 18);             \
	c ^= b; c -= (b << 24) | (b >>  8);             \
} while (0)

#define JHASH_INITVAL 0xdeadbeef

// jhash_3words mirrors Linux's jhash_3words helper. We use it inside
// hrw_select to compute a deterministic per-(flow_hash, index, endpoint)
// weight; the full flow hash is provided by the kernel separately.
static __always_inline __u32 jhash_3words(__u32 a, __u32 b, __u32 c)
{
	a += JHASH_INITVAL + (3 << 2);
	b += JHASH_INITVAL + (3 << 2);
	c += JHASH_INITVAL + (3 << 2);
	__jhash_final(a, b, c);
	return c;
}

// endpoint_low32 returns the low 32 bits of a 16-byte endpoint, used as a
// hash mixing input. For IPv4-mapped underlays this is the IPv4 address
// itself; for native v6 underlays it is the trailing 32 bits, which is
// well-distributed across most address plans.
static __always_inline __u32 endpoint_low32(const __u8 *endpoint16)
{
	__u32 lo;
	__builtin_memcpy(&lo, endpoint16 + 12, sizeof(lo));
	return lo;
}

// is_v4_mapped reports whether a 16-byte address is in ::ffff:0:0/96.
static __always_inline int is_v4_mapped(const __u8 *addr16)
{
	__u64 zeros;
	__u32 mid;
	__builtin_memcpy(&zeros, addr16,     sizeof(zeros));
	__builtin_memcpy(&mid,   addr16 + 8, sizeof(mid));
	return zeros == 0 && mid == bpf_htonl(0x0000ffff);
}

// needs_tunnel_key reports whether the given protocol expects the BPF
// program to populate bpf_tunnel_key. GENEVE/VXLAN/IPIP do; WireGuard and
// None (plain redirect) do not.
static __always_inline int needs_tunnel_key(__u32 protocol)
{
	return protocol == PROTO_GENEVE ||
	       protocol == PROTO_VXLAN  ||
	       protocol == PROTO_IPIP;
}

// hrw_select picks a nexthop using Highest Random Weight hashing among
// healthy nexthops. Healthcheck packets are only routed through the
// single-nexthop fast path in unbounded_encap; multi-nexthop entries
// only carry production traffic, so this loop unconditionally filters
// out unhealthy nexthops.
static __always_inline int hrw_select(struct tunnel_nexthop *nhs, __u32 count,
				      __u32 flow_hash)
{
	__u32 best_weight = 0;
	int best_idx = -1;

#pragma unroll
	for (int i = 0; i < MAX_NEXTHOPS; i++) {
		if ((__u32)i >= count)
			break;
		if (!nhs[i].healthy)
			continue;
		__u32 w = jhash_3words(flow_hash, (__u32)i,
				       endpoint_low32(nhs[i].remote_endpoint));
		if (best_idx < 0 || w > best_weight) {
			best_weight = w;
			best_idx = i;
		}
	}
	return best_idx;
}

// build_v4_mapped_addr fills a 16-byte buffer with the IPv4-mapped IPv6
// form of a 4-byte IPv4 address (already in network byte order).
static __always_inline void build_v4_mapped_addr(__u8 *out16, __u32 v4_be)
{
	__builtin_memset(out16, 0, 10);
	out16[10] = 0xff;
	out16[11] = 0xff;
	__builtin_memcpy(&out16[12], &v4_be, sizeof(v4_be));
}

// trace_l4 reads the L4 protocol's first two 16-bit fields (source &
// destination ports for TCP/UDP/SCTP) into the trace event. For
// non-port protocols (e.g. ICMP) the bytes still get copied but only
// ip_proto carries meaning; the caller is expected to check ip_proto
// before rendering port numbers.
static __always_inline void trace_l4(struct unb_trace_event *ev, __u8 proto,
				     const void *l4, void *data_end)
{
	ev->ip_proto = proto;

	switch (proto) {
	case IPPROTO_TCP:
	case IPPROTO_UDP:
	case IPPROTO_SCTP: {
		// First 4 bytes of TCP/UDP/SCTP headers are sport/dport, BE.
		__u16 ports[2];
		if ((const void *)((const __u8 *)l4 + sizeof(ports)) > data_end)
			return;

		__builtin_memcpy(ports, l4, sizeof(ports));
		ev->sport = bpf_ntohs(ports[0]);
		ev->dport = bpf_ntohs(ports[1]);
		break;
	}
	default:
		break;
	}
}


// set_tunnel_key_from_endpoint populates a struct bpf_tunnel_key from a
// remote_endpoint, choosing IPv4 vs IPv6 framing based on the v4-mapped
// check, and calls bpf_skb_set_tunnel_key. Returns the helper's return
// value so callers can record it in trace events.
//
// The size argument is bpf_core_type_size(struct bpf_tunnel_key): the
// cilium/ebpf loader rewrites this to the *running* kernel's actual
// sizeof(struct bpf_tunnel_key) at load time. The helper then takes its
// fast path (no compat-size memset), regardless of kernel version. This
// is the CO-RE pattern: compile once against the BTF in bpf/vmlinux.h,
// run anywhere with a different struct layout.
//
// remote_endpoint stores the underlay IP in network byte order (bytes
// [12..15] for the v4-mapped form). The kernel side runs
// cpu_to_be32(remote_ipv4) on the value we pass, so we must give it
// the IP in *native* byte order: bpf_ntohl converts the memcpy'd
// network-order bytes into the native form the kernel expects.
static __always_inline long set_tunnel_key_from_endpoint(struct __sk_buff *skb,
							 const struct tunnel_nexthop *nh)
{
	struct bpf_tunnel_key tkey = {};
	tkey.tunnel_ttl = 64;
	tkey.tunnel_id = nh->vni;

	if (is_v4_mapped(nh->remote_endpoint)) {
		__u32 v4_net;
		__builtin_memcpy(&v4_net, &nh->remote_endpoint[12], sizeof(v4_net));
		tkey.remote_ipv4 = bpf_ntohl(v4_net);
		return bpf_skb_set_tunnel_key(skb, &tkey,
					      bpf_core_type_size(struct bpf_tunnel_key),
					      0);
	}

	__builtin_memcpy(tkey.remote_ipv6, nh->remote_endpoint, 16);
	return bpf_skb_set_tunnel_key(skb, &tkey,
				      bpf_core_type_size(struct bpf_tunnel_key),
				      BPF_F_TUNINFO_IPV6);
}

// emit_trace tries to copy a fully-populated trace_event into the
// ringbuf. If no consumer is attached (or one is too slow), the ring is
// full and bpf_ringbuf_reserve returns NULL; we silently drop the event.
// Cost per dropped packet: one failed reserve (a single CAS).
static __always_inline void emit_trace(const struct unb_trace_event *ev)
{
	struct unb_trace_event *out = bpf_ringbuf_reserve(&unb_trace, sizeof(*out), 0);
	if (!out)
		return;

	__builtin_memcpy(out, ev, sizeof(*out));
	bpf_ringbuf_submit(out, 0);
}

// --- TC entry point ---

SEC("tc")
int unbounded_encap(struct __sk_buff *skb)
{
	void *data = (void *)(long)skb->data;
	void *data_end = (void *)(long)skb->data_end;

	struct unb_trace_event ev = {};
	ev.ts_ns = bpf_ktime_get_ns();
	ev.cpu = bpf_get_smp_processor_id();
	ev.skb_len = skb->len;
	ev.chosen_idx = -1;

	struct ethhdr *eth = data;
	if ((void *)(eth + 1) > data_end) {
		ev.verdict = TC_ACT_OK;
		emit_trace(&ev);
		return TC_ACT_OK;
	}

	ev.eth_proto = bpf_ntohs(eth->h_proto);

	struct lpm_key key = { .prefixlen = 128 };

	if (eth->h_proto == bpf_htons(ETH_P_IP)) {
		struct iphdr *iph = (void *)(eth + 1);
		if ((void *)(iph + 1) > data_end) {
			ev.verdict = TC_ACT_OK;
			emit_trace(&ev);
			return TC_ACT_OK;
		}

		build_v4_mapped_addr(key.addr, iph->daddr);
		build_v4_mapped_addr(ev.saddr, iph->saddr);
		build_v4_mapped_addr(ev.daddr, iph->daddr);
		trace_l4(&ev, iph->protocol, (const void *)iph + (iph->ihl * 4), data_end);
	} else if (eth->h_proto == bpf_htons(ETH_P_IPV6)) {
		struct ipv6hdr *ip6h = (void *)(eth + 1);
		if ((void *)(ip6h + 1) > data_end) {
			ev.verdict = TC_ACT_OK;
			emit_trace(&ev);
			return TC_ACT_OK;
		}

		__builtin_memcpy(key.addr, &ip6h->daddr, 16);
		__builtin_memcpy(ev.saddr, &ip6h->saddr, 16);
		__builtin_memcpy(ev.daddr, &ip6h->daddr, 16);
		// Treats the first L4 nexthdr verbatim; IPv6 extension headers
		// are uncommon in pod traffic and any extension parser would
		// blow the verifier instruction budget. If the next header
		// isn't TCP/UDP/SCTP we just leave the ports zeroed.
		trace_l4(&ev, ip6h->nexthdr, ip6h + 1, data_end);
	} else {
		ev.verdict = TC_ACT_OK;
		emit_trace(&ev);
		return TC_ACT_OK;
	}

	struct tunnel_endpoint *ep = bpf_map_lookup_elem(&unb_endpts, &key);
	if (!ep || ep->count == 0) {
		ev.verdict = TC_ACT_OK;
		emit_trace(&ev);
		return TC_ACT_OK;
	}

	ev.lpm_prefixlen = key.prefixlen;
	ev.nh_count = ep->count;

	int idx;
	if (ep->count == 1) {
		// Common endpoint-node case: one nexthop. Forward
		// unconditionally - the healthy flag is meaningless when there
		// is no alternative path, and dropping locally only ensures
		// the destination cannot recover via real traffic.
		idx = 0;
	} else {
		// Multi-nexthop ECMP path: skip unhealthy nexthops so we don't
		// load-balance through a known-bad gateway.
		__u32 hash = bpf_get_hash_recalc(skb);
		idx = hrw_select(ep->nexthops, ep->count, hash);
		if (idx < 0 || idx >= MAX_NEXTHOPS) {
			ev.verdict = TC_ACT_OK;
			emit_trace(&ev);
			return TC_ACT_OK;
		}
	}

	struct tunnel_nexthop nh = ep->nexthops[idx];

	ev.chosen_idx = idx;
	__builtin_memcpy(ev.remote, nh.remote_endpoint, 16);
	ev.vni = nh.vni;
	ev.ifindex = nh.ifindex;
	ev.protocol = nh.protocol;

	// Destination MAC is derived from the low 32 bits of the underlay
	// endpoint; for v4 underlay this is the IPv4 address itself, for
	// v6 underlay this is the trailing 32 bits (sufficient to drive
	// the bpf_redirect to the right underlay device).
	derive_mac_from_ipv4_bytes(eth->h_dest, &nh.remote_endpoint[12]);

	if (needs_tunnel_key(nh.protocol)) {
		ev.needs_key = 1;
		ev.set_key_ret = (__s32)set_tunnel_key_from_endpoint(skb, &nh);
	}

	long redir = bpf_redirect(nh.ifindex, 0);
	ev.redirect_ret = (__s32)redir;
	ev.verdict = (__s32)redir;
	emit_trace(&ev);

	return redir;
}

char _license[] SEC("license") = "MIT";
