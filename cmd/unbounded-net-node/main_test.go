// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

func gatewayNodeToUnstructured(t *testing.T, node *unboundednetv1alpha1.GatewayPoolNode) *unstructured.Unstructured {
	t.Helper()

	data, err := json.Marshal(node)
	if err != nil {
		t.Fatalf("failed to marshal GatewayNode: %v", err)
	}

	obj := make(map[string]interface{})
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatalf("failed to unmarshal GatewayNode into map: %v", err)
	}

	return &unstructured.Unstructured{Object: obj}
}

// TestMergeGatewayNodeAdvertisedRoutes_MergesEqualLengthMiddleHops tests MergeGatewayNodeAdvertisedRoutes_MergesEqualLengthMiddleHops.
func TestMergeGatewayNodeAdvertisedRoutes_MergesEqualLengthMiddleHops(t *testing.T) {
	cidr := "100.65.0.0/16"
	localPool := "site1-gwpool"

	peerA := gatewayPeerInfo{
		LearnedRoutes: map[string]unboundednetv1alpha1.GatewayNodeRoute{
			cidr: {
				Type: "RoutedCidr",
				Paths: [][]unboundednetv1alpha1.GatewayNodePathHop{
					{{Type: "Site", Name: "site2"}, {Type: "GatewayPool", Name: "site2extgw1"}},
				},
			},
		},
	}
	peerB := gatewayPeerInfo{
		LearnedRoutes: map[string]unboundednetv1alpha1.GatewayNodeRoute{
			cidr: {
				Type: "RoutedCidr",
				Paths: [][]unboundednetv1alpha1.GatewayNodePathHop{
					{{Type: "Site", Name: "site2"}, {Type: "GatewayPool", Name: "site2intgw1"}},
				},
			},
		},
	}

	merged := mergeGatewayNodeAdvertisedRoutes(nil, []gatewayPeerInfo{peerA, peerB}, localPool)

	route, ok := merged[cidr]
	if !ok {
		t.Fatalf("expected merged route for %s", cidr)
	}

	if len(route.Paths) != 2 {
		t.Fatalf("expected two full paths, got %d: %#v", len(route.Paths), route.Paths)
	}

	for i, path := range route.Paths {
		if len(path) != 3 {
			t.Fatalf("expected full path[%d] length 3, got %d: %#v", i, len(path), path)
		}

		if path[0].Type != "Site" || path[0].Name != "site2" {
			t.Fatalf("unexpected path[%d] site hop: %#v", i, path)
		}

		if path[2].Type != "GatewayPool" || path[2].Name != localPool {
			t.Fatalf("expected local pool appended on path[%d], got %#v", i, path)
		}
	}
}

// TestMergeGatewayNodeAdvertisedRoutes_PreservesVariableLengthPaths tests MergeGatewayNodeAdvertisedRoutes_PreservesVariableLengthPaths.
func TestMergeGatewayNodeAdvertisedRoutes_PreservesVariableLengthPaths(t *testing.T) {
	cidr := "100.65.0.0/16"
	localPool := "site1-gwpool"

	short := gatewayPeerInfo{
		LearnedRoutes: map[string]unboundednetv1alpha1.GatewayNodeRoute{
			cidr: {
				Type: "RoutedCidr",
				Paths: [][]unboundednetv1alpha1.GatewayNodePathHop{
					{{Type: "Site", Name: "site2"}},
				},
			},
		},
	}
	long := gatewayPeerInfo{
		LearnedRoutes: map[string]unboundednetv1alpha1.GatewayNodeRoute{
			cidr: {
				Type: "RoutedCidr",
				Paths: [][]unboundednetv1alpha1.GatewayNodePathHop{
					{{Type: "Site", Name: "site2"}, {Type: "GatewayPool", Name: "site2intgw1"}},
				},
			},
		},
	}

	merged := mergeGatewayNodeAdvertisedRoutes(nil, []gatewayPeerInfo{long, short}, localPool)

	route := merged[cidr]
	if len(route.Paths) != 2 {
		t.Fatalf("expected both short and long full paths, got %d: %#v", len(route.Paths), route.Paths)
	}

	if got := gatewayNodeRoutePathLength(route); got != 2 {
		t.Fatalf("expected shortest path length 2 after local hop append, got %d", got)
	}
}

