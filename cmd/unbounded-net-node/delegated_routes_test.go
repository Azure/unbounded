// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

func TestUnmanagedLocalPoolsDoNotAttractThirdPartyTraffic(t *testing.T) {
	state := &wireGuardState{sitePodCIDRPools: []string{"10.244.0.0/16"}}
	for _, managed := range []bool{false, true} {
		supernets := collectSupernets(state, nil, nil, nil, nil, managed)
		if supernets["10.244.0.0/16"] != managed {
			t.Fatalf("local pool routing with management=%v: %v", managed, supernets)
		}
	}
}

func TestUnmanagedGatewayPoolsDoNotAttractThirdPartyTraffic(t *testing.T) {
	sites := map[string]*unboundedv1alpha3.Site{
		"local": {Spec: unboundedv1alpha3.SiteSpec{
			ManageCniPlugin: new(false),
			PodCidrAssignments: []unboundednetv1alpha1.PodCidrAssignment{{
				CidrBlocks: []string{"10.244.0.0/16", "fd00:0244::/48"},
			}},
		}},
		"remote": {Spec: unboundedv1alpha3.SiteSpec{
			NodeCidrs: []string{"192.0.2.0/24"},
			PodCidrAssignments: []unboundednetv1alpha1.PodCidrAssignment{{
				CidrBlocks: []string{"10.245.0.0/16"},
			}},
		}},
	}

	routing, err := collectSiteRoutingConfig(sites, "local", true)
	if err != nil {
		t.Fatal(err)
	}

	routing.gatewaySupernets = append(routing.gatewaySupernets, "fd00:244::/48")
	state := &wireGuardState{
		sitePodCIDRPools: []string{"10.244.0.0/16", "fd00:0244::/48"},
		sitePodCIDRs:     routing.gatewaySupernets,
	}

	gateways := []gatewayPeerInfo{{RoutedCidrs: routing.gatewaySupernets}}
	for _, managed := range []bool{false, true} {
		supernets := collectSupernets(state, nil, gateways, gateways, routing.gatewaySupernets, managed)
		for _, cidr := range []string{"10.244.0.0/16", "fd00:0244::/48", "fd00:244::/48"} {
			if supernets[cidr] != managed {
				t.Fatalf("gateway local pool routing with management=%v: %v", managed, supernets)
			}
		}

		for _, cidr := range []string{"10.245.0.0/16", "192.0.2.0/24"} {
			if !supernets[cidr] {
				t.Fatalf("gateway lost remote route %s with management=%v: %v", cidr, managed, supernets)
			}
		}
	}
}

func TestUnmanagedSupernetsHonorSkippedPeerPodCIDRs(t *testing.T) {
	state := &wireGuardState{sitePodCIDRPools: []string{"10.244.0.0/16"}}
	peers := []meshPeerInfo{
		{PodCIDRs: []string{"10.244.1.0/24"}, SkipPodCIDRRoutes: true},
		{PodCIDRs: []string{"10.245.1.0/24"}},
	}
	gateways := []gatewayPeerInfo{
		{PodCIDRs: []string{"10.244.2.0/24"}, SkipPodCIDRRoutes: true},
		{PodCIDRs: []string{"10.246.1.0/24"}},
	}

	supernets := collectSupernets(state, peers, gateways, gateways, nil, false)
	for _, cidr := range []string{"10.244.0.0/16", "10.244.1.0/24", "10.244.2.0/24"} {
		if supernets[cidr] {
			t.Fatalf("unmanaged local route %s was added: %v", cidr, supernets)
		}
	}

	for _, cidr := range []string{"10.245.1.0/24", "10.246.1.0/24"} {
		if !supernets[cidr] {
			t.Fatalf("non-skipped peer route %s was lost: %v", cidr, supernets)
		}
	}
}
