// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package racerctrl

import (
	"fmt"
	"net/url"
	"sort"
)

// Automatic zone and cohort placement.
//
// A racer zone is a failure domain, a single catalog and a single anti-entropy
// domain. An operator may name one per node and that always wins, but naming a
// zone for every node in a thousand-node cluster is not a thing anyone wants to
// do, so this file works one out.
//
// Everything here is pure. It reads the cursors and a census of what is already
// placed, and returns the zone and cohort one node should take; the caller
// commits the mutated cursors before it stamps the node, so a crash between the
// two leaks a zone id rather than placing a node twice. Zone ids were never
// reusable, so a leaked one costs nothing but a slot in the 64 a universe may
// name.

// PlacementNode is what placement knows about a node before it is placed.
type PlacementNode struct {
	// Name is the Kubernetes node name, used only for error messages.
	Name string

	// Site is the unbounded site the node belongs to. A zone never spans two
	// sites, so this is the one hard partition placement observes.
	Site string

	// AZ is the node's availability zone, from the standard Kubernetes topology
	// label. Empty for a node that declares none, which costs it the guarantee
	// that its trios span three of them.
	AZ string

	// Fabric is the RDMA fabric the node sits on, from its fabric-id
	// annotation. Empty is a value like any other: nodes with no fabric prefer
	// each other's company for exactly the same reason nodes on fabric "a" do,
	// which is that nothing else is known to distinguish them.
	Fabric string

	// ZoneName is the zone the operator asked for. Non-empty takes precedence
	// over everything below.
	ZoneName string
}

// Placement is where one node lands.
type Placement struct {
	Zone   uint32
	Cohort uint32
}

// PlacedNode is one already-placed node, as the census sees it.
type PlacedNode struct {
	Zone   uint32
	Cohort uint32
	AZ     string
	Fabric string
}

// zoneCensus is what is already in a zone. Placement balances against it and
// the caller never sees it.
type zoneCensus struct {
	total    int
	byAZ     map[string]int
	byCohort [Cohorts]int
}

// Placer places nodes into zones and cohorts, one at a time, carrying the
// decisions it has already made. It mutates the cursors it is given; the caller
// is expected to commit them before stamping any node.
type Placer struct {
	cursors *Cursors

	// census is what each zone already holds, updated as nodes are placed so a
	// pass that places several nodes spreads them rather than stacking them.
	census map[uint32]*zoneCensus

	// unplaced counts the nodes still waiting for an identity, keyed by site and
	// fabric. It is what decides whether a fabric is worth a zone of its own,
	// and it is decremented as those nodes are placed.
	unplaced map[fabricKey]int
}

// fabricKey identifies one fabric within one site.
type fabricKey struct {
	site   string
	fabric string
}

// NewPlacer builds a placer over the current cursors, the nodes already placed
// and the nodes still waiting. It does not copy the cursors: placing a node
// allocates ids out of them.
func NewPlacer(cursors *Cursors, placed []PlacedNode, unplaced []PlacementNode) *Placer {
	p := &Placer{
		cursors:  cursors,
		census:   map[uint32]*zoneCensus{},
		unplaced: map[fabricKey]int{},
	}

	for _, node := range placed {
		if node.Zone == 0 {
			continue
		}

		zone := p.zoneCensus(node.Zone)
		zone.total++
		zone.byAZ[node.AZ]++

		if node.Cohort < Cohorts {
			zone.byCohort[node.Cohort]++
		}
	}

	for _, node := range unplaced {
		// A node that named its zone is not evidence that its fabric deserves
		// one: it is going where it was told either way.
		if node.ZoneName != "" {
			continue
		}

		p.unplaced[fabricKey{site: node.Site, fabric: node.Fabric}]++
	}

	return p
}

// zoneCensus returns a zone's census, creating an empty one for a zone that has
// no nodes yet.
func (p *Placer) zoneCensus(zone uint32) *zoneCensus {
	census, ok := p.census[zone]
	if !ok {
		census = &zoneCensus{byAZ: map[string]int{}}
		p.census[zone] = census
	}

	return census
}

// target is how many nodes a zone is filled to before a new one is minted.
func (p *Placer) target() int {
	if p.cursors.ZoneTarget != 0 {
		return int(p.cursors.ZoneTarget)
	}

	return DefaultZoneTarget
}

// Place decides where a node goes and records the decision, so the next call
// sees it. It allocates a zone id when it mints a zone, and it is the caller's
// job to commit the cursors before stamping the node.
func (p *Placer) Place(node PlacementNode) (Placement, error) {
	zone, err := p.placeZone(node)
	if err != nil {
		return Placement{}, err
	}

	cohort := p.placeCohort(zone, node.AZ)

	census := p.zoneCensus(zone)
	census.total++
	census.byAZ[node.AZ]++
	census.byCohort[cohort]++

	if node.ZoneName == "" {
		key := fabricKey{site: node.Site, fabric: node.Fabric}
		if p.unplaced[key] > 0 {
			p.unplaced[key]--
		}
	}

	return Placement{Zone: zone, Cohort: cohort}, nil
}

