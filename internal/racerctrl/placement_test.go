// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package racerctrl

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// placeAll runs a whole pass of placement over a set of unplaced nodes, which
// is what the operator does: one placer, every waiting node, in name order.
func placeAll(t *testing.T, cursors *Cursors, placed []PlacedNode, nodes []PlacementNode) []Placement {
	t.Helper()

	placer := NewPlacer(cursors, placed, nodes)
	out := make([]Placement, 0, len(nodes))

	for _, node := range nodes {
		placement, err := placer.Place(node)
		if err != nil {
			t.Fatalf("place %s: %v", node.Name, err)
		}

		out = append(out, placement)
	}

	return out
}

func TestPlaceHonoursDeclaredZoneName(t *testing.T) {
	cursors := Cursors{}

	nodes := []PlacementNode{
		{Name: "a", Site: "edge", AZ: "az1", Fabric: "f1", ZoneName: "chosen"},
		{Name: "b", Site: "edge", AZ: "az2", Fabric: "f2", ZoneName: "chosen"},
		{Name: "c", Site: "edge", AZ: "az3", Fabric: "f1"},
	}

	got := placeAll(t, &cursors, nil, nodes)

	if got[0].Zone != got[1].Zone {
		t.Fatalf("nodes naming the same zone landed in %d and %d", got[0].Zone, got[1].Zone)
	}

	// The declared zone is a zone like any other once it exists, so the node
	// that named none joins it rather than minting a second.
	if got[2].Zone != got[0].Zone {
		t.Fatalf("undeclared node landed in zone %d, want the declared zone %d", got[2].Zone, got[0].Zone)
	}

	if def := cursors.ZoneDefs[got[0].Zone]; def.Site != "edge" {
		t.Fatalf("declared zone recorded site %q, want edge", def.Site)
	}
}

func TestPlaceNeverCrossesSites(t *testing.T) {
	cursors := Cursors{}

	nodes := []PlacementNode{
		{Name: "a", Site: "one", AZ: "az1", Fabric: "f"},
		{Name: "b", Site: "two", AZ: "az1", Fabric: "f"},
		{Name: "c", Site: "one", AZ: "az2", Fabric: "f"},
		{Name: "d", Site: "two", AZ: "az2", Fabric: "f"},
	}

	got := placeAll(t, &cursors, nil, nodes)

	if got[0].Zone != got[2].Zone {
		t.Fatalf("site one split across zones %d and %d", got[0].Zone, got[2].Zone)
	}

	if got[1].Zone != got[3].Zone {
		t.Fatalf("site two split across zones %d and %d", got[1].Zone, got[3].Zone)
	}

	if got[0].Zone == got[1].Zone {
		t.Fatalf("both sites landed in zone %d", got[0].Zone)
	}

	for zone, def := range cursors.ZoneDefs {
		if def.Site == "" {
			t.Fatalf("zone %d has no site", zone)
		}
	}
}

func TestPlaceSeedsAZoneForAFabricWithEnoughNodes(t *testing.T) {
	cursors := Cursors{}

	// One node already holds a zone on fabric f1. Three more arrive on f2,
	// which is MinZoneSeed, so they are worth a zone of their own rather than
	// being scattered into f1's.
	placed := []PlacedNode{{Zone: 1, Cohort: 0, AZ: "az1", Fabric: "f1"}}
	cursors.NextZoneID = 2
	cursors.DefineZone(1, ZoneDef{Site: "edge", Fabric: "f1"})

	nodes := []PlacementNode{
		{Name: "a", Site: "edge", AZ: "az1", Fabric: "f2"},
		{Name: "b", Site: "edge", AZ: "az2", Fabric: "f2"},
		{Name: "c", Site: "edge", AZ: "az3", Fabric: "f2"},
	}

	got := placeAll(t, &cursors, placed, nodes)

	for i, placement := range got {
		if placement.Zone == 1 {
			t.Fatalf("node %s joined the fabric f1 zone", nodes[i].Name)
		}

		if placement.Zone != got[0].Zone {
			t.Fatalf("fabric f2 split across zones %d and %d", got[0].Zone, placement.Zone)
		}
	}

	if def := cursors.ZoneDefs[got[0].Zone]; def.Fabric != "f2" {
		t.Fatalf("minted zone seeded on fabric %q, want f2", def.Fabric)
	}
}

