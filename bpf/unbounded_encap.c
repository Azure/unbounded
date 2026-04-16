/*
 * Copyright (c) Microsoft Corporation.
 * Licensed under the MIT License
 */

// SPDX-License-Identifier: GPL-2.0
// tunnel_encap.c -- TC egress classifier for eBPF tunnel dataplane.
//
// Runs on unbounded0 egress. Intercepts packets destined to remote overlay
// CIDRs, selects a nexthop using consistent hashing (HRW), and redirects
// to the appropriate tunnel interface. Supports ECMP with health-aware
// nexthop selection.

#include <linux/bpf.h>
#include <linux/pkt_cls.h>
#include <linux/if_ether.h>
#include <linux/ip.h>
#include <linux/ipv6.h>
#include <linux/udp.h>
#include <linux/in.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

// Healthcheck UDP port -- probes always forwarded to unhealthy peers.
#define HEALTHCHECK_PORT 9997

// Maximum number of nexthops per CIDR prefix.
#define MAX_NEXTHOPS 4

// --- Nexthop and endpoint structures ---

struct tunnel_nexthop {
__u32 remote_ipv4;
__u32 vni;
__u32 ifindex;
__u32 flags;
__u32 protocol;
};

struct tunnel_endpoint_v4 {
struct tunnel_nexthop nexthops[MAX_NEXTHOPS];
__u32 count;
};

struct lpm_key_v4 {
__u32 prefixlen;
__u32 addr;
};

struct tunnel_nexthop_v6 {
union {
__u32 remote_ipv4;
__u32 remote_ipv6[4];
};
__u32 vni;
__u32 ifindex;
__u32 flags;
__u32 protocol;
};

struct tunnel_endpoint_v6 {
struct tunnel_nexthop_v6 nexthops[MAX_NEXTHOPS];
__u32 count;
};

struct lpm_key_v6 {
__u32 prefixlen;
__u32 addr[4];
};

// Tunnel endpoint flags.
#define TUNNEL_F_SET_KEY       0x01
#define TUNNEL_F_HEALTHY       0x02
#define TUNNEL_F_IPV6_UNDERLAY 0x04

// Protocol constants (display/debugging only).
#define PROTO_GENEVE    1
#define PROTO_VXLAN     2
#define PROTO_IPIP      3
#define PROTO_WIREGUARD 4
#define PROTO_NONE      5

// --- Maps ---

struct {
__uint(type, BPF_MAP_TYPE_LPM_TRIE);
__uint(max_entries, 16384);
__type(key, struct lpm_key_v4);
__type(value, struct tunnel_endpoint_v4);
__uint(map_flags, BPF_F_NO_PREALLOC);
} unbounded_endpoints_v4 SEC(".maps");

struct {
__uint(type, BPF_MAP_TYPE_LPM_TRIE);
__uint(max_entries, 16384);
__type(key, struct lpm_key_v6);
__type(value, struct tunnel_endpoint_v6);
__uint(map_flags, BPF_F_NO_PREALLOC);
} unbounded_endpoints_v6 SEC(".maps");

// --- Helpers ---

static __always_inline void derive_mac_from_ipv4(__u8 *mac, __u32 ip_be32)
{
mac[0] = 0x02;
mac[1] = (ip_be32 >> 24) & 0xFF;
mac[2] = (ip_be32 >> 16) & 0xFF;
mac[3] = (ip_be32 >> 8) & 0xFF;
mac[4] = ip_be32 & 0xFF;
mac[5] = 0xFF;
}

// jhash_3words -- Jenkins hash for HRW nexthop selection.
static __always_inline __u32 jhash_3words(__u32 a, __u32 b, __u32 c)
{
a += 0xdeadbeef + (3 << 2);
b += 0xdeadbeef + (3 << 2);
c += 0xdeadbeef + (3 << 2);
c ^= b; c -= (b << 14) | (b >> 18);
a ^= c; a -= (c << 11) | (c >> 21);
b ^= a; b -= (a << 25) | (a >> 7);
c ^= b; c -= (b << 16) | (b >> 16);
a ^= c; a -= (c << 4)  | (c >> 28);
b ^= a; b -= (a << 14) | (a >> 18);
c ^= b; c -= (b << 24) | (b >> 8);
return c;
}

// hrw_select picks a nexthop using Highest Random Weight hashing.
// Only healthy nexthops are considered unless is_healthcheck is set.
static __always_inline int hrw_select(struct tunnel_nexthop *nhs, __u32 count,
__u32 flow_hash, int is_healthcheck)
{
__u32 best_weight = 0;
int best_idx = -1;

#pragma unroll
for (int i = 0; i < MAX_NEXTHOPS; i++) {
if ((__u32)i >= count)
break;
if (!is_healthcheck && !(nhs[i].flags & TUNNEL_F_HEALTHY))
continue;
__u32 w = jhash_3words(flow_hash, (__u32)i, nhs[i].remote_ipv4);
if (best_idx < 0 || w > best_weight) {
best_weight = w;
best_idx = i;
}
}
return best_idx;
}