// TestAppendGatewayPoolHop_DropsPathContainingOwnPool verifies that paths
// where the local gateway pool already appears are dropped entirely to
// prevent advertising loop routes (e.g. B->C->A when our pool is C).
func TestAppendGatewayPoolHop_DropsPathContainingOwnPool(t *testing.T) {
	route := unboundednetv1alpha1.GatewayNodeRoute{
		Type: "RoutedCidr",
		Paths: [][]unboundednetv1alpha1.GatewayNodePathHop{
			{{Type: "Site", Name: "site2"}, {Type: "GatewayPool", Name: "site1-gwpool"}},
		},
	}

	updated := appendGatewayPoolHop(route, "site1-gwpool")
	if len(updated.Paths) != 0 {
		t.Fatalf("expected loop path to be dropped, got %d paths: %#v", len(updated.Paths), updated.Paths)
	}
}

// TestAppendGatewayPoolHop_KeepsCleanPathsDropsLoops verifies that only
// loop paths are dropped while clean paths are preserved with the local
// pool appended.
func TestAppendGatewayPoolHop_KeepsCleanPathsDropsLoops(t *testing.T) {
	route := unboundednetv1alpha1.GatewayNodeRoute{
		Type: "NodeCidr",
		Paths: [][]unboundednetv1alpha1.GatewayNodePathHop{
			// Clean path -- pool C not present, should be kept with C appended
			{{Type: "Site", Name: "s4"}, {Type: "GatewayPool", Name: "s4extgw1"}},
			// Loop path -- pool C (s5intgw1) is in the middle, should be dropped
			{{Type: "Site", Name: "s4"}, {Type: "GatewayPool", Name: "s4extgw1"}, {Type: "GatewayPool", Name: "s5intgw1"}, {Type: "GatewayPool", Name: "s3intgw1"}},
		},
	}

	updated := appendGatewayPoolHop(route, "s5intgw1")
	if len(updated.Paths) != 1 {
		t.Fatalf("expected 1 clean path (loop dropped), got %d: %#v", len(updated.Paths), updated.Paths)
	}

	path := updated.Paths[0]
	if len(path) != 3 {
		t.Fatalf("expected path length 3 (site + remote pool + local pool), got %d: %#v", len(path), path)
	}

	if path[2].Type != "GatewayPool" || path[2].Name != "s5intgw1" {
		t.Fatalf("expected local pool appended, got %#v", path[2])
	}
}

// TestRoutedCIDRsForGatewayPeer_FiltersInvalidPathsKeepsValidAlternatives tests RoutedCIDRsForGatewayPeer_FiltersInvalidPathsKeepsValidAlternatives.
func TestRoutedCIDRsForGatewayPeer_FiltersInvalidPathsKeepsValidAlternatives(t *testing.T) {
	now := time.Now().UTC()
	mySite := "site1"
	localPools := []string{"pool-local"}

	validCIDR := "100.65.0.0/16"
	invalidCIDR := "100.66.0.0/16"

	gatewayNode := &unboundednetv1alpha1.GatewayPoolNode{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-a"},
		Status: unboundednetv1alpha1.GatewayNodeStatus{
			LastUpdated: metav1.NewTime(now),
			Routes: map[string]unboundednetv1alpha1.GatewayNodeRoute{
				validCIDR: {
					Type: "RoutedCidr",
					Paths: [][]unboundednetv1alpha1.GatewayNodePathHop{
						{{Type: "Site", Name: mySite}, {Type: "GatewayPool", Name: "pool-remote-a"}},
						{{Type: "Site", Name: "site2"}, {Type: "GatewayPool", Name: "pool-local"}},
						{{Type: "Site", Name: "site2"}, {Type: "GatewayPool", Name: "pool-remote-b"}},
					},
				},
				invalidCIDR: {
					Type: "RoutedCidr",
					Paths: [][]unboundednetv1alpha1.GatewayNodePathHop{
						{{Type: "Site", Name: mySite}, {Type: "GatewayPool", Name: "pool-local"}},
					},
				},
			},
		},
	}

	routes, distances, selected := routedCIDRsForGatewayPeer(
		gatewayNode,
		[]string{"10.0.0.0/8"},
		mySite,
		localPools,
		nil,
		now,
		gatewayNodeHeartbeatInterval*4,
	)

	if len(routes) != 1 || routes[0] != validCIDR {
		t.Fatalf("expected only valid CIDR to remain, got %#v", routes)
	}

	if got := distances[validCIDR]; got != 1 {
		t.Fatalf("expected default route distance 1 for shortest remaining path, got %d", got)
	}

	route, ok := selected[validCIDR]
	if !ok {
		t.Fatalf("expected selected route for %s", validCIDR)
	}

	if len(route.Paths) != 2 {
		t.Fatalf("expected two remote valid paths to survive filtering, got %#v", route.Paths)
	}

	hasLocalPoolRemoteSitePath := false
	hasRemotePoolPath := false

	for _, path := range route.Paths {
		if len(path) != 2 || path[0].Type != "Site" || path[0].Name != "site2" || path[1].Type != "GatewayPool" {
			continue
		}

		switch path[1].Name {
		case "pool-local":
			hasLocalPoolRemoteSitePath = true
		case "pool-remote-b":
			hasRemotePoolPath = true
		}
	}

	if !hasLocalPoolRemoteSitePath || !hasRemotePoolPath {
		t.Fatalf("expected surviving paths via remote site to include both local and remote pool hops, got %#v", route.Paths)
	}

	if _, exists := selected[invalidCIDR]; exists {
		t.Fatalf("did not expect fully-looping CIDR %s to be selected", invalidCIDR)
	}
}

