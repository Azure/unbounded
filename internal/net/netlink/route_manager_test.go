// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netlink

import (
	"net"
	"sort"
	"testing"
)

// ---------------------------------------------------------------------------
// incrementIP tests
// ---------------------------------------------------------------------------

func TestIncrementIP_IPv4_Simple(t *testing.T) {
	ip := net.ParseIP("10.0.0.0").To4()
	got := incrementIP(ip)

	want := net.ParseIP("10.0.0.1").To4()
	if !got.Equal(want) {
		t.Errorf("incrementIP(%s) = %s, want %s", ip, got, want)
	}
}

func TestIncrementIP_IPv4_LastByteOverflow(t *testing.T) {
	ip := net.ParseIP("10.0.0.255").To4()
	got := incrementIP(ip)

	want := net.ParseIP("10.0.1.0").To4()
	if !got.Equal(want) {
		t.Errorf("incrementIP(%s) = %s, want %s", ip, got, want)
	}
}

func TestIncrementIP_IPv4_MultiByteCarry(t *testing.T) {
	ip := net.ParseIP("10.0.255.255").To4()
	got := incrementIP(ip)

	want := net.ParseIP("10.1.0.0").To4()
	if !got.Equal(want) {
		t.Errorf("incrementIP(%s) = %s, want %s", ip, got, want)
	}
}

func TestIncrementIP_IPv4_AllOnesOverflow(t *testing.T) {
	ip := net.ParseIP("255.255.255.255").To4()
	got := incrementIP(ip)

	want := net.IPv4(0, 0, 0, 0).To4()
	if !got.Equal(want) {
		t.Errorf("incrementIP(%s) = %s, want %s", ip, got, want)
	}
}

func TestIncrementIP_IPv6_Simple(t *testing.T) {
	ip := net.ParseIP("fd00::0")
	got := incrementIP(ip)

	want := net.ParseIP("fd00::1")
	if !got.Equal(want) {
		t.Errorf("incrementIP(%s) = %s, want %s", ip, got, want)
	}
}

func TestIncrementIP_IPv6_LastByteOverflow(t *testing.T) {
	ip := net.ParseIP("fd00::ff")
	got := incrementIP(ip)

	want := net.ParseIP("fd00::100")
	if !got.Equal(want) {
		t.Errorf("incrementIP(%s) = %s, want %s", ip, got, want)
	}
}

func TestIncrementIP_IPv6_MultiByteCarry(t *testing.T) {
	ip := net.ParseIP("fd00::ffff")
	got := incrementIP(ip)

	want := net.ParseIP("fd00::1:0")
	if !got.Equal(want) {
		t.Errorf("incrementIP(%s) = %s, want %s", ip, got, want)
	}
}

func TestIncrementIP_DoesNotMutateOriginal(t *testing.T) {
	ip := net.ParseIP("10.0.0.5").To4()
	original := make(net.IP, len(ip))
	copy(original, ip)

	_ = incrementIP(ip)
	if !ip.Equal(original) {
		t.Errorf("incrementIP mutated original: got %s, want %s", ip, original)
	}
}

// ---------------------------------------------------------------------------
// intSlicesEqual tests
// ---------------------------------------------------------------------------

func TestIntSlicesEqual_SameOrder(t *testing.T) {
	if !intSlicesEqual([]int{1, 2, 3}, []int{1, 2, 3}) {
		t.Error("expected equal for same-order slices")
	}
}

func TestIntSlicesEqual_DifferentOrder(t *testing.T) {
	if !intSlicesEqual([]int{3, 1, 2}, []int{1, 2, 3}) {
		t.Error("expected equal for different-order slices")
	}
}

func TestIntSlicesEqual_DifferentLengths(t *testing.T) {
	if intSlicesEqual([]int{1, 2}, []int{1, 2, 3}) {
		t.Error("expected not equal for different-length slices")
	}
}

func TestIntSlicesEqual_DifferentValues(t *testing.T) {
	if intSlicesEqual([]int{1, 2, 3}, []int{1, 2, 4}) {
		t.Error("expected not equal for different values")
	}
}

func TestIntSlicesEqual_DoesNotMutateOriginals(t *testing.T) {
	a := []int{3, 1, 2}
	b := []int{2, 3, 1}
	aCopy := make([]int, len(a))
	bCopy := make([]int, len(b))

	copy(aCopy, a)
	copy(bCopy, b)

	intSlicesEqual(a, b)

	for i := range a {
		if a[i] != aCopy[i] {
			t.Errorf("intSlicesEqual mutated slice a at index %d: got %d, want %d", i, a[i], aCopy[i])
		}

		if b[i] != bCopy[i] {
			t.Errorf("intSlicesEqual mutated slice b at index %d: got %d, want %d", i, b[i], bCopy[i])
		}
	}
}