func TestPlaceMakesBridgeNodesBelowTheSeedThreshold(t *testing.T) {
	cursors := Cursors{}

	// Two nodes on f2 is below MinZoneSeed, so they join the existing zone and
	// become bridge nodes: full members of a zone seeded on f1 while sitting on
	// f2.
	placed := []PlacedNode{{Zone: 1, Cohort: 0, AZ: "az1", Fabric: "f1"}}
	cursors.NextZoneID = 2
	cursors.DefineZone(1, ZoneDef{Site: "edge", Fabric: "f1"})

	nodes := []PlacementNode{
		{Name: "a", Site: "edge", AZ: "az2", Fabric: "f2"},
		{Name: "b", Site: "edge", AZ: "az3", Fabric: "f2"},
	}

	got := placeAll(t, &cursors, placed, nodes)

	for i, placement := range got {
		if placement.Zone != 1 {
			t.Fatalf("node %s landed in zone %d, want the existing zone 1", nodes[i].Name, placement.Zone)
		}
	}

	if len(cursors.ZoneDefs) != 1 {
		t.Fatalf("placement minted %d zones, want 1", len(cursors.ZoneDefs))
	}
}

func TestPlaceBalancesAvailabilityZonesAcrossZones(t *testing.T) {
	cursors := Cursors{NextZoneID: 3}
	cursors.DefineZone(1, ZoneDef{Site: "edge", Fabric: "f"})
	cursors.DefineZone(2, ZoneDef{Site: "edge", Fabric: "f"})

	// Zone 1 already holds two nodes from az1; zone 2 holds none. A third az1
	// node belongs in zone 2.
	placed := []PlacedNode{
		{Zone: 1, Cohort: 0, AZ: "az1", Fabric: "f"},
		{Zone: 1, Cohort: 1, AZ: "az1", Fabric: "f"},
		{Zone: 2, Cohort: 0, AZ: "az2", Fabric: "f"},
	}

	got := placeAll(t, &cursors, placed, []PlacementNode{
		{Name: "a", Site: "edge", AZ: "az1", Fabric: "f"},
	})

	if got[0].Zone != 2 {
		t.Fatalf("node landed in zone %d, want 2, which holds fewer az1 nodes", got[0].Zone)
	}
}

func TestPlaceMintsANewZoneAtTheTarget(t *testing.T) {
	cursors := Cursors{NextZoneID: 2, ZoneTarget: 3}
	cursors.DefineZone(1, ZoneDef{Site: "edge", Fabric: "f"})

	placed := []PlacedNode{
		{Zone: 1, Cohort: 0, AZ: "az1", Fabric: "f"},
		{Zone: 1, Cohort: 1, AZ: "az2", Fabric: "f"},
		{Zone: 1, Cohort: 2, AZ: "az3", Fabric: "f"},
	}

	got := placeAll(t, &cursors, placed, []PlacementNode{
		{Name: "a", Site: "edge", AZ: "az1", Fabric: "f"},
	})

	if got[0].Zone == 1 {
		t.Fatal("node joined a zone already at the target")
	}

	if def := cursors.ZoneDefs[got[0].Zone]; def.Site != "edge" {
		t.Fatalf("minted zone recorded site %q, want edge", def.Site)
	}
}