// TestRoutedCIDRsForGatewayPeer_ExcludesNodeCIDRsForLocalAndPeeredSites tests RoutedCIDRsForGatewayPeer_ExcludesNodeCIDRsForLocalAndPeeredSites.
func TestRoutedCIDRsForGatewayPeer_ExcludesNodeCIDRsForLocalAndPeeredSites(t *testing.T) {
	now := time.Now().UTC()
	gatewayNode := &unboundednetv1alpha1.GatewayPoolNode{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-a"},
		Status: unboundednetv1alpha1.GatewayNodeStatus{
			LastUpdated: metav1.NewTime(now),
			Routes: map[string]unboundednetv1alpha1.GatewayNodeRoute{
				"100.65.0.0/16": {
					Type: "NodeCidr",
					Paths: [][]unboundednetv1alpha1.GatewayNodePathHop{
						{{Type: "Site", Name: "site1"}, {Type: "GatewayPool", Name: "pool-a"}},
						{{Type: "Site", Name: "site2"}, {Type: "GatewayPool", Name: "pool-a"}},
						{{Type: "Site", Name: "site3"}, {Type: "GatewayPool", Name: "pool-a"}},
					},
				},
				"100.125.0.0/16": {
					Type: "RoutedCidr",
					Paths: [][]unboundednetv1alpha1.GatewayNodePathHop{
						{{Type: "Site", Name: "site2"}, {Type: "GatewayPool", Name: "pool-a"}},
					},
				},
				"100.126.0.0/16": {
					Type: "RoutedCidr",
					Paths: [][]unboundednetv1alpha1.GatewayNodePathHop{
						{{Type: "Site", Name: "site3"}, {Type: "GatewayPool", Name: "pool-a"}},
					},
				},
			},
		},
	}

	routes, distances, selected := routedCIDRsForGatewayPeer(
		gatewayNode,
		nil,
		"site1",
		nil,
		map[string]bool{"site1": true, "site2": true},
		now,
		gatewayNodeHeartbeatInterval*4,
	)

	if len(routes) != 2 {
		t.Fatalf("expected NodeCidr and RoutedCidr to remain only for non-peered site paths, got %#v", routes)
	}

	nodeRoute, ok := selected["100.65.0.0/16"]
	if !ok {
		t.Fatalf("expected nodeCidr route to remain with non-peered site path")
	}

	if len(nodeRoute.Paths) != 1 || len(nodeRoute.Paths[0]) == 0 || nodeRoute.Paths[0][0].Name != "site3" {
		t.Fatalf("expected nodeCidr route paths to keep only non-peered site, got %#v", nodeRoute.Paths)
	}

	if distances["100.65.0.0/16"] != 1 {
		t.Fatalf("expected recomputed nodeCidr distance of 1, got %d", distances["100.65.0.0/16"])
	}

	if _, okSelected := selected["100.125.0.0/16"]; okSelected {
		t.Fatalf("did not expect routedCidr from peered site to remain")
	}

	routedRoute, ok := selected["100.126.0.0/16"]
	if !ok {
		t.Fatalf("expected routedCidr route to remain for non-peered site path")
	}

	if len(routedRoute.Paths) != 1 || len(routedRoute.Paths[0]) == 0 || routedRoute.Paths[0][0].Name != "site3" {
		t.Fatalf("expected routedCidr path to keep non-peered site only, got %#v", routedRoute.Paths)
	}
}