// placeZone picks the zone a node joins.
//
// The order is: a zone the operator named, then a zone of this site already on
// this node's fabric, then a zone of this node's own if enough of its fabric is
// waiting to fill one, then any zone of this site with room, then a new one.
// The fourth rule is what produces bridge nodes: a node on fabric A joining a
// zone seeded on fabric B is a full member of that zone's catalog while still
// being one RDMA hop from everything on A, which is exactly what a gateway
// wants to be.
func (p *Placer) placeZone(node PlacementNode) (uint32, error) {
	if node.ZoneName != "" {
		zone, err := p.cursors.ZoneID(declaredZoneKey(node.Site, node.ZoneName))
		if err != nil {
			return 0, fmt.Errorf("zone %q for node %s: %w", node.ZoneName, node.Name, err)
		}

		// A declared zone still records its site, so a later automatic
		// placement will not put a node from another site into it.
		p.cursors.DefineZone(zone, ZoneDef{Site: node.Site, Fabric: node.Fabric})

		return zone, nil
	}

	room := p.zonesWithRoom(node.Site)

	if zone, ok := p.best(room, node.AZ, func(def ZoneDef) bool { return def.Fabric == node.Fabric }); ok {
		return zone, nil
	}

	if p.unplaced[fabricKey{site: node.Site, fabric: node.Fabric}] >= MinZoneSeed {
		return p.mint(node)
	}

	if zone, ok := p.best(room, node.AZ, func(ZoneDef) bool { return true }); ok {
		return zone, nil
	}

	return p.mint(node)
}

// mint creates a zone seeded on a node's site and fabric.
func (p *Placer) mint(node PlacementNode) (uint32, error) {
	zone, err := p.cursors.AllocateZoneID()
	if err != nil {
		return 0, fmt.Errorf("mint a zone for node %s: %w", node.Name, err)
	}

	p.cursors.DefineZone(zone, ZoneDef{Site: node.Site, Fabric: node.Fabric})

	return zone, nil
}

// zonesWithRoom lists this site's zones that are below the node target, lowest
// id first.
func (p *Placer) zonesWithRoom(site string) []uint32 {
	target := p.target()
	zones := make([]uint32, 0, len(p.cursors.ZoneDefs))

	for zone, def := range p.cursors.ZoneDefs {
		if def.Site != site {
			continue
		}

		if census, ok := p.census[zone]; ok && census.total >= target {
			continue
		}

		zones = append(zones, zone)
	}

	sort.Slice(zones, func(i, j int) bool { return zones[i] < zones[j] })

	return zones
}

// best picks the zone among candidates that holds the fewest nodes from this
// node's availability zone, so a zone's availability zones stay level and its
// cohorts with them. Ties go to the lowest id, which makes the choice a
// function of published state rather than of map iteration order.
func (p *Placer) best(candidates []uint32, az string, admit func(ZoneDef) bool) (uint32, bool) {
	var (
		best  uint32
		count int
	)

	for _, zone := range candidates {
		if !admit(p.cursors.ZoneDefs[zone]) {
			continue
		}

		held := 0
		if census, ok := p.census[zone]; ok {
			held = census.byAZ[az]
		}

		if best == 0 || held < count {
			best, count = zone, held
		}
	}

	return best, best != 0
}

// placeCohort picks the cohort a node joins.
//
// A trio takes one node from each cohort, so making a cohort an availability
// zone makes every trio span three of them. The mapping is append-only and the
// first three availability zones a zone sees take distinct cohorts, which is
// what makes that true: distinct cohorts hold disjoint sets of availability
// zones, so three distinct cohorts are three distinct availability zones.
//
// Until a zone has seen three, the mapping cannot be used - two availability
// zones cannot fill three cohorts, and a zone with an empty cohort has no
// catalog at all - so cohorts are filled evenly instead and the nodes placed in
// that window keep whatever cohort they were given. A zone that grows from one
// availability zone to three is not repartitioned; a cohort is frozen for a
// node's life.
func (p *Placer) placeCohort(zone uint32, az string) uint32 {
	census := p.zoneCensus(zone)

	if az == "" {
		return leastLoaded(census.byCohort)
	}

	bucket := p.bucket(zone, az)

	if len(p.cursors.ZoneBuckets[zone]) < Cohorts {
		return leastLoaded(census.byCohort)
	}

	return bucket
}