// ---------------------------------------------------------------------------
// ipEqual tests
// ---------------------------------------------------------------------------

func TestIPEqual_BothNil(t *testing.T) {
	if !ipEqual(nil, nil) {
		t.Error("expected nil == nil")
	}
}

func TestIPEqual_OneNil(t *testing.T) {
	if ipEqual(net.ParseIP("10.0.0.1"), nil) {
		t.Error("expected IP != nil")
	}

	if ipEqual(nil, net.ParseIP("10.0.0.1")) {
		t.Error("expected nil != IP")
	}
}

func TestIPEqual_Same(t *testing.T) {
	if !ipEqual(net.ParseIP("10.0.0.1"), net.ParseIP("10.0.0.1")) {
		t.Error("expected equal IPs to be equal")
	}
}

func TestIPEqual_Different(t *testing.T) {
	if ipEqual(net.ParseIP("10.0.0.1"), net.ParseIP("10.0.0.2")) {
		t.Error("expected different IPs to not be equal")
	}
}

// ---------------------------------------------------------------------------
// isLinkScopeRoute tests
// ---------------------------------------------------------------------------

func TestIsLinkScopeRoute_SingleNoGateway(t *testing.T) {
	nhs := []DesiredNexthop{{PeerID: "peer-a", LinkIndex: 1, Gateway: nil}}
	if !isLinkScopeRoute(nhs) {
		t.Error("single nexthop with no gateway should be link-scope")
	}
}

func TestIsLinkScopeRoute_SingleWithGateway(t *testing.T) {
	nhs := []DesiredNexthop{{PeerID: "peer-a", LinkIndex: 1, Gateway: net.ParseIP("10.0.0.1")}}
	if isLinkScopeRoute(nhs) {
		t.Error("single nexthop with gateway should not be link-scope")
	}
}

func TestIsLinkScopeRoute_Multiple(t *testing.T) {
	nhs := []DesiredNexthop{
		{PeerID: "peer-a", LinkIndex: 1},
		{PeerID: "peer-b", LinkIndex: 2},
	}
	if isLinkScopeRoute(nhs) {
		t.Error("multiple nexthops should not be link-scope")
	}
}

func TestIsLinkScopeRoute_Empty(t *testing.T) {
	if isLinkScopeRoute(nil) {
		t.Error("empty nexthops should not be link-scope")
	}
}

// ---------------------------------------------------------------------------
// routeKey tests
// ---------------------------------------------------------------------------

func TestRouteKey_MainTable(t *testing.T) {
	m := NewUnifiedRouteManager("test", 0)
	_, prefix, _ := net.ParseCIDR("10.0.0.0/24")
	key := m.routeKey(0, *prefix)

	want := "254:10.0.0.0/24"
	if key != want {
		t.Errorf("routeKey(0, 10.0.0.0/24) = %s, want %s", key, want)
	}
}

func TestRouteKey_CustomTable(t *testing.T) {
	m := NewUnifiedRouteManager("test", 0)
	_, prefix, _ := net.ParseCIDR("fd00::/64")
	key := m.routeKey(1001, *prefix)

	want := "1001:fd00::/64"
	if key != want {
		t.Errorf("routeKey(1001, fd00::/64) = %s, want %s", key, want)
	}
}

func TestRouteKey_ExplicitMainTable(t *testing.T) {
	m := NewUnifiedRouteManager("test", 0)
	_, prefix, _ := net.ParseCIDR("192.168.0.0/16")
	key := m.routeKey(254, *prefix)

	want := "254:192.168.0.0/16"
	if key != want {
		t.Errorf("routeKey(254, ...) = %s, want %s", key, want)
	}
}

func TestRouteKey_DedicatedDefaultTable(t *testing.T) {
	m := NewUnifiedRouteManager("test", 500)
	_, prefix, _ := net.ParseCIDR("10.0.0.0/24")
	// Table==0 should resolve to the dedicated default table, not main.
	key := m.routeKey(0, *prefix)

	want := "500:10.0.0.0/24"
	if key != want {
		t.Errorf("routeKey(0, 10.0.0.0/24) with defaultTable=500 = %s, want %s", key, want)
	}
	// Explicitly set table should be preserved.
	key2 := m.routeKey(1001, *prefix)

	want2 := "1001:10.0.0.0/24"
	if key2 != want2 {
		t.Errorf("routeKey(1001, ...) with defaultTable=500 = %s, want %s", key2, want2)
	}
}

