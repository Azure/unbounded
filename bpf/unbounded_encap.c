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

#include <linux/bpf.h>
#include <linux/pkt_cls.h>
#include <linux/if_ether.h>
#include <linux/ip.h>
#include <linux/ipv6.h>
#include <linux/udp.h>
#include <linux/in.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

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

// --- Helpers ---

// derive_mac_from_ipv4 fills a locally-administered destination MAC from
// an IPv4 address in network byte order. Mirrors Go's TunnelMACFromIP.
static __always_inline void derive_mac_from_ipv4(__u8 *mac, __u32 ip_be32)
{
	mac[0] = 0x02;
	mac[1] = (ip_be32 >> 24) & 0xFF;
	mac[2] = (ip_be32 >> 16) & 0xFF;
	mac[3] = (ip_be32 >> 8) & 0xFF;
	mac[4] = ip_be32 & 0xFF;
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

// set_tunnel_key_from_endpoint populates a bpf_tunnel_key from a
// remote_endpoint, choosing IPv4 vs IPv6 framing based on the v4-mapped
// check, and calls bpf_skb_set_tunnel_key.
static __always_inline void set_tunnel_key_from_endpoint(struct __sk_buff *skb,
							 const struct tunnel_nexthop *nh)
{
	struct bpf_tunnel_key tkey = {};
	tkey.tunnel_ttl = 64;
	tkey.tunnel_id = nh->vni;

	if (is_v4_mapped(nh->remote_endpoint)) {
		__u32 v4_be;
		__builtin_memcpy(&v4_be, &nh->remote_endpoint[12], sizeof(v4_be));
		tkey.remote_ipv4 = v4_be;
		bpf_skb_set_tunnel_key(skb, &tkey, sizeof(tkey), 0);
	} else {
		__builtin_memcpy(tkey.remote_ipv6, nh->remote_endpoint, 16);
		bpf_skb_set_tunnel_key(skb, &tkey, sizeof(tkey),
				       BPF_F_TUNINFO_IPV6);
	}
}

// --- TC entry point ---

SEC("tc")
int unbounded_encap(struct __sk_buff *skb)
{
	void *data = (void *)(long)skb->data;
	void *data_end = (void *)(long)skb->data_end;

	struct ethhdr *eth = data;
	if ((void *)(eth + 1) > data_end)
		return TC_ACT_OK;

	struct lpm_key key = { .prefixlen = 128 };

	if (eth->h_proto == bpf_htons(ETH_P_IP)) {
		struct iphdr *iph = (void *)(eth + 1);
		if ((void *)(iph + 1) > data_end)
			return TC_ACT_OK;

		build_v4_mapped_addr(key.addr, iph->daddr);
	} else if (eth->h_proto == bpf_htons(ETH_P_IPV6)) {
		struct ipv6hdr *ip6h = (void *)(eth + 1);
		if ((void *)(ip6h + 1) > data_end)
			return TC_ACT_OK;

		__builtin_memcpy(key.addr, &ip6h->daddr, 16);
	} else {
		return TC_ACT_OK;
	}

	struct tunnel_endpoint *ep = bpf_map_lookup_elem(&unb_endpts, &key);
	if (!ep || ep->count == 0)
		return TC_ACT_OK;

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
		if (idx < 0 || idx >= MAX_NEXTHOPS)
			return TC_ACT_OK;
	}

	struct tunnel_nexthop nh = ep->nexthops[idx];

	// Destination MAC is derived from the low 32 bits of the underlay
	// endpoint; for v4 underlay this is the IPv4 address itself, for
	// v6 underlay this is the trailing 32 bits (sufficient to drive
	// the bpf_redirect to the right underlay device).
	derive_mac_from_ipv4(eth->h_dest, endpoint_low32(nh.remote_endpoint));

	if (needs_tunnel_key(nh.protocol))
		set_tunnel_key_from_endpoint(skb, &nh);

	return bpf_redirect(nh.ifindex, 0);
}

char _license[] SEC("license") = "MIT";
