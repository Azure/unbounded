// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import "testing"

// TestBFDProfilePrecedence validates that BFD profile resolution follows
// the most-specific-association rule:
//
//   Gateway to gateway (same pool):      GatewayPool settings
//   Gateway to gateway (different pool):  GatewayPoolPeering settings
//   Node to gateway (associated pool):    SiteGatewayPoolAssignment settings
//   Node to node (different peered site): SitePeering settings
//   Node to node (same site):             Site settings

func TestMeshPeerHealthCheck_SameSite_UsesSiteProfile(t *testing.T) {
	peer := meshPeerInfo{SiteName: "s1"}
	siteBFD := map[string]string{"s1": "s-s1"}
	peeringBFD := map[string]string{}
	assignmentBFD := map[string]string{}

	got := resolveMeshPeerHealthCheckProfileName(false, peer, "s1", siteBFD, peeringBFD, assignmentBFD)
	if got != "s-s1" {
		t.Errorf("same-site peer (no overrides): want Site profile %q, got %q", "s-s1", got)
	}
}

func TestMeshPeerHealthCheck_SameSite_AssignmentOverridesSite(t *testing.T) {
	// Even for same-site peers, a more specific assignment profile wins.
	peer := meshPeerInfo{SiteName: "s1"}
	siteBFD := map[string]string{"s1": "s-s1"}
	peeringBFD := map[string]string{}
	assignmentBFD := map[string]string{"s1": "sgpa-s1-pool"}

	got := resolveMeshPeerHealthCheckProfileName(false, peer, "s1", siteBFD, peeringBFD, assignmentBFD)
	if got != "sgpa-s1-pool" {
		t.Errorf("same-site peer (assignment exists): want Assignment profile %q, got %q", "sgpa-s1-pool", got)
	}
}

func TestMeshPeerHealthCheck_SameSite_PeeringOverridesSite(t *testing.T) {
	// SitePeering overrides Site for same-site peers.
	peer := meshPeerInfo{SiteName: "s1"}
	siteBFD := map[string]string{"s1": "s-s1"}
	peeringBFD := map[string]string{"s1": "sp-s1-s1"}
	assignmentBFD := map[string]string{}

	got := resolveMeshPeerHealthCheckProfileName(false, peer, "s1", siteBFD, peeringBFD, assignmentBFD)
	if got != "sp-s1-s1" {
		t.Errorf("same-site peer (peering exists): want SitePeering profile %q, got %q", "sp-s1-s1", got)
	}
}

func TestMeshPeerHealthCheck_DifferentPeeredSite_AssignmentOverridesPeering(t *testing.T) {
	// SiteGatewayPoolAssignment is more specific than SitePeering.
	peer := meshPeerInfo{SiteName: "s2"}
	siteBFD := map[string]string{"s1": "s-s1", "s2": "s-s2"}
	peeringBFD := map[string]string{"s2": "sp-s1-s2"}
	assignmentBFD := map[string]string{"s2": "sgpa-s2-pool"}

	got := resolveMeshPeerHealthCheckProfileName(false, peer, "s1", siteBFD, peeringBFD, assignmentBFD)
	if got != "sgpa-s2-pool" {
		t.Errorf("peered-site (both exist): want Assignment profile %q, got %q", "sgpa-s2-pool", got)
	}
}

func TestMeshPeerHealthCheck_DifferentPeeredSite_PeeringOnly(t *testing.T) {
	// When no assignment exists, SitePeering is used.
	peer := meshPeerInfo{SiteName: "s2"}
	siteBFD := map[string]string{"s1": "s-s1", "s2": "s-s2"}
	peeringBFD := map[string]string{"s2": "sp-s1-s2"}
	assignmentBFD := map[string]string{}

	got := resolveMeshPeerHealthCheckProfileName(false, peer, "s1", siteBFD, peeringBFD, assignmentBFD)
	if got != "sp-s1-s2" {
		t.Errorf("peered-site (peering only): want SitePeering profile %q, got %q", "sp-s1-s2", got)
	}
}