// TestRoutedCIDRsForGatewayPeer_DropsNodeCIDRsForNetworkPeeredSite tests RoutedCIDRsForGatewayPeer_DropsNodeCIDRsForNetworkPeeredSite.
func TestRoutedCIDRsForGatewayPeer_DropsNodeCIDRsForNetworkPeeredSite(t *testing.T) {
	now := time.Now().UTC()
	gatewayNode := &unboundednetv1alpha1.GatewayPoolNode{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-a"},
		Status: unboundednetv1alpha1.GatewayNodeStatus{
			LastUpdated: metav1.NewTime(now),
			Routes: map[string]unboundednetv1alpha1.GatewayNodeRoute{
				"100.66.0.0/16": {
					Type: "NodeCidr",
					Paths: [][]unboundednetv1alpha1.GatewayNodePathHop{
						{{Type: "Site", Name: "site2"}, {Type: "GatewayPool", Name: "pool-a"}},
					},
				},
				"100.127.0.0/16": {
					Type: "RoutedCidr",
					Paths: [][]unboundednetv1alpha1.GatewayNodePathHop{
						{{Type: "Site", Name: "site2"}, {Type: "GatewayPool", Name: "pool-a"}},
					},
				},
			},
		},
	}

	routes, _, selected := routedCIDRsForGatewayPeer(
		gatewayNode,
		nil,
		"site1",
		nil,
		map[string]bool{"site1": true, "site2": true},
		now,
		gatewayNodeHeartbeatInterval*4,
	)

	if len(routes) != 0 {
		t.Fatalf("expected no routes to remain after excluding network-peered nodeCidr and routedCidr, got %#v", routes)
	}

	if _, exists := selected["100.66.0.0/16"]; exists {
		t.Fatalf("did not expect nodeCidr from network-peered site to be selected")
	}

	if _, exists := selected["100.127.0.0/16"]; exists {
		t.Fatalf("did not expect routedCidr from network-peered site to be selected")
	}
}

// TestShouldReconcileGatewayNodeUpdate_HeartbeatOnlyChange tests ShouldReconcileGatewayNodeUpdate_HeartbeatOnlyChange.
func TestShouldReconcileGatewayNodeUpdate_HeartbeatOnlyChange(t *testing.T) {
	now := time.Now().UTC()
	oldNode := &unboundednetv1alpha1.GatewayPoolNode{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-a"},
		Spec: unboundednetv1alpha1.GatewayNodeSpec{
			NodeName:    "node-a",
			GatewayPool: "pool-a",
			Site:        "site-a",
		},
		Status: unboundednetv1alpha1.GatewayNodeStatus{
			LastUpdated: metav1.NewTime(now),
			Routes: map[string]unboundednetv1alpha1.GatewayNodeRoute{
				"100.64.0.0/16": {
					Type: "RoutedCidr",
					Paths: [][]unboundednetv1alpha1.GatewayNodePathHop{
						{{Type: "Site", Name: "site-a"}, {Type: "GatewayPool", Name: "pool-a"}},
					},
				},
			},
		},
	}
	newNode := oldNode.DeepCopy()
	newNode.Status.LastUpdated = metav1.NewTime(now.Add(10 * time.Second))

	if shouldReconcileGatewayNodeUpdate(gatewayNodeToUnstructured(t, oldNode), gatewayNodeToUnstructured(t, newNode)) {
		t.Fatalf("expected heartbeat-only GatewayNode update to be ignored")
	}
}