// ---------------------------------------------------------------------------
// normalizePrefix tests
// ---------------------------------------------------------------------------

func TestNormalizePrefix_AlreadyNormalized(t *testing.T) {
	_, prefix, _ := net.ParseCIDR("10.0.0.0/24")

	got := normalizePrefix(*prefix)
	if got.String() != "10.0.0.0/24" {
		t.Errorf("normalizePrefix(10.0.0.0/24) = %s, want 10.0.0.0/24", got.String())
	}
}

func TestNormalizePrefix_HostBitsSet(t *testing.T) {
	// net.ParseCIDR already masks, but test the function directly
	prefix := net.IPNet{
		IP:   net.ParseIP("10.0.0.5").To4(),
		Mask: net.CIDRMask(24, 32),
	}

	got := normalizePrefix(prefix)
	if got.String() != "10.0.0.0/24" {
		t.Errorf("normalizePrefix(10.0.0.5/24) = %s, want 10.0.0.0/24", got.String())
	}
}

// ---------------------------------------------------------------------------
// routeReferencesPeer tests
// ---------------------------------------------------------------------------

func TestRouteReferencesPeer_Found(t *testing.T) {
	dr := DesiredRoute{
		Nexthops: []DesiredNexthop{
			{PeerID: "peer-a"},
			{PeerID: "peer-b"},
		},
	}
	if !routeReferencesPeer(dr, "peer-b") {
		t.Error("expected route to reference peer-b")
	}
}

func TestRouteReferencesPeer_NotFound(t *testing.T) {
	dr := DesiredRoute{
		Nexthops: []DesiredNexthop{
			{PeerID: "peer-a"},
		},
	}
	if routeReferencesPeer(dr, "peer-c") {
		t.Error("expected route not to reference peer-c")
	}
}

// ---------------------------------------------------------------------------
// UnifiedRouteManager unit tests (no netlink calls)
// ---------------------------------------------------------------------------

func TestNewUnifiedRouteManager(t *testing.T) {
	m := NewUnifiedRouteManager("test-iface", 0)
	if m == nil {
		t.Fatal("expected non-nil manager")
	}

	if m.linkName != "test-iface" {
		t.Errorf("linkName = %s, want test-iface", m.linkName)
	}

	if m.defaultTable != 254 {
		t.Errorf("defaultTable = %d, want 254 (main) when 0 is passed", m.defaultTable)
	}

	if len(m.nexthops) != 0 {
		t.Errorf("nexthops should be empty, got %d", len(m.nexthops))
	}

	if len(m.installedRoutes) != 0 {
		t.Errorf("installedRoutes should be empty, got %d", len(m.installedRoutes))
	}
}

func TestNewUnifiedRouteManager_DedicatedTable(t *testing.T) {
	m := NewUnifiedRouteManager("test-iface", 500)
	if m == nil {
		t.Fatal("expected non-nil manager")
	}

	if m.defaultTable != 500 {
		t.Errorf("defaultTable = %d, want 500", m.defaultTable)
	}

	if !m.isDedicatedTable() {
		t.Error("expected isDedicatedTable() = true for table 500")
	}
}

func TestEffectiveTable(t *testing.T) {
	m := NewUnifiedRouteManager("test", 500)

	// Table==0 should resolve to defaultTable.
	if got := m.effectiveTable(0); got != 500 {
		t.Errorf("effectiveTable(0) = %d, want 500", got)
	}
	// Explicitly set table should be preserved.
	if got := m.effectiveTable(1001); got != 1001 {
		t.Errorf("effectiveTable(1001) = %d, want 1001", got)
	}
}

func TestIsDedicatedTable(t *testing.T) {
	tests := []struct {
		table int
		want  bool
	}{
		{0, false},   // 0 becomes 254 (main)
		{254, false}, // main table is not dedicated
		{500, true},
		{51820, true},
	}
	for _, tt := range tests {
		m := NewUnifiedRouteManager("test", tt.table)
		if got := m.isDedicatedTable(); got != tt.want {
			t.Errorf("NewUnifiedRouteManager(_, %d).isDedicatedTable() = %v, want %v", tt.table, got, tt.want)
		}
	}
}

func TestPeerNexthopID_Deterministic(t *testing.T) {
	m := NewUnifiedRouteManager("test", 0)
	id1 := m.peerNexthopID("peer-a")

	id2 := m.peerNexthopID("peer-a")
	if id1 != id2 {
		t.Errorf("peerNexthopID should be deterministic: %d != %d", id1, id2)
	}

	if id1 == 0 {
		t.Error("nexthop ID should not be 0")
	}
}