// Compute 5-tuple flow hash for consistent ECMP.
static __always_inline __u32 flow_hash_v4(struct iphdr *iph, void *data_end)
{
__u32 ports = 0;
if (iph->protocol == IPPROTO_TCP || iph->protocol == IPPROTO_UDP) {
__u16 *ph = (void *)(iph + 1);
if ((void *)(ph + 2) <= data_end)
ports = ((__u32)ph[0] << 16) | ph[1];
}
return jhash_3words(iph->saddr, iph->daddr,
((__u32)iph->protocol << 24) | ports);
}

// --- IPv4 handler ---

static __always_inline int handle_ipv4(struct __sk_buff *skb,
struct ethhdr *eth, struct iphdr *iph, void *data_end)
{
struct lpm_key_v4 key = { .prefixlen = 32, .addr = iph->daddr };

struct tunnel_endpoint_v4 *ep =
bpf_map_lookup_elem(&unbounded_endpoints_v4, &key);
if (!ep || ep->count == 0)
return TC_ACT_OK;

int is_hc = 0;
if (iph->protocol == IPPROTO_UDP) {
struct udphdr *udp = (void *)(iph + 1);
if ((void *)(udp + 1) <= data_end &&
    bpf_ntohs(udp->dest) == HEALTHCHECK_PORT)
is_hc = 1;
}

__u32 fh = flow_hash_v4(iph, data_end);
int idx = hrw_select(ep->nexthops, ep->count, fh, is_hc);
if (idx < 0 || idx >= MAX_NEXTHOPS)
return TC_ACT_OK;

struct tunnel_nexthop nh = ep->nexthops[idx];

derive_mac_from_ipv4(eth->h_dest, nh.remote_ipv4);

if (nh.flags & TUNNEL_F_SET_KEY) {
struct bpf_tunnel_key tkey = {};
tkey.remote_ipv4 = nh.remote_ipv4;
tkey.tunnel_id = nh.vni;
tkey.tunnel_ttl = 64;
bpf_skb_set_tunnel_key(skb, &tkey, 28, 0);
}

return bpf_redirect(nh.ifindex, 0);
}

// --- IPv6 handler ---

static __always_inline int handle_ipv6(struct __sk_buff *skb,
struct ethhdr *eth, struct ipv6hdr *ip6h, void *data_end)
{
struct lpm_key_v6 key = { .prefixlen = 128 };
__builtin_memcpy(key.addr, &ip6h->daddr, 16);

struct tunnel_endpoint_v6 *ep =
bpf_map_lookup_elem(&unbounded_endpoints_v6, &key);
if (!ep || ep->count == 0)
return TC_ACT_OK;

__u32 src_lo = ((__u32 *)&ip6h->saddr)[3];
__u32 dst_lo = ((__u32 *)&ip6h->daddr)[3];
__u32 fh = jhash_3words(src_lo, dst_lo, (__u32)ip6h->nexthdr);

int is_hc = 0;
if (ip6h->nexthdr == IPPROTO_UDP) {
struct udphdr *udp = (void *)(ip6h + 1);
if ((void *)(udp + 1) <= data_end &&
    bpf_ntohs(udp->dest) == HEALTHCHECK_PORT)
is_hc = 1;
}

int idx = hrw_select(
(struct tunnel_nexthop *)ep->nexthops, ep->count, fh, is_hc);
if (idx < 0 || idx >= MAX_NEXTHOPS)
return TC_ACT_OK;

struct tunnel_nexthop_v6 nh = ep->nexthops[idx];

derive_mac_from_ipv4(eth->h_dest, nh.remote_ipv4);

if (nh.flags & TUNNEL_F_SET_KEY) {
struct bpf_tunnel_key tkey = {};
tkey.tunnel_ttl = 64;
if (nh.flags & TUNNEL_F_IPV6_UNDERLAY) {
__builtin_memcpy(tkey.remote_ipv6, nh.remote_ipv6, 16);
tkey.tunnel_id = nh.vni;
bpf_skb_set_tunnel_key(skb, &tkey, 28,
BPF_F_TUNINFO_IPV6);
} else {
tkey.remote_ipv4 = nh.remote_ipv4;
tkey.tunnel_id = nh.vni;
bpf_skb_set_tunnel_key(skb, &tkey, 28, 0);
}
}

return bpf_redirect(nh.ifindex, 0);
}

SEC("tc")
int unbounded_encap(struct __sk_buff *skb)
{
void *data = (void *)(long)skb->data;
void *data_end = (void *)(long)skb->data_end;

struct ethhdr *eth = data;
if ((void *)(eth + 1) > data_end)
return TC_ACT_OK;

if (eth->h_proto == bpf_htons(ETH_P_IP)) {
struct iphdr *iph = (void *)(eth + 1);
if ((void *)(iph + 1) > data_end)
return TC_ACT_OK;
return handle_ipv4(skb, eth, iph, data_end);
}

if (eth->h_proto == bpf_htons(ETH_P_IPV6)) {
struct ipv6hdr *ip6h = (void *)(eth + 1);
if ((void *)(ip6h + 1) > data_end)
return TC_ACT_OK;
return handle_ipv6(skb, eth, ip6h, data_end);
}

return TC_ACT_OK;
}

char _license[] SEC("license") = "GPL";