// TestShouldReconcileGatewayNodeUpdate_RoutesChange tests ShouldReconcileGatewayNodeUpdate_RoutesChange.
func TestShouldReconcileGatewayNodeUpdate_RoutesChange(t *testing.T) {
	now := time.Now().UTC()
	oldNode := &unboundednetv1alpha1.GatewayPoolNode{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-a"},
		Spec: unboundednetv1alpha1.GatewayNodeSpec{
			NodeName:    "node-a",
			GatewayPool: "pool-a",
			Site:        "site-a",
		},
		Status: unboundednetv1alpha1.GatewayNodeStatus{
			LastUpdated: metav1.NewTime(now),
			Routes: map[string]unboundednetv1alpha1.GatewayNodeRoute{
				"100.64.0.0/16": {
					Type: "RoutedCidr",
					Paths: [][]unboundednetv1alpha1.GatewayNodePathHop{
						{{Type: "Site", Name: "site-a"}, {Type: "GatewayPool", Name: "pool-a"}},
					},
				},
			},
		},
	}
	newNode := oldNode.DeepCopy()
	newNode.Status.Routes["100.65.0.0/16"] = unboundednetv1alpha1.GatewayNodeRoute{
		Type: "RoutedCidr",
		Paths: [][]unboundednetv1alpha1.GatewayNodePathHop{
			{{Type: "Site", Name: "site-b"}, {Type: "GatewayPool", Name: "pool-b"}},
		},
	}

	if !shouldReconcileGatewayNodeUpdate(gatewayNodeToUnstructured(t, oldNode), gatewayNodeToUnstructured(t, newNode)) {
		t.Fatalf("expected reconcile when GatewayNode routes change")
	}
}

// TestShouldIncludeSliceForGatewayNode tests ShouldIncludeSliceForGatewayNode.
func TestShouldIncludeSliceForGatewayNode(t *testing.T) {
	t.Run("local site requires assignment", func(t *testing.T) {
		connectedSites := map[string]bool{}
		assignedSites := map[string]bool{"site1": true}

		if !shouldIncludeSliceForGatewayNode("site1", "site1", connectedSites, assignedSites, false) {
			t.Fatalf("expected local site to be included")
		}

		if shouldIncludeSliceForGatewayNode("site1", "site1", connectedSites, map[string]bool{}, false) {
			t.Fatalf("did not expect local site without assignment to be included")
		}
	})

	t.Run("internal pool excludes assigned non-connected remote site", func(t *testing.T) {
		connectedSites := map[string]bool{}
		assignedSites := map[string]bool{"site-remote": true}

		if shouldIncludeSliceForGatewayNode("site-remote", "site-local", connectedSites, assignedSites, false) {
			t.Fatalf("did not expect non-connected remote site to be included")
		}
	})

	t.Run("external pool includes assigned non-connected remote site", func(t *testing.T) {
		connectedSites := map[string]bool{}
		assignedSites := map[string]bool{"site-remote": true}

		if !shouldIncludeSliceForGatewayNode("site-remote", "site-local", connectedSites, assignedSites, true) {
			t.Fatalf("expected assigned non-connected remote site to be included for external pool")
		}
	})

	t.Run("connected remote site with assignment included", func(t *testing.T) {
		connectedSites := map[string]bool{"site2": true}
		assignedSites := map[string]bool{"site2": true}

		if shouldIncludeSliceForGatewayNode("site3", "site-local", connectedSites, assignedSites, false) {
			t.Fatalf("did not expect gateway to include non-connected remote site")
		}

		if !shouldIncludeSliceForGatewayNode("site2", "site-local", connectedSites, assignedSites, false) {
			t.Fatalf("expected gateway to include directly connected remote site")
		}

		if shouldIncludeSliceForGatewayNode("site2", "site-local", connectedSites, map[string]bool{}, false) {
			t.Fatalf("did not expect gateway to include connected site without assignment")
		}
	})

	t.Run("transitive-only site excluded", func(t *testing.T) {
		connectedSites := map[string]bool{"site2": true}
		assignedSites := map[string]bool{"site1": true}

		if shouldIncludeSliceForGatewayNode("site1", "site-local", connectedSites, assignedSites, false) {
			t.Fatalf("did not expect gateway to include transitively reachable but non-connected site")
		}
	})
}