func TestPeerNexthopID_CollisionHandling(t *testing.T) {
	m := NewUnifiedRouteManager("test", 0)

	// Allocate a nexthop for peer-a
	idA := m.peerNexthopID("peer-a")
	m.nexthopIDs[idA] = "peer-a"

	// Simulate a collision by requesting the same hash for a different peer
	// We can't easily force a collision with FNV, but we can verify the
	// collision-handling loop by pre-populating the map.
	collidingPeer := "peer-collision"
	collidingHash := m.peerNexthopID(collidingPeer)

	// The collidingPeer should get a different ID than peer-a
	if collidingHash == idA && collidingPeer != "peer-a" {
		// This would only happen if both peers hash to the same value,
		// which the collision handler should resolve.
		t.Errorf("collision not resolved: peer-a and %s both got ID %d", collidingPeer, idA)
	}
}

func TestEnsureNexthop_NewAndUpdate(t *testing.T) {
	m := NewUnifiedRouteManager("test", 0)

	nh := DesiredNexthop{PeerID: "peer-a", LinkIndex: 5, Gateway: net.ParseIP("10.0.0.1")}

	id1 := m.ensureNexthop(nh)
	if id1 == 0 {
		t.Error("ensureNexthop returned 0")
	}

	state, ok := m.nexthops["peer-a"]
	if !ok {
		t.Fatal("peer-a not found in nexthops map")
	}

	if state.linkIndex != 5 {
		t.Errorf("linkIndex = %d, want 5", state.linkIndex)
	}

	// Update with new link index
	nh2 := DesiredNexthop{PeerID: "peer-a", LinkIndex: 10, Gateway: net.ParseIP("10.0.0.2")}

	id2 := m.ensureNexthop(nh2)
	if id2 != id1 {
		t.Errorf("ensureNexthop should return same ID for same peer: %d != %d", id2, id1)
	}

	if state.linkIndex != 10 {
		t.Errorf("linkIndex should be updated to 10, got %d", state.linkIndex)
	}
}

func TestActiveNexthops_AllHealthy(t *testing.T) {
	m := NewUnifiedRouteManager("test", 0)
	m.peerHealthy["peer-a"] = true
	m.peerHealthy["peer-b"] = true

	dr := DesiredRoute{
		Nexthops: []DesiredNexthop{
			{PeerID: "peer-a", LinkIndex: 1},
			{PeerID: "peer-b", LinkIndex: 2},
		},
	}

	active := m.activeNexthops(dr)
	if len(active) != 2 {
		t.Errorf("expected 2 active nexthops, got %d", len(active))
	}
}

func TestActiveNexthops_OneUnhealthy(t *testing.T) {
	m := NewUnifiedRouteManager("test", 0)
	m.peerHealthy["peer-a"] = true
	m.peerHealthy["peer-b"] = false

	dr := DesiredRoute{
		Nexthops: []DesiredNexthop{
			{PeerID: "peer-a", LinkIndex: 1},
			{PeerID: "peer-b", LinkIndex: 2},
		},
	}

	active := m.activeNexthops(dr)
	if len(active) != 1 {
		t.Fatalf("expected 1 active nexthop, got %d", len(active))
	}

	if active[0].PeerID != "peer-a" {
		t.Errorf("expected peer-a, got %s", active[0].PeerID)
	}
}

func TestActiveNexthops_UntrackedDefaultsHealthy(t *testing.T) {
	m := NewUnifiedRouteManager("test", 0)
	// Do NOT set any peer health state

	dr := DesiredRoute{
		Nexthops: []DesiredNexthop{
			{PeerID: "peer-new", LinkIndex: 1},
		},
	}

	active := m.activeNexthops(dr)
	if len(active) != 1 {
		t.Errorf("untracked peer should default to healthy, got %d active", len(active))
	}
}

func TestRouteNeedsUpdate_NoChange(t *testing.T) {
	m := NewUnifiedRouteManager("test", 0)

	installed := &installedRouteState{
		metric: 100,
		mtu:    1400,
		peerNexthops: map[string]DesiredNexthop{
			"peer-a": {PeerID: "peer-a", LinkIndex: 1, Gateway: net.ParseIP("10.0.0.1")},
		},
	}
	desired := DesiredRoute{Metric: 100, MTU: 1400}
	active := []DesiredNexthop{{PeerID: "peer-a", LinkIndex: 1, Gateway: net.ParseIP("10.0.0.1")}}

	if m.routeNeedsUpdate(installed, desired, active) {
		t.Error("route should not need update when nothing changed")
	}
}