func TestPlaceIsIndependentOfNodeOrder(t *testing.T) {
	nodes := []PlacementNode{
		{Name: "a", Site: "edge", AZ: "az1", Fabric: "f1"},
		{Name: "b", Site: "edge", AZ: "az2", Fabric: "f1"},
		{Name: "c", Site: "edge", AZ: "az3", Fabric: "f1"},
	}

	forward := Cursors{}
	first := placeAll(t, &forward, nil, nodes)

	// The same nodes arriving over three passes rather than one must land the
	// same way: a restart between two nodes is indistinguishable from a
	// requeue, and placement is re-derived from published state each time.
	stepped := Cursors{}
	placedSoFar := make([]PlacedNode, 0, len(nodes))

	for i, node := range nodes {
		placement := placeAll(t, &stepped, placedSoFar, nodes[i:])[0]
		if placement != first[i] {
			t.Fatalf("node %s placed at %+v in one pass and %+v across passes", node.Name, first[i], placement)
		}

		placedSoFar = append(placedSoFar, PlacedNode{
			Zone:   placement.Zone,
			Cohort: placement.Cohort,
			AZ:     node.AZ,
			Fabric: node.Fabric,
		})
	}
}

func TestPlaceRefusesToExceedMaxZones(t *testing.T) {
	cursors := Cursors{NextZoneID: uint32(MaxZones) + 1}

	placer := NewPlacer(&cursors, nil, nil)

	if _, err := placer.Place(PlacementNode{Name: "a", Site: "edge", AZ: "az1"}); err == nil {
		t.Fatal("placement past the zone ceiling succeeded, want an error")
	}
}

func TestCohortsAreAvailabilityZones(t *testing.T) {
	cursors := Cursors{}

	// Nine nodes over three availability zones, arriving interleaved so that a
	// naive least-loaded cohort would mix them.
	nodes := make([]PlacementNode, 0, 9)

	for i := range 9 {
		az := fmt.Sprintf("az%d", i%3)
		nodes = append(nodes, PlacementNode{
			Name:   fmt.Sprintf("n%d", i),
			Site:   "edge",
			AZ:     az,
			Fabric: "f",
		})
	}

	got := placeAll(t, &cursors, nil, nodes)

	zone := got[0].Zone

	buckets := cursors.ZoneBuckets[zone]
	if len(buckets) != Cohorts {
		t.Fatalf("zone recorded %d availability zone buckets, want %d", len(buckets), Cohorts)
	}

	seen := map[uint32]string{}

	for az, bucket := range buckets {
		if other, ok := seen[bucket]; ok {
			t.Fatalf("availability zones %q and %q share cohort %d", az, other, bucket)
		}

		seen[bucket] = az
	}

	// Build the catalog the way the operator would and check that every trio
	// spans three distinct availability zones. Only the nodes placed after the
	// third availability zone was learned are guaranteed to sit in their own
	// bucket, so the check is over those.
	members := make(Membership, 0, len(got))
	azOf := map[uint32]string{}

	for i, placement := range got {
		if buckets[nodes[i].AZ] != placement.Cohort {
			continue
		}

		id := uint32(i + 1)
		members = append(members, Member{NodeID: id, Cohort: placement.Cohort})
		azOf[id] = nodes[i].AZ
	}

	perCohort, err := members.PerCohort()
	if err != nil || perCohort == 0 {
		t.Fatalf("membership is not balanced: %v", err)
	}

	catalog, err := BuildCatalog(members, perCohort*2)
	if err != nil {
		t.Fatalf("build catalog: %v", err)
	}

	for i, trio := range catalog {
		a, b, c := azOf[trio.GetCohort_0()], azOf[trio.GetCohort_1()], azOf[trio.GetCohort_2()]
		if a == b || a == c || b == c {
			t.Fatalf("group %d spans availability zones %q, %q, %q, which are not distinct", i, a, b, c)
		}
	}
}