func TestMeshPeerHealthCheck_GatewayNode_AssignmentBeatsPeering(t *testing.T) {
	peer := meshPeerInfo{SiteName: "s2"}
	siteBFD := map[string]string{"s1": "s-s1"}
	peeringBFD := map[string]string{"s2": "sp-s1-s2"}
	assignmentBFD := map[string]string{"s2": "sgpa-s2-extgw1"}

	got := resolveMeshPeerHealthCheckProfileName(true, peer, "s1", siteBFD, peeringBFD, assignmentBFD)
	if got != "sgpa-s2-extgw1" {
		t.Errorf("gateway mesh peer: want Assignment profile %q, got %q", "sgpa-s2-extgw1", got)
	}
}

func TestMeshPeerHealthCheck_GatewayNode_FallsBackToPeering(t *testing.T) {
	peer := meshPeerInfo{SiteName: "s2"}
	siteBFD := map[string]string{"s1": "s-s1"}
	peeringBFD := map[string]string{"s2": "sp-s1-s2"}
	assignmentBFD := map[string]string{}

	got := resolveMeshPeerHealthCheckProfileName(true, peer, "s1", siteBFD, peeringBFD, assignmentBFD)
	if got != "sp-s1-s2" {
		t.Errorf("gateway mesh peer (no assignment): want SitePeering fallback %q, got %q", "sp-s1-s2", got)
	}
}

func TestMeshPeerHealthCheck_DifferentSiteNoDirectPeering_UsesAssignmentProfile(t *testing.T) {
	peer := meshPeerInfo{SiteName: "s3"}
	siteBFD := map[string]string{"s1": "s-s1"}
	peeringBFD := map[string]string{}
	assignmentBFD := map[string]string{"s3": "sgpa-s3-pool"}

	got := resolveMeshPeerHealthCheckProfileName(false, peer, "s1", siteBFD, peeringBFD, assignmentBFD)
	if got != "sgpa-s3-pool" {
		t.Errorf("non-peered-site peer: want Assignment profile %q, got %q", "sgpa-s3-pool", got)
	}
}

func TestMeshPeerHealthCheck_ExplicitPeerOverride_WinsOverAll(t *testing.T) {
	peer := meshPeerInfo{SiteName: "s1", HealthCheckProfileName: "explicit-override"}
	siteBFD := map[string]string{"s1": "s-s1"}
	peeringBFD := map[string]string{"s1": "sp-s1-s2"}
	assignmentBFD := map[string]string{"s1": "sgpa-s1-pool"}

	got := resolveMeshPeerHealthCheckProfileName(false, peer, "s1", siteBFD, peeringBFD, assignmentBFD)
	if got != "explicit-override" {
		t.Errorf("explicit override: want %q, got %q", "explicit-override", got)
	}
}

func TestMeshPeerHealthCheck_SamePoolGateway_UsesGatewayPoolProfile(t *testing.T) {
	// Same-pool gateway peers get HealthCheckProfileName set from GatewayPool at
	// construction time (site_watch_reconcile.go:1322). This exercises the
	// explicit override path and verifies it beats all other sources.
	peer := meshPeerInfo{
		SiteName:               "s1",
		HealthCheckProfileName: "gp-extgw1", // set at construction from poolHealthCheckProfileNames
	}
	siteBFD := map[string]string{"s1": "s-s1"}
	peeringBFD := map[string]string{}
	assignmentBFD := map[string]string{"s1": "sgpa-s1-extgw1"}

	got := resolveMeshPeerHealthCheckProfileName(true, peer, "s1", siteBFD, peeringBFD, assignmentBFD)
	if got != "gp-extgw1" {
		t.Errorf("same-pool gateway peer: want GatewayPool profile %q, got %q", "gp-extgw1", got)
	}
}