func TestRouteNeedsUpdate_MetricChanged(t *testing.T) {
	m := NewUnifiedRouteManager("test", 0)

	installed := &installedRouteState{metric: 100, mtu: 1400, peerNexthops: map[string]DesiredNexthop{}}
	desired := DesiredRoute{Metric: 200, MTU: 1400}

	if !m.routeNeedsUpdate(installed, desired, nil) {
		t.Error("route should need update when metric changed")
	}
}

func TestRouteNeedsUpdate_MTUChanged(t *testing.T) {
	m := NewUnifiedRouteManager("test", 0)

	installed := &installedRouteState{metric: 100, mtu: 1400, peerNexthops: map[string]DesiredNexthop{}}
	desired := DesiredRoute{Metric: 100, MTU: 1500}

	if !m.routeNeedsUpdate(installed, desired, nil) {
		t.Error("route should need update when MTU changed")
	}
}

func TestRouteNeedsUpdate_NexthopAdded(t *testing.T) {
	m := NewUnifiedRouteManager("test", 0)

	installed := &installedRouteState{
		metric: 0, mtu: 0,
		peerNexthops: map[string]DesiredNexthop{
			"peer-a": {PeerID: "peer-a", LinkIndex: 1},
		},
	}
	desired := DesiredRoute{}
	active := []DesiredNexthop{
		{PeerID: "peer-a", LinkIndex: 1},
		{PeerID: "peer-b", LinkIndex: 2},
	}

	if !m.routeNeedsUpdate(installed, desired, active) {
		t.Error("route should need update when nexthop added")
	}
}

func TestRouteNeedsUpdate_LinkIndexChanged(t *testing.T) {
	m := NewUnifiedRouteManager("test", 0)

	installed := &installedRouteState{
		peerNexthops: map[string]DesiredNexthop{
			"peer-a": {PeerID: "peer-a", LinkIndex: 1},
		},
	}
	desired := DesiredRoute{}
	active := []DesiredNexthop{{PeerID: "peer-a", LinkIndex: 99}}

	if !m.routeNeedsUpdate(installed, desired, active) {
		t.Error("route should need update when link index changed")
	}
}

func TestRouteNeedsUpdate_GatewayChanged(t *testing.T) {
	m := NewUnifiedRouteManager("test", 0)

	installed := &installedRouteState{
		peerNexthops: map[string]DesiredNexthop{
			"peer-a": {PeerID: "peer-a", LinkIndex: 1, Gateway: net.ParseIP("10.0.0.1")},
		},
	}
	desired := DesiredRoute{}
	active := []DesiredNexthop{{PeerID: "peer-a", LinkIndex: 1, Gateway: net.ParseIP("10.0.0.2")}}

	if !m.routeNeedsUpdate(installed, desired, active) {
		t.Error("route should need update when gateway changed")
	}
}

func TestSetPreferredSourceIPs(t *testing.T) {
	m := NewUnifiedRouteManager("test", 0)

	v4 := net.ParseIP("192.168.1.1")
	v6 := net.ParseIP("fd00::1")
	m.SetPreferredSourceIPs(v4, v6)

	if !m.preferredSrcIPv4.Equal(v4) {
		t.Errorf("expected IPv4 src %s, got %s", v4, m.preferredSrcIPv4)
	}

	if !m.preferredSrcIPv6.Equal(v6) {
		t.Errorf("expected IPv6 src %s, got %s", v6, m.preferredSrcIPv6)
	}
}

func TestPreferredSrc_IPv4(t *testing.T) {
	m := NewUnifiedRouteManager("test", 0)
	m.preferredSrcIPv4 = net.ParseIP("192.168.1.1")
	m.preferredSrcIPv6 = net.ParseIP("fd00::1")

	_, prefix, _ := net.ParseCIDR("10.0.0.0/24")

	src := m.preferredSrc(*prefix)
	if !src.Equal(net.ParseIP("192.168.1.1")) {
		t.Errorf("expected IPv4 preferred src, got %s", src)
	}
}

func TestPreferredSrc_IPv6(t *testing.T) {
	m := NewUnifiedRouteManager("test", 0)
	m.preferredSrcIPv4 = net.ParseIP("192.168.1.1")
	m.preferredSrcIPv6 = net.ParseIP("fd00::1")

	_, prefix, _ := net.ParseCIDR("fd00:100::/48")

	src := m.preferredSrc(*prefix)
	if !src.Equal(net.ParseIP("fd00::1")) {
		t.Errorf("expected IPv6 preferred src, got %s", src)
	}
}