func TestCohortBucketsAreAppendOnly(t *testing.T) {
	cursors := Cursors{}

	nodes := []PlacementNode{
		{Name: "a", Site: "edge", AZ: "az1", Fabric: "f"},
		{Name: "b", Site: "edge", AZ: "az2", Fabric: "f"},
		{Name: "c", Site: "edge", AZ: "az3", Fabric: "f"},
	}

	got := placeAll(t, &cursors, nil, nodes)

	zone := got[0].Zone
	before := map[string]uint32{}

	for az, bucket := range cursors.ZoneBuckets[zone] {
		before[az] = bucket
	}

	// A fourth availability zone joins the cohort backing the fewest, and the
	// three already recorded do not move.
	placed := make([]PlacedNode, 0, len(got))
	for i, placement := range got {
		placed = append(placed, PlacedNode{
			Zone:   placement.Zone,
			Cohort: placement.Cohort,
			AZ:     nodes[i].AZ,
			Fabric: nodes[i].Fabric,
		})
	}

	placeAll(t, &cursors, placed, []PlacementNode{
		{Name: "d", Site: "edge", AZ: "az4", Fabric: "f"},
	})

	for az, bucket := range before {
		if cursors.ZoneBuckets[zone][az] != bucket {
			t.Fatalf("availability zone %q moved from cohort %d to %d", az, bucket, cursors.ZoneBuckets[zone][az])
		}
	}

	if _, ok := cursors.ZoneBuckets[zone]["az4"]; !ok {
		t.Fatal("the new availability zone was not recorded")
	}
}

func TestCohortsFallBackWithFewerThanThreeAvailabilityZones(t *testing.T) {
	cursors := Cursors{}

	nodes := []PlacementNode{
		{Name: "a", Site: "edge", AZ: "az1", Fabric: "f"},
		{Name: "b", Site: "edge", AZ: "az1", Fabric: "f"},
		{Name: "c", Site: "edge", AZ: "az1", Fabric: "f"},
	}

	got := placeAll(t, &cursors, nil, nodes)

	// One availability zone cannot fill three cohorts, and a zone with an empty
	// cohort has no catalog at all, so the cohorts are levelled instead.
	seen := map[uint32]bool{}
	for _, placement := range got {
		seen[placement.Cohort] = true
	}

	if len(seen) != Cohorts {
		t.Fatalf("three nodes in one availability zone filled %d cohorts, want %d", len(seen), Cohorts)
	}
}

func TestPlaceCohortsNodesWithNoAvailabilityZone(t *testing.T) {
	cursors := Cursors{}

	nodes := []PlacementNode{
		{Name: "a", Site: "edge", Fabric: "f"},
		{Name: "b", Site: "edge", Fabric: "f"},
		{Name: "c", Site: "edge", Fabric: "f"},
	}

	got := placeAll(t, &cursors, nil, nodes)

	seen := map[uint32]bool{}
	for _, placement := range got {
		seen[placement.Cohort] = true
	}

	if len(seen) != Cohorts {
		t.Fatalf("cohorts %v, want all three filled", seen)
	}

	if len(cursors.ZoneBuckets[got[0].Zone]) != 0 {
		t.Fatal("a node with no availability zone claimed a bucket")
	}
}

func TestSelectGatewaysPrefersBridgeNodes(t *testing.T) {
	members := Membership{
		{NodeID: 1, Cohort: 0},
		{NodeID: 2, Cohort: 0},
		{NodeID: 3, Cohort: 0},
		{NodeID: 4, Cohort: 1},
		{NodeID: 5, Cohort: 1},
		{NodeID: 6, Cohort: 1},
		{NodeID: 7, Cohort: 2},
		{NodeID: 8, Cohort: 2},
		{NodeID: 9, Cohort: 2},
	}

	// 3, 6 and 9 are the highest ids in their cohorts, so round-robin alone
	// would never reach them.
	bridge := map[uint32]bool{3: true, 6: true, 9: true}

	got := SelectGateways(members, bridge, 3)

	want := []uint32{3, 6, 9}
	if len(got) != len(want) {
		t.Fatalf("selected %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("selected %v, want %v", got, want)
		}
	}
}