func TestMeshPeerHealthCheck_CrossPoolGateway_UsesGatewayPoolPeeringProfile(t *testing.T) {
	// Cross-pool gateway mesh peers get HealthCheckProfileName set from
	// GatewayPoolPeering at construction time (site_watch_reconcile.go:1216).
	peer := meshPeerInfo{
		SiteName:               "s2",
		HealthCheckProfileName: "gpp-extgw1-intgw1", // set at construction from poolPeeringHealthCheckProfileNames
	}
	siteBFD := map[string]string{"s2": "s-s2"}
	peeringBFD := map[string]string{"s2": "sp-s1-s2"}
	assignmentBFD := map[string]string{"s2": "sgpa-s2-extgw1"}

	got := resolveMeshPeerHealthCheckProfileName(true, peer, "s1", siteBFD, peeringBFD, assignmentBFD)
	if got != "gpp-extgw1-intgw1" {
		t.Errorf("cross-pool gateway peer: want GatewayPoolPeering profile %q, got %q", "gpp-extgw1-intgw1", got)
	}
}

func TestGatewayPeerHealthCheck_SamePool_UsesGatewayPoolProfile(t *testing.T) {
	// Gateway node in pool "extgw1" talking to another gateway in the same pool.
	// The peer's HealthCheckProfileName is set from GatewayPool settings during peer collection.
	peer := gatewayPeerInfo{PoolName: "extgw1", SiteName: "s1", HealthCheckProfileName: "gp-extgw1"}
	assignmentBFD := map[string]string{"s1|extgw1": "sgpa-s1-extgw1"}
	poolBFD := map[string]string{"extgw1": "gp-extgw1"}

	got := resolveGatewayPeerHealthCheckProfileName(true, "s1", peer, assignmentBFD, poolBFD)
	if got != "gp-extgw1" {
		t.Errorf("same-pool gateway: want GatewayPool profile %q, got %q", "gp-extgw1", got)
	}
}

func TestGatewayPeerHealthCheck_DifferentPool_UsesGatewayPoolPeeringProfile(t *testing.T) {
	// Gateway node in pool "extgw1" talking to a gateway in pool "intgw1".
	// The peer's HealthCheckProfileName is set from GatewayPoolPeering during peer collection.
	peer := gatewayPeerInfo{PoolName: "intgw1", SiteName: "s2", HealthCheckProfileName: "gpp-extgw1-intgw1"}
	assignmentBFD := map[string]string{}
	poolBFD := map[string]string{"extgw1": "gp-extgw1"}

	got := resolveGatewayPeerHealthCheckProfileName(true, "s1", peer, assignmentBFD, poolBFD)
	if got != "gpp-extgw1-intgw1" {
		t.Errorf("cross-pool gateway: want GatewayPoolPeering profile %q, got %q", "gpp-extgw1-intgw1", got)
	}
}

func TestGatewayPeerHealthCheck_NodeToGateway_UsesAssignmentProfile(t *testing.T) {
	// Non-gateway node in s1 talking to a gateway in pool "extgw1".
	// The assignment s1-extgw1 defines the BFD profile.
	peer := gatewayPeerInfo{PoolName: "extgw1", SiteName: "s1"}
	assignmentBFD := map[string]string{"s1|extgw1": "sgpa-s1-extgw1", "s2|extgw1": "sgpa-s2-extgw1"}
	poolBFD := map[string]string{"extgw1": "gp-extgw1"}

	got := resolveGatewayPeerHealthCheckProfileName(false, "s1", peer, assignmentBFD, poolBFD)
	if got != "sgpa-s1-extgw1" {
		t.Errorf("node-to-gateway (s1): want Assignment profile %q, got %q", "sgpa-s1-extgw1", got)
	}

	// Same pool but from site s2 -- should use the s2 assignment profile.
	got = resolveGatewayPeerHealthCheckProfileName(false, "s2", peer, assignmentBFD, poolBFD)
	if got != "sgpa-s2-extgw1" {
		t.Errorf("node-to-gateway (s2): want Assignment profile %q, got %q", "sgpa-s2-extgw1", got)
	}
}

func TestGatewayPeerHealthCheck_GatewayNodeFallback_UsesPoolProfile(t *testing.T) {
	// Gateway node with no explicit peer HealthCheckProfileName and no assignment match.
	// Falls back to the pool-level profile.
	peer := gatewayPeerInfo{PoolName: "extgw1", SiteName: "s1"}
	assignmentBFD := map[string]string{}
	poolBFD := map[string]string{"extgw1": "gp-extgw1"}

	got := resolveGatewayPeerHealthCheckProfileName(true, "s1", peer, assignmentBFD, poolBFD)
	if got != "gp-extgw1" {
		t.Errorf("gateway fallback: want Pool profile %q, got %q", "gp-extgw1", got)
	}
}