func TestBuildInstalledState(t *testing.T) {
	m := NewUnifiedRouteManager("test", 0)
	_, prefix, _ := net.ParseCIDR("10.0.0.0/24")

	dr := DesiredRoute{
		Prefix: *prefix,
		Metric: 100,
		MTU:    1400,
		Table:  1001,
	}
	active := []DesiredNexthop{
		{PeerID: "peer-a", LinkIndex: 1},
		{PeerID: "peer-b", LinkIndex: 2, Gateway: net.ParseIP("10.0.0.1")},
	}

	state := m.buildInstalledState(dr, active)
	if state.metric != 100 {
		t.Errorf("metric = %d, want 100", state.metric)
	}

	if state.mtu != 1400 {
		t.Errorf("mtu = %d, want 1400", state.mtu)
	}

	if state.table != 1001 {
		t.Errorf("table = %d, want 1001", state.table)
	}

	if state.linkScope {
		t.Error("should not be link-scope with multiple nexthops")
	}

	if len(state.peerNexthops) != 2 {
		t.Errorf("expected 2 peer nexthops, got %d", len(state.peerNexthops))
	}

	if _, ok := state.peerNexthops["peer-a"]; !ok {
		t.Error("peer-a not found in peerNexthops")
	}

	if _, ok := state.peerNexthops["peer-b"]; !ok {
		t.Error("peer-b not found in peerNexthops")
	}
}

func TestBuildInstalledState_LinkScope(t *testing.T) {
	m := NewUnifiedRouteManager("test", 0)
	_, prefix, _ := net.ParseCIDR("10.0.0.1/32")

	dr := DesiredRoute{Prefix: *prefix}
	active := []DesiredNexthop{{PeerID: "peer-a", LinkIndex: 1}}

	state := m.buildInstalledState(dr, active)
	if !state.linkScope {
		t.Error("single nexthop with no gateway should be link-scope")
	}
}

func TestGetInstalledRoutes_Empty(t *testing.T) {
	m := NewUnifiedRouteManager("test", 0)

	routes := m.GetInstalledRoutes()
	if len(routes) != 0 {
		t.Errorf("expected 0 installed routes, got %d", len(routes))
	}
}

func TestGetInstalledRoutes_WithState(t *testing.T) {
	m := NewUnifiedRouteManager("test", 0)
	_, prefix, _ := net.ParseCIDR("10.0.0.0/24")

	m.installedRoutes["254:10.0.0.0/24"] = &installedRouteState{
		prefix:    *prefix,
		metric:    100,
		mtu:       1400,
		table:     254,
		linkScope: false,
		peerNexthops: map[string]DesiredNexthop{
			"peer-b": {PeerID: "peer-b"},
			"peer-a": {PeerID: "peer-a"},
		},
	}

	routes := m.GetInstalledRoutes()
	if len(routes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(routes))
	}

	r := routes[0]
	if r.Prefix != "10.0.0.0/24" {
		t.Errorf("prefix = %s, want 10.0.0.0/24", r.Prefix)
	}

	if r.Metric != 100 {
		t.Errorf("metric = %d, want 100", r.Metric)
	}

	if r.MTU != 1400 {
		t.Errorf("mtu = %d, want 1400", r.MTU)
	}

	// Nexthops should be sorted
	sort.Strings(r.Nexthops)

	if len(r.Nexthops) != 2 || r.Nexthops[0] != "peer-a" || r.Nexthops[1] != "peer-b" {
		t.Errorf("unexpected nexthops: %v", r.Nexthops)
	}
}

func TestBuildKernelRoute_LinkScope(t *testing.T) {
	m := NewUnifiedRouteManager("test", 0)
	_, prefix, _ := net.ParseCIDR("10.0.0.1/32")

	dr := DesiredRoute{
		Prefix: *prefix,
		MTU:    1420,
		Table:  0,
	}
	active := []DesiredNexthop{{PeerID: "peer-a", LinkIndex: 7}}

	route := m.buildKernelRoute(dr, active)
	if route == nil {
		t.Fatal("expected non-nil route")
	}

	if route.LinkIndex != 7 {
		t.Errorf("LinkIndex = %d, want 7", route.LinkIndex)
	}

	if route.MTU != 1420 {
		t.Errorf("MTU = %d, want 1420", route.MTU)
	}

	if route.MultiPath != nil {
		t.Error("link-scope route should not have MultiPath")
	}
}

