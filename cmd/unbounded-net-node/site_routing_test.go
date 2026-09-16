// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/netip"
	"reflect"
	"slices"
	"testing"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

func TestExcludeLocalCIDRs(t *testing.T) {
	for _, tt := range []struct {
		name, local string
		cidrs, want []string
	}{
		{"exact", "10.0.0.0/24", []string{"10.0.0.0/24"}, nil},
		{"covered", "10.0.0.0/16", []string{"10.0.1.0/24"}, nil},
		{"partial", "10.0.0.64/26", []string{"10.0.0.0/24"}, []string{"10.0.0.0/26", "10.0.0.128/25"}},
		{"host bits", "10.0.0.65/26", []string{"10.0.0.9/24"}, []string{"10.0.0.0/26", "10.0.0.128/25"}},
		{"disjoint", "10.0.0.0/24", []string{"10.0.1.0/24"}, []string{"10.0.1.0/24"}},
		{"other family", "fd00::/64", []string{"10.0.0.0/24"}, []string{"10.0.0.0/24"}},
		{"IPv6 partial", "fd00::2/127", []string{"fd00::/126"}, []string{"fd00::/127"}},
		{"mapped IPv4", "::ffff:10.0.0.0/120", []string{"10.0.0.0/24"}, nil},
		{"invalid route", "10.0.0.0/24", []string{"invalid"}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			local, err := parseRoutingPrefix(tt.local)
			if err != nil {
				t.Fatal(err)
			}

			got := excludeLocalCIDRs(tt.cidrs, []netip.Prefix{local})
			if !slices.Equal(got, tt.want) {
				t.Fatalf("routes=%v, want %v", got, tt.want)
			}
		})
	}
}

func TestExcludeLocalCIDRsPreservesEveryNonlocalAddress(t *testing.T) {
	locals := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.64/26"),
		netip.MustParsePrefix("10.0.0.80/28"),
		netip.MustParsePrefix("10.0.0.200/32"),
	}
	got := excludeLocalCIDRs([]string{"10.0.0.0/24", "10.0.0.0/24"}, locals)

	for addr := netip.MustParseAddr("10.0.0.0"); addr.IsValid() && netip.MustParsePrefix("10.0.0.0/24").Contains(addr); addr = addr.Next() {
		local := slices.ContainsFunc(locals, func(prefix netip.Prefix) bool { return prefix.Contains(addr) })
		matches := 0

		for _, cidr := range got {
			if netip.MustParsePrefix(cidr).Contains(addr) {
				matches++
			}
		}

		want := 1
		if local {
			want = 0
		}

		if matches != want {
			t.Fatalf("%s matches %d output routes, want %d", addr, matches, want)
		}
	}
}

func TestExcludeLocalCIDRsDefaultRoutes(t *testing.T) {
	for _, tt := range []struct {
		route, local string
		wantCount    int
	}{
		{"0.0.0.0/0", "10.0.0.1/32", 32},
		{"::/0", "fd00::1/128", 128},
	} {
		local := netip.MustParsePrefix(tt.local)

		got := excludeLocalCIDRs([]string{tt.route}, []netip.Prefix{local})
		if len(got) != tt.wantCount {
			t.Fatalf("%s excluding %s: %d prefixes, want %d", tt.route, tt.local, len(got), tt.wantCount)
		}

		for _, cidr := range got {
			if netip.MustParsePrefix(cidr).Overlaps(local) {
				t.Fatalf("route %s still covers local prefix %s", cidr, local)
			}
		}
	}
}