func TestSelectGatewaysRoundRobinsCohorts(t *testing.T) {
	members := Membership{
		{NodeID: 1, Cohort: 0},
		{NodeID: 2, Cohort: 0},
		{NodeID: 3, Cohort: 1},
		{NodeID: 4, Cohort: 1},
		{NodeID: 5, Cohort: 2},
		{NodeID: 6, Cohort: 2},
	}

	got := SelectGateways(members, nil, 3)

	// Lowest of each cohort, so losing one availability zone leaves two
	// gateways rather than none.
	want := []uint32{1, 3, 5}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("selected %v, want %v", got, want)
		}
	}
}

func TestSelectGatewaysHonoursCount(t *testing.T) {
	members := make(Membership, 0, 3*MaxGateways)
	for cohort := range uint32(Cohorts) {
		for i := range MaxGateways {
			members = append(members, Member{NodeID: cohort*uint32(MaxGateways) + uint32(i) + 1, Cohort: cohort})
		}
	}

	if got := SelectGateways(members, nil, 0); len(got) != DefaultGatewayCount {
		t.Fatalf("zero count selected %d gateways, want the default %d", len(got), DefaultGatewayCount)
	}

	if got := SelectGateways(members, nil, 4*MaxGateways); len(got) != MaxGateways {
		t.Fatalf("oversized count selected %d gateways, want the cap %d", len(got), MaxGateways)
	}

	if got := SelectGateways(members, nil, 5); len(got) != 5 {
		t.Fatalf("selected %d gateways, want 5", len(got))
	}
}

func TestSelectGatewaysStopsAtTheMembership(t *testing.T) {
	members := Membership{
		{NodeID: 1, Cohort: 0},
		{NodeID: 2, Cohort: 1},
		{NodeID: 3, Cohort: 2},
	}

	if got := SelectGateways(members, nil, 10); len(got) != 3 {
		t.Fatalf("selected %v, want every member and no more", got)
	}
}

func TestPlacementDrift(t *testing.T) {
	buckets := map[string]uint32{"az1": 0, "az2": 1, "az3": 2}

	tests := []struct {
		name  string
		node  PlacementNode
		place Placement
		def   ZoneDef
		want  bool
	}{
		{
			name:  "settled",
			node:  PlacementNode{Name: "a", Site: "edge", AZ: "az1"},
			place: Placement{Zone: 1, Cohort: 0},
			def:   ZoneDef{Site: "edge", Fabric: "f"},
		},
		{
			name:  "site moved",
			node:  PlacementNode{Name: "a", Site: "other", AZ: "az1"},
			place: Placement{Zone: 1, Cohort: 0},
			def:   ZoneDef{Site: "edge", Fabric: "f"},
			want:  true,
		},
		{
			name:  "availability zone moved",
			node:  PlacementNode{Name: "a", Site: "edge", AZ: "az2"},
			place: Placement{Zone: 1, Cohort: 0},
			def:   ZoneDef{Site: "edge", Fabric: "f"},
			want:  true,
		},
		{
			name:  "fabric moved is not drift",
			node:  PlacementNode{Name: "a", Site: "edge", AZ: "az1", Fabric: "other"},
			place: Placement{Zone: 1, Cohort: 0},
			def:   ZoneDef{Site: "edge", Fabric: "f"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := PlacementDrift(test.node, test.place, test.def, buckets)
			if (got != "") != test.want {
				t.Fatalf("drift %q, want reported = %v", got, test.want)
			}
		})
	}
}

// Zones never cross sites, so a declared name is interned per site. Joining the
// two raw meant a site whose name contained the separator interned to the same
// key as a differently split pair, and two failure domains sharing a zone id is
// a silent merge rather than an error.
func TestDeclaredZoneKeyCannotCollideAcrossSites(t *testing.T) {
	assert.NotEqual(t,
		declaredZoneKey("east|rack", "3"),
		declaredZoneKey("east", "rack|3"),
	)

	assert.Equal(t, declaredZoneKey("east", "rack-3"), declaredZoneKey("east", "rack-3"),
		"the same pair still interns to the same key")

	assert.NotEqual(t, declaredZoneKey("east", "rack-3"), declaredZoneKey("west", "rack-3"),
		"two sites may use the same zone name without being merged")
}