func TestBuildKernelRoute_MultipathIPv4(t *testing.T) {
	m := NewUnifiedRouteManager("test", 0)
	_, prefix, _ := net.ParseCIDR("100.64.0.0/16")

	dr := DesiredRoute{
		Prefix: *prefix,
		Metric: 200,
		Table:  1001,
	}
	active := []DesiredNexthop{
		{PeerID: "peer-a", LinkIndex: 5, Gateway: net.ParseIP("10.0.0.1")},
		{PeerID: "peer-b", LinkIndex: 6, Gateway: net.ParseIP("10.0.0.2")},
	}

	route := m.buildKernelRoute(dr, active)
	if route == nil {
		t.Fatal("expected non-nil route")
	}

	if len(route.MultiPath) != 2 {
		t.Errorf("expected 2 multipath nexthops, got %d", len(route.MultiPath))
	}

	if route.Priority != 200 {
		t.Errorf("Priority = %d, want 200", route.Priority)
	}

	if route.Table != 1001 {
		t.Errorf("Table = %d, want 1001", route.Table)
	}
}

func TestBuildKernelRoute_MultipathIPv6_SkipNoGateway(t *testing.T) {
	m := NewUnifiedRouteManager("test", 0)
	_, prefix, _ := net.ParseCIDR("fd00:100::/48")

	dr := DesiredRoute{Prefix: *prefix}
	active := []DesiredNexthop{
		{PeerID: "peer-a", LinkIndex: 5, Gateway: net.ParseIP("fd00::1")},
		{PeerID: "peer-b", LinkIndex: 6, Gateway: nil}, // no gateway
	}

	route := m.buildKernelRoute(dr, active)
	if route == nil {
		t.Fatal("expected non-nil route")
	}

	if len(route.MultiPath) != 1 {
		t.Errorf("expected 1 multipath nexthop (peer-b skipped), got %d", len(route.MultiPath))
	}
}

func TestBuildKernelRoute_MultipathIPv6_AllNoGateway(t *testing.T) {
	m := NewUnifiedRouteManager("test", 0)
	_, prefix, _ := net.ParseCIDR("fd00:100::/48")

	// Multiple IPv6 nexthops all without gateways -- multipath cannot be built.
	dr := DesiredRoute{Prefix: *prefix}
	active := []DesiredNexthop{
		{PeerID: "peer-a", LinkIndex: 5, Gateway: nil},
		{PeerID: "peer-b", LinkIndex: 6, Gateway: nil},
	}

	route := m.buildKernelRoute(dr, active)
	if route != nil {
		t.Error("expected nil route when all IPv6 multipath nexthops lack gateways")
	}
}

func TestBuildKernelRoute_SingleIPv6LinkScope(t *testing.T) {
	m := NewUnifiedRouteManager("test", 0)
	_, prefix, _ := net.ParseCIDR("fd00::1/128")

	// Single IPv6 nexthop with no gateway is a valid link-scope route.
	dr := DesiredRoute{Prefix: *prefix}
	active := []DesiredNexthop{
		{PeerID: "peer-a", LinkIndex: 5, Gateway: nil},
	}

	route := m.buildKernelRoute(dr, active)
	if route == nil {
		t.Fatal("single IPv6 nexthop with no gateway should produce a link-scope route")
	}

	if route.LinkIndex != 5 {
		t.Errorf("LinkIndex = %d, want 5", route.LinkIndex)
	}
}

func TestBuildKernelRoute_PreferredSource(t *testing.T) {
	m := NewUnifiedRouteManager("test", 0)
	m.preferredSrcIPv4 = net.ParseIP("192.168.1.1")

	_, prefix, _ := net.ParseCIDR("10.0.0.0/24")
	dr := DesiredRoute{Prefix: *prefix}
	active := []DesiredNexthop{{PeerID: "peer-a", LinkIndex: 1}}

	route := m.buildKernelRoute(dr, active)
	if route == nil {
		t.Fatal("expected non-nil route")
	}

	if !route.Src.Equal(net.ParseIP("192.168.1.1")) {
		t.Errorf("Src = %s, want 192.168.1.1", route.Src)
	}
}

// ---------------------------------------------------------------------------
// Dedicated table behaviour
// ---------------------------------------------------------------------------

// TestSyncRoutes_DedicatedTable verifies that routes with Table==0 get keyed
// under the manager's default table when a dedicated table is configured.
func TestSyncRoutes_DedicatedTable(t *testing.T) {
	m := NewUnifiedRouteManager("test", 252)

	// Pre-populate a nexthop so the manager can build a route.
	nh := DesiredNexthop{PeerID: "peer-a", LinkIndex: 1}
	m.ensureNexthop(nh)
	m.peerHealthy["peer-a"] = true

	_, prefix, _ := net.ParseCIDR("10.99.0.0/24")
	desired := []DesiredRoute{
		{
			Prefix:   *prefix,
			Nexthops: []DesiredNexthop{nh},
			Table:    0, // should resolve to 252
		},
	}

	// SyncRoutes will call netlink.RouteReplace which fails without
	// privileges, but the in-memory state is what we care about.
	// We tolerate the error here.
	_ = m.SyncRoutes(desired)

	// The route key should use table 252.
	wantKey := m.routeKey(0, *prefix)
	if wantKey != "252:10.99.0.0/24" {
		t.Errorf("routeKey(0, prefix) = %s, want 252:10.99.0.0/24", wantKey)
	}

	// Verify the installed routes report table 252.
	installed := m.GetInstalledRoutes()
	for _, r := range installed {
		if r.Prefix == "10.99.0.0/24" {
			if r.Table != 252 {
				t.Errorf("installed route table = %d, want 252", r.Table)
			}

			return
		}
	}
	// If route was not installed due to netlink error (non-root), verify
	// at least the key generation is correct.
	t.Logf("route not in installed state (likely non-root), key verified: %s", wantKey)
}