func TestCollectSiteRoutingConfig(t *testing.T) {
	sites := map[string]*unboundedv1alpha3.Site{
		"local": {Spec: unboundedv1alpha3.SiteSpec{
			NodeCidrs:  []string{"10.1.0.0/16"},
			LocalCIDRs: []string{"192.168.1.9/24", "192.168.1.0/24"},
			PodCidrAssignments: []unboundednetv1alpha1.PodCidrAssignment{
				{CidrBlocks: []string{"10.244.0.0/16"}},
			},
		}},
		"remote": {Spec: unboundedv1alpha3.SiteSpec{
			NodeCidrs: []string{"10.2.0.0/16"},
			PodCidrAssignments: []unboundednetv1alpha1.PodCidrAssignment{
				{CidrBlocks: []string{"10.245.0.0/16"}},
			},
		}},
	}

	got, err := collectSiteRoutingConfig(sites, "local", true)
	if err != nil {
		t.Fatal(err)
	}

	want := siteRoutingConfig{
		localCIDRs:          []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")},
		gatewaySupernets:    []string{"10.2.0.0/16", "10.244.0.0/16", "10.245.0.0/16"},
		gatewayNotrackCIDRs: []string{"10.1.0.0/16", "10.2.0.0/16", "10.244.0.0/16", "10.245.0.0/16"},
		gatewayReturnCIDRs:  []string{"10.1.0.0/16", "192.168.1.0/24"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gateway routing config=%+v, want %+v", got, want)
	}

	got, err = collectSiteRoutingConfig(sites, "local", false)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(got, siteRoutingConfig{localCIDRs: want.localCIDRs}) {
		t.Fatalf("ordinary node inherited gateway-only routing: %+v", got)
	}

	sites["local"].Spec.LocalCIDRs = []string{"invalid"}
	if _, err := collectSiteRoutingConfig(sites, "local", true); err == nil {
		t.Fatal("invalid local CIDR was accepted")
	}
}

func TestExcludeLocalGatewayRoutesPreservesMetadata(t *testing.T) {
	peer := gatewayPeerInfo{
		Name:           "gateway",
		PodCIDRs:       []string{"10.244.1.0/24"},
		RoutedCidrs:    []string{"10.0.0.0/24", "10.1.0.0/24"},
		RouteDistances: map[string]int{"10.0.0.0/24": 3, "10.1.0.0/24": 2},
		LearnedRoutes: map[string]unboundednetv1alpha1.GatewayNodeRoute{
			"10.0.0.0/24": {Type: "NodeCidr", Paths: [][]unboundednetv1alpha1.GatewayNodePathHop{{{Type: "Site", Name: "remote"}}}},
		},
	}

	got := excludeLocalGatewayRoutes(peer, []netip.Prefix{netip.MustParsePrefix("10.0.0.128/25")})
	if !slices.Equal(got.RoutedCidrs, []string{"10.0.0.0/25", "10.1.0.0/24"}) {
		t.Fatalf("filtered routes=%v", got.RoutedCidrs)
	}

	if got.RouteDistances["10.0.0.0/25"] != 3 || got.RouteDistances["10.1.0.0/24"] != 2 {
		t.Fatalf("route distances were lost: %v", got.RouteDistances)
	}

	if !reflect.DeepEqual(got.LearnedRoutes["10.0.0.0/25"], peer.LearnedRoutes["10.0.0.0/24"]) {
		t.Fatal("split advertisement lost its type or path")
	}

	if _, exists := got.LearnedRoutes["10.0.0.0/24"]; exists {
		t.Fatal("unfiltered advertisement survived")
	}

	if !slices.Equal(got.PodCIDRs, peer.PodCIDRs) || peer.RoutedCidrs[0] != "10.0.0.0/24" {
		t.Fatal("filter changed gateway identity or its input")
	}
}

func TestExcludeLocalSupernetsPreservesDirectMesh(t *testing.T) {
	original := map[string]bool{"10.244.0.0/16": true, "192.168.0.0/16": true}
	local := []netip.Prefix{netip.MustParsePrefix("192.168.0.0/17")}
	peers := []meshPeerInfo{{PodCIDRs: []string{"192.168.1.0/24"}}}
	got := excludeLocalSupernets(original, peers, local)

	want := map[string]bool{"10.244.0.0/16": true, "192.168.128.0/17": true, "192.168.1.0/24": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("routes=%v, want %v", got, want)
	}

	if !original["192.168.0.0/16"] || len(original) != 2 {
		t.Fatal("filter changed the input route set")
	}
}

func TestGatewayBPFPrefixesExcludeEveryLocalSource(t *testing.T) {
	peer := gatewayPeerInfo{
		PodCIDRs:    []string{"10.0.0.0/24"},
		RoutedCidrs: []string{"10.0.0.0/16", "fd00::/64"},
		InternalIPs: []string{"10.0.1.10", "10.2.0.10"},
	}
	local := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/17"), netip.MustParsePrefix("fd00::/65")}
	got := gatewayBPFPrefixes(peer, local)

	want := []string{"10.0.128.0/17", "10.2.0.10/32", "fd00::8000:0:0:0/65"}
	if !slices.Equal(got, want) {
		t.Fatalf("gateway BPF prefixes=%v, want %v", got, want)
	}

	if !slices.Equal(peer.PodCIDRs, []string{"10.0.0.0/24"}) || len(peer.RoutedCidrs) != 2 {
		t.Fatal("filter changed peer metadata")
	}
}