// TestBuildGatewayNodeRoutesForAssignedSitesStatus_IncludesRemoteAssignedSiteRoutes tests BuildGatewayNodeRoutesForAssignedSitesStatus_IncludesRemoteAssignedSiteRoutes.
func TestBuildGatewayNodeRoutesForAssignedSitesStatus_IncludesRemoteAssignedSiteRoutes(t *testing.T) {
	poolName := "mx-gateway"
	pool := &unboundednetv1alpha1.GatewayPool{
		ObjectMeta: metav1.ObjectMeta{Name: poolName},
		Spec: unboundednetv1alpha1.GatewayPoolSpec{
			RoutedCidrs: []string{"100.200.0.0/16"},
		},
	}

	assignments := []unboundednetv1alpha1.SiteGatewayPoolAssignment{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "assign-remote"},
			Spec: unboundednetv1alpha1.SiteGatewayPoolAssignmentSpec{
				Sites:        []string{"pal3-dc1"},
				GatewayPools: []string{poolName},
			},
		},
	}

	siteMap := map[string]*unboundednetv1alpha1.Site{
		"pal3-dc1": {
			ObjectMeta: metav1.ObjectMeta{Name: "pal3-dc1"},
			Spec: unboundednetv1alpha1.SiteSpec{
				NodeCidrs: []string{"100.65.0.0/16"},
				PodCidrAssignments: []unboundednetv1alpha1.PodCidrAssignment{
					{CidrBlocks: []string{"100.125.0.0/16"}},
				},
			},
		},
	}

	routes := buildGatewayNodeRoutesForAssignedSitesStatus(poolName, pool, assignments, siteMap)

	if _, ok := routes["100.200.0.0/16"]; !ok {
		t.Fatalf("expected pool routed CIDR to be present, got %#v", routes)
	}

	if route, ok := routes["100.65.0.0/16"]; !ok {
		t.Fatalf("expected remote assigned site node CIDR route to be present, got %#v", routes)
	} else if route.Type != "NodeCidr" {
		t.Fatalf("expected node CIDR route type, got %#v", route)
	}

	if route, ok := routes["100.125.0.0/16"]; !ok {
		t.Fatalf("expected remote assigned site pod CIDR route to be present, got %#v", routes)
	} else if route.Type != "PodCidr" {
		t.Fatalf("expected pod CIDR route type, got %#v", route)
	}
}

// TestBuildGatewayNodeRoutesForAssignedSitesStatus_SkipsDisabledAssignments tests disabled assignment handling.
func TestBuildGatewayNodeRoutesForAssignedSitesStatus_SkipsDisabledAssignments(t *testing.T) {
	poolName := "mx-gateway"
	disabled := false
	pool := &unboundednetv1alpha1.GatewayPool{
		ObjectMeta: metav1.ObjectMeta{Name: poolName},
		Spec: unboundednetv1alpha1.GatewayPoolSpec{
			RoutedCidrs: []string{"100.200.0.0/16"},
		},
	}

	assignments := []unboundednetv1alpha1.SiteGatewayPoolAssignment{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "assign-remote-disabled"},
			Spec: unboundednetv1alpha1.SiteGatewayPoolAssignmentSpec{
				Enabled:      &disabled,
				Sites:        []string{"pal3-dc1"},
				GatewayPools: []string{poolName},
			},
		},
	}

	siteMap := map[string]*unboundednetv1alpha1.Site{
		"pal3-dc1": {
			ObjectMeta: metav1.ObjectMeta{Name: "pal3-dc1"},
			Spec: unboundednetv1alpha1.SiteSpec{
				NodeCidrs: []string{"100.65.0.0/16"},
			},
		},
	}

	routes := buildGatewayNodeRoutesForAssignedSitesStatus(poolName, pool, assignments, siteMap)
	if _, ok := routes["100.65.0.0/16"]; ok {
		t.Fatalf("expected disabled assignment routes to be ignored, got %#v", routes)
	}
}