func TestGatewayPeerHealthCheck_ExplicitPeerOverride_WinsOverAll(t *testing.T) {
	peer := gatewayPeerInfo{PoolName: "extgw1", SiteName: "s1", HealthCheckProfileName: "explicit"}
	assignmentBFD := map[string]string{"s1|extgw1": "sgpa-s1-extgw1"}
	poolBFD := map[string]string{"extgw1": "gp-extgw1"}

	got := resolveGatewayPeerHealthCheckProfileName(false, "s1", peer, assignmentBFD, poolBFD)
	if got != "explicit" {
		t.Errorf("explicit override (non-gw): want %q, got %q", "explicit", got)
	}

	got = resolveGatewayPeerHealthCheckProfileName(true, "s1", peer, assignmentBFD, poolBFD)
	if got != "explicit" {
		t.Errorf("explicit override (gw): want %q, got %q", "explicit", got)
	}
}

func TestGatewayPeerHealthCheck_PerSiteAssignmentKeys(t *testing.T) {
	// Verify that two different sites connecting to the same pool get
	// independent BFD profiles (the bug fixed in the site|pool keying).
	assignmentBFD := map[string]string{}
	assignmentSource := map[string]string{}

	a1 := newTestAssignment("s1-extgw1", []string{"s1"}, []string{"extgw1"})
	a2 := newTestAssignment("s2-extgw1", []string{"s2"}, []string{"extgw1"})

	bfdProfiles := map[string]string{}
	// Simulate the profile being added for each assignment
	assignmentBFD["s1|extgw1"] = "sgpa-s1-extgw1"
	assignmentSource["s1|extgw1"] = a1.Name
	assignmentBFD["s2|extgw1"] = "sgpa-s2-extgw1"
	assignmentSource["s2|extgw1"] = a2.Name

	_ = bfdProfiles // suppress unused

	// Non-gateway node in s1 should use s1's profile
	peer := gatewayPeerInfo{PoolName: "extgw1", SiteName: "s1"}

	got := resolveGatewayPeerHealthCheckProfileName(false, "s1", peer, assignmentBFD, nil)
	if got != "sgpa-s1-extgw1" {
		t.Errorf("per-site key (s1): want %q, got %q", "sgpa-s1-extgw1", got)
	}

	// Non-gateway node in s2 should use s2's profile
	got = resolveGatewayPeerHealthCheckProfileName(false, "s2", peer, assignmentBFD, nil)
	if got != "sgpa-s2-extgw1" {
		t.Errorf("per-site key (s2): want %q, got %q", "sgpa-s2-extgw1", got)
	}

	// Gateway node should use peer's site to select profile
	got = resolveGatewayPeerHealthCheckProfileName(true, "gw-site", peer, assignmentBFD, nil)
	if got != "sgpa-s1-extgw1" {
		t.Errorf("gateway per-site key (peer in s1): want %q, got %q", "sgpa-s1-extgw1", got)
	}

	peerS2 := gatewayPeerInfo{PoolName: "extgw1", SiteName: "s2"}

	got = resolveGatewayPeerHealthCheckProfileName(true, "gw-site", peerS2, assignmentBFD, nil)
	if got != "sgpa-s2-extgw1" {
		t.Errorf("gateway per-site key (peer in s2): want %q, got %q", "sgpa-s2-extgw1", got)
	}
}

func newTestAssignment(name string, sites, pools []string) unboundednetv1alpha1SiteGatewayPoolAssignmentLite {
	return unboundednetv1alpha1SiteGatewayPoolAssignmentLite{Name: name, Sites: sites, GatewayPools: pools}
}

// unboundednetv1alpha1SiteGatewayPoolAssignmentLite is a test-only minimal representation.
type unboundednetv1alpha1SiteGatewayPoolAssignmentLite struct {
	Name         string
	Sites        []string
	GatewayPools []string
}