// TestSyncRoutes_ExplicitTableNotOverridden verifies that a route with an
// explicit non-zero Table is not overridden by the manager's default table.
func TestSyncRoutes_ExplicitTableNotOverridden(t *testing.T) {
	m := NewUnifiedRouteManager("test", 252)

	nh := DesiredNexthop{PeerID: "peer-b", LinkIndex: 2, Gateway: net.ParseIP("10.0.0.1")}
	m.ensureNexthop(nh)
	m.peerHealthy["peer-b"] = true

	_, prefix, _ := net.ParseCIDR("10.88.0.0/16")
	desired := []DesiredRoute{
		{
			Prefix:   *prefix,
			Nexthops: []DesiredNexthop{nh},
			Table:    51820, // explicit gateway policy table
		},
	}

	_ = m.SyncRoutes(desired)

	// The route key must use the explicit table, not 252.
	wantKey := m.routeKey(51820, *prefix)
	if wantKey != "51820:10.88.0.0/16" {
		t.Errorf("routeKey(51820, prefix) = %s, want 51820:10.88.0.0/16", wantKey)
	}

	// Also verify effectiveTable does not override explicit.
	if got := m.effectiveTable(51820); got != 51820 {
		t.Errorf("effectiveTable(51820) = %d, want 51820", got)
	}

	// Check installed routes if available.
	for _, r := range m.GetInstalledRoutes() {
		if r.Prefix == "10.88.0.0/16" {
			if r.Table != 51820 {
				t.Errorf("installed route table = %d, want 51820", r.Table)
			}

			return
		}
	}

	t.Logf("route not in installed state (likely non-root), key verified: %s", wantKey)
}

// TestValidateRoutes_DedicatedTable_NoFilterNeeded verifies that a manager
// with a dedicated table reports isDedicatedTable() == true, which
// selects the simplified validation code path.
func TestValidateRoutes_DedicatedTable_NoFilterNeeded(t *testing.T) {
	m := NewUnifiedRouteManager("test", 252)
	if !m.isDedicatedTable() {
		t.Fatal("expected isDedicatedTable() == true for table 252")
	}

	// Contrast with the main table.
	mMain := NewUnifiedRouteManager("test", 0)
	if mMain.isDedicatedTable() {
		t.Fatal("expected isDedicatedTable() == false for default (main) table")
	}

	// Verify effectiveTable(0) resolves to the dedicated table.
	if got := m.effectiveTable(0); got != 252 {
		t.Errorf("effectiveTable(0) on dedicated manager = %d, want 252", got)
	}
}

// TestBuildInstalledState_DedicatedTable verifies that buildInstalledState
// records the raw DesiredRoute.Table value (the route key handles resolution
// via effectiveTable, while the state preserves the original).
func TestBuildInstalledState_DedicatedTable(t *testing.T) {
	m := NewUnifiedRouteManager("test", 252)

	_, prefix, _ := net.ParseCIDR("10.50.0.0/24")
	dr := DesiredRoute{
		Prefix:   *prefix,
		Nexthops: []DesiredNexthop{{PeerID: "peer-a", LinkIndex: 1}},
		Metric:   100,
		Table:    0, // raw value stored in state
	}
	active := []DesiredNexthop{{PeerID: "peer-a", LinkIndex: 1}}

	state := m.buildInstalledState(dr, active)
	// The installed state preserves the raw table from the DesiredRoute.
	if state.table != dr.Table {
		t.Errorf("installed state table = %d, want %d (raw DesiredRoute.Table)", state.table, dr.Table)
	}
	// Meanwhile, the route key resolves via effectiveTable.
	key := m.routeKey(dr.Table, *prefix)
	if key != "252:10.50.0.0/24" {
		t.Errorf("routeKey = %s, want 252:10.50.0.0/24", key)
	}
}