// bucket returns the cohort an availability zone maps to in a zone, recording
// the mapping the first time the availability zone is seen. An unclaimed cohort
// is taken first, so the first three availability zones are guaranteed distinct
// cohorts; past that the cohort backing the fewest availability zones wins.
func (p *Placer) bucket(zone uint32, az string) uint32 {
	if p.cursors.ZoneBuckets == nil {
		p.cursors.ZoneBuckets = map[uint32]map[string]uint32{}
	}

	buckets, ok := p.cursors.ZoneBuckets[zone]
	if !ok {
		buckets = map[string]uint32{}
		p.cursors.ZoneBuckets[zone] = buckets
	}

	if bucket, ok := buckets[az]; ok {
		return bucket
	}

	var claims [Cohorts]int
	for _, bucket := range buckets {
		if bucket < Cohorts {
			claims[bucket]++
		}
	}

	bucket := leastLoaded(claims)
	buckets[az] = bucket

	return bucket
}

// leastLoaded is the index of the smallest count, ties to the lowest index.
func leastLoaded(counts [Cohorts]int) uint32 {
	best := 0

	for i := 1; i < Cohorts; i++ {
		if counts[i] < counts[best] {
			best = i
		}
	}

	return uint32(best)
}

// declaredZoneKey is the name a declared zone is interned under. The site is
// part of it because zones never cross sites, so two sites may use the same
// zone names without being merged into one failure domain.
//
// The site is escaped rather than joined raw: a site whose name contained the
// separator would otherwise intern to the same key as a differently split pair,
// and two failure domains sharing a zone id is a silent merge.
func declaredZoneKey(site, name string) string {
	return url.QueryEscape(site) + "|" + name
}

// SelectGateways picks the members of a zone that other zones may route
// through.
//
// Gateways are how much a zone's edge overlaps its neighbours, and the choice
// is not arbitrary. Bridge nodes come first: a member sitting on a fabric other
// than its zone's own is one RDMA hop from everything on that other fabric, so
// routing a neighbour's traffic through it is the difference between a fabric
// crossing and none. After that the list is taken round-robin across the
// cohorts, which are availability zones once a zone has three of them, so
// losing one availability zone leaves gateways in the others. Within a cohort
// the lowest node id wins, which makes the list stable as the zone churns.
//
// count is the width, zero meaning DefaultGatewayCount, capped at MaxGateways
// by the schema. The result is sorted, since the annotation is compared as a
// string and the dataplane ranks the list by rendezvous hash regardless.
func SelectGateways(members Membership, bridge map[uint32]bool, count int) []uint32 {
	if count <= 0 {
		count = DefaultGatewayCount
	}

	if count > MaxGateways {
		count = MaxGateways
	}

	cohorts := members.ByCohort()

	var bridges, rest [Cohorts][]uint32

	for cohort := range Cohorts {
		for _, id := range cohorts[cohort] {
			if bridge[id] {
				bridges[cohort] = append(bridges[cohort], id)

				continue
			}

			rest[cohort] = append(rest[cohort], id)
		}
	}

	picked := make([]uint32, 0, count)
	for _, tier := range [][Cohorts][]uint32{bridges, rest} {
		picked = roundRobin(picked, tier, count)
	}

	sort.Slice(picked, func(i, j int) bool { return picked[i] < picked[j] })

	return picked
}

// roundRobin appends from each cohort in turn until the cohorts run out or the
// list reaches count.
func roundRobin(into []uint32, cohorts [Cohorts][]uint32, count int) []uint32 {
	for len(into) < count {
		took := false

		for cohort := range Cohorts {
			if len(into) == count {
				return into
			}

			if len(cohorts[cohort]) == 0 {
				continue
			}

			into = append(into, cohorts[cohort][0])
			cohorts[cohort] = cohorts[cohort][1:]
			took = true
		}

		if !took {
			return into
		}
	}

	return into
}

// PlacementDrift reports a node whose site or availability zone no longer
// matches the placement it was given.
//
// Placement is never revisited: a node's zone and cohort are frozen the moment
// they are stamped, because both appear in a catalog whose membership moves one
// node at a time, and because a cohort is a node's column in every trio it
// holds. So drift is reported and not acted on. The remedy is to decommission
// the node and let it back in again, which is a deliberate act with a known
// cost.
//
// A node's fabric is deliberately not drift. A member of a zone seeded on
// another fabric is a bridge node, which is a shape placement produces on
// purpose, so a fabric that changes to or from the zone's own is a routing
// preference changing and nothing more.
func PlacementDrift(node PlacementNode, placed Placement, def ZoneDef, buckets map[string]uint32) string {
	if node.Site != def.Site {
		return fmt.Sprintf("node %s is in site %q but its zone %d was placed for site %q",
			node.Name, node.Site, placed.Zone, def.Site)
	}

	if bucket, ok := buckets[node.AZ]; ok && bucket != placed.Cohort && len(buckets) >= Cohorts {
		return fmt.Sprintf("node %s is in availability zone %q, which zone %d holds in cohort %d, but the node is in cohort %d",
			node.Name, node.AZ, placed.Zone, bucket, placed.Cohort)
	}

	return ""
}
