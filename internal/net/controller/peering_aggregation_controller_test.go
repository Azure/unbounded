// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"reflect"
	"testing"

	unboundednetv1alpha1 "github.com/Azure/unbounded-kube/internal/net/apis/unboundednet/v1alpha1"
)

// TestComputeReachable_DirectVsTransitiveSites tests compute reachable direct vs transitive sites.
func TestComputeReachable_DirectVsTransitiveSites(t *testing.T) {
	pools := map[string]*unboundednetv1alpha1.GatewayPool{
		"site2extgw1": {
			Spec: unboundednetv1alpha1.GatewayPoolSpec{
				RoutedCidrs: []string{"100.64.0.0/16"},
			},
		},
		"site1gwpool": {
			Spec: unboundednetv1alpha1.GatewayPoolSpec{
				RoutedCidrs: []string{"100.65.0.0/16"},
			},
		},
	}

	sites := map[string]*unboundednetv1alpha1.Site{
		"site2": {
			Spec: unboundednetv1alpha1.SiteSpec{
				NodeCidrs: []string{"10.2.0.0/16"},
			},
		},
		"site1": {
			Spec: unboundednetv1alpha1.SiteSpec{
				NodeCidrs: []string{"10.1.0.0/16"},
			},
		},
	}

	connectedSites := map[string]map[string]struct{}{
		"site2extgw1": {
			"site2": {},
		},
		"site1gwpool": {
			"site1": {},
		},
	}

	adjacency := map[string]map[string]struct{}{
		"site2extgw1": {
			"site1gwpool": {},
		},
		"site1gwpool": {
			"site2extgw1": {},
		},
	}

	reachable, routes := computeReachable("site2extgw1", pools, sites, connectedSites, adjacency)

	if len(reachable.connected) != 1 || reachable.connected[0] != "site2" {
		t.Fatalf("expected connected sites to include only direct site2, got %#v", reachable.connected)
	}

	if len(reachable.reachable) != 2 || reachable.reachable[0] != "site1" || reachable.reachable[1] != "site2" {
		t.Fatalf("expected reachable sites to include site1 and site2, got %#v", reachable.reachable)
	}

	foundSite1NodeCIDR := false

	for _, route := range routes {
		if route.CIDR == "10.1.0.0/16" && route.Type == routeTypeNodeCIDR {
			foundSite1NodeCIDR = true
			break
		}
	}

	if !foundSite1NodeCIDR {
		t.Fatalf("expected transitive site1 node CIDR route to be present in reachable routes")
	}
}

// TestComputeReachableRoutesAndRouteSelectionPriority tests compute reachable routes and route selection priority.
func TestComputeReachableRoutesAndRouteSelectionPriority(t *testing.T) {
	pools := map[string]*unboundednetv1alpha1.GatewayPool{
		"pool-a": {Spec: unboundednetv1alpha1.GatewayPoolSpec{RoutedCidrs: []string{"100.64.0.0/16"}}},
		"pool-b": {Spec: unboundednetv1alpha1.GatewayPoolSpec{RoutedCidrs: []string{"100.65.0.0/16"}}},
	}
	sites := map[string]*unboundednetv1alpha1.Site{
		"site-a": {
			Spec: unboundednetv1alpha1.SiteSpec{
				NodeCidrs:          []string{"10.1.0.0/16"},
				PodCidrAssignments: []unboundednetv1alpha1.PodCidrAssignment{{CidrBlocks: []string{"10.10.0.0/16"}}},
			},
		},
		"site-b": {
			Spec: unboundednetv1alpha1.SiteSpec{NodeCidrs: []string{"10.2.0.0/16"}},
		},
	}
	connectedSites := map[string]map[string]struct{}{
		"pool-a": {"site-a": {}},
		"pool-b": {"site-b": {}},
	}
	adjacency := map[string]map[string]struct{}{
		"pool-a": {"pool-b": {}},
		"pool-b": {"pool-a": {}},
	}

	routes := computeReachableRoutes("pool-a", pools, sites, connectedSites, adjacency)
	if len(routes) == 0 {
		t.Fatalf("expected non-empty routes from computeReachableRoutes")
	}

	// Check direct-vs-transitive weighting by looking up site-a/site-b node CIDRs.
	weights := map[string]int{}
	for _, route := range routes {
		weights[route.CIDR] = route.Weight
	}

	if weights["10.1.0.0/16"] == 0 || weights["10.2.0.0/16"] == 0 {
		t.Fatalf("expected both direct and transitive site node routes: %#v", weights)
	}

	if weights["10.1.0.0/16"] >= weights["10.2.0.0/16"] {
		t.Fatalf("expected direct site route to have lower weight than transitive route: %#v", weights)
	}

	set := map[string]struct{}{"b": {}, "a": {}}
	if got := setToSortedSlice(set); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("unexpected sorted set output: %#v", got)
	}

	if !equalStringSlices([]string{"a", "b"}, []string{"a", "b"}) || equalStringSlices([]string{"a"}, []string{"b"}) {
		t.Fatalf("unexpected equalStringSlices behavior")
	}
}
