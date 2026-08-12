// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package racerctrl

import (
	"fmt"
	"sort"

	racerconfig "github.com/Azure/unbounded/api/racer"
)

// Validation.
//
// R5 says the control plane validates before it publishes: a config that racer
// would reject must never reach a node's watched directory, because a rejected
// config leaves that node running the previous generation while every other node
// moves on, and a cluster split across two generations is exactly the state the
// membership rules exist to prevent.
//
// So this is a port of racer's own validate(), kept deliberately close to the
// original. It is not a superset and not a subset: rules the dataplane enforces
// are enforced here in the same terms, so a config that passes here passes there,
// and the error text is close enough that a failure found in a control plane test
// reads the same as a failure found on a node.
//
// The one thing it cannot check is agreement between nodes. Validate sees one
// node's config; ValidateTransition sees one node's history. Cross-node
// consistency is a property of derivation, not of validation.

// Validate checks a rendered NodeConfig against every rule racer will apply to
// it. A nil error means the dataplane will accept it.
func Validate(cfg *racerconfig.NodeConfig) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}

	if err := validateNode(cfg.GetNode()); err != nil {
		return err
	}

	if err := validateUniverses(cfg); err != nil {
		return err
	}

	if err := validateDevices(cfg); err != nil {
		return err
	}

	return validatePolicy(cfg)
}

func validateNode(node *racerconfig.Node) error {
	if node == nil {
		return fmt.Errorf("node is not set")
	}

	if node.GetId() == 0 {
		return fmt.Errorf("node id must not be zero")
	}

	if node.GetZone() == 0 {
		return fmt.Errorf("node zone must not be zero")
	}

	// Cohort is optional in the schema so that a missing cohort is visible as a
	// missing cohort rather than as COHORT_0. A node's position in a trio is
	// normative, so defaulting it would silently place the node in the wrong
	// replica slot.
	if node.Cohort == nil {
		return fmt.Errorf("node cohort must be set explicitly")
	}

	switch node.GetCohort() {
	case racerconfig.Cohort_COHORT_0, racerconfig.Cohort_COHORT_1, racerconfig.Cohort_COHORT_2:
	default:
		return fmt.Errorf("node cohort %d is out of range", node.GetCohort())
	}

	if node.GetStore().GetSizeBytes() == 0 {
		return fmt.Errorf("node store size must not be zero")
	}

	return nil
}

func validateUniverses(cfg *racerconfig.NodeConfig) error {
	universes := cfg.GetUniverses()
	if len(universes) == 0 {
		return fmt.Errorf("config names no universes")
	}

	seenUniverse := make(map[uint32]struct{}, len(universes))
	seenExtent := make(map[uint32]struct{})

	totalExtents := 0

	for _, universe := range universes {
		id := universe.GetId()

		if id == 0 {
			return fmt.Errorf("universe id must not be zero")
		}

		if uint64(id) >= MaxUniverse {
			return fmt.Errorf("universe %d is not below 2^26", id)
		}

		if _, dup := seenUniverse[id]; dup {
			return fmt.Errorf("universe %d appears twice", id)
		}

		seenUniverse[id] = struct{}{}

		if err := validateUniverse(cfg.GetNode(), universe, seenExtent); err != nil {
			return fmt.Errorf("universe %d: %w", id, err)
		}

		totalExtents += len(universe.GetExtents())
	}

	if totalExtents > MaxExtents {
		return fmt.Errorf(
			"config names %d extents, more than the %d the per-extent metrics table holds",
			totalExtents, MaxExtents,
		)
	}

	return nil
}

func validateUniverse(node *racerconfig.Node, universe *racerconfig.Universe, seenExtent map[uint32]struct{}) error {
	if err := validateCatalog(universe); err != nil {
		return err
	}

	zones, err := validateZones(node, universe)
	if err != nil {
		return err
	}

	if err := validatePeers(node, universe); err != nil {
		return err
	}

	return validateExtents(node, universe, zones, seenExtent)
}

func validateCatalog(universe *racerconfig.Universe) error {
	catalog := universe.GetCatalog()
	if len(catalog) == 0 {
		return fmt.Errorf("catalog is empty")
	}

	counts := make(map[uint32]int)

	for i, trio := range catalog {
		members := [Cohorts]uint32{trio.GetCohort_0(), trio.GetCohort_1(), trio.GetCohort_2()}

		for cohort, member := range members {
			if member == 0 {
				return fmt.Errorf("catalog group %d cohort %d is zero", i, cohort)
			}
		}

		if members[0] == members[1] || members[1] == members[2] || members[0] == members[2] {
			return fmt.Errorf("catalog group %d names the same node twice", i)
		}

		for _, member := range members {
			counts[member]++
		}
	}

	// R3's balance rule. Every named node holds exactly the same share of the
	// groups, so a lost node costs every survivor the same amount of replay.
	// Stated as a divisibility requirement because an unbalanced catalog has no
	// correct answer, only a least-bad one.
	total := Cohorts * len(catalog)
	if total%len(counts) != 0 {
		return fmt.Errorf(
			"catalog names %d nodes across %d groups; %d does not divide %d",
			len(counts), len(catalog), len(counts), total,
		)
	}

	share := total / len(counts)

	ids := make([]uint32, 0, len(counts))
	for id := range counts {
		ids = append(ids, id)
	}

	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	for _, id := range ids {
		if counts[id] != share {
			return fmt.Errorf(
				"node %d holds %d of %d groups, not the balanced share of %d",
				id, counts[id], len(catalog), share,
			)
		}
	}

	return nil
}

func validateZones(node *racerconfig.Node, universe *racerconfig.Universe) (map[uint32]struct{}, error) {
	zones := universe.GetZones()
	if len(zones) > MaxZones {
		return nil, fmt.Errorf("universe names %d zones, more than %d", len(zones), MaxZones)
	}

	peers := make(map[uint32]struct{}, len(universe.GetPeers()))
	for _, peer := range universe.GetPeers() {
		peers[peer.GetId()] = struct{}{}
	}

	known := map[uint32]struct{}{node.GetZone(): {}}

	for _, zone := range zones {
		if zone.GetId() == 0 {
			return nil, fmt.Errorf("zone id must not be zero")
		}

		if zone.GetId() == node.GetZone() {
			return nil, fmt.Errorf("zone %d is our own zone and must not be listed", zone.GetId())
		}

		if _, dup := known[zone.GetId()]; dup {
			return nil, fmt.Errorf("zone %d appears twice", zone.GetId())
		}

		known[zone.GetId()] = struct{}{}

		gateways := zone.GetGateways()
		if len(gateways) == 0 {
			return nil, fmt.Errorf("zone %d names no gateways", zone.GetId())
		}

		if len(gateways) > MaxGateways {
			return nil, fmt.Errorf(
				"zone %d names %d gateways, more than %d",
				zone.GetId(), len(gateways), MaxGateways,
			)
		}

		reachable := false

		for _, gateway := range gateways {
			if gateway == 0 {
				return nil, fmt.Errorf("zone %d names gateway zero", zone.GetId())
			}

			if _, ok := peers[gateway]; ok {
				reachable = true
			}
		}

		// A gateway we hold no fabric link to is a gateway we cannot reach. At
		// least one has to be attached or every cross-zone read from this node
		// falls back, which is the failure racer_gateway_fallback_total counts.
		if !reachable {
			return nil, fmt.Errorf("zone %d names no gateway that is also a peer", zone.GetId())
		}
	}

	return known, nil
}

func validatePeers(node *racerconfig.Node, universe *racerconfig.Universe) error {
	seen := make(map[uint32]struct{}, len(universe.GetPeers()))

	for _, peer := range universe.GetPeers() {
		if peer.GetId() == 0 {
			return fmt.Errorf("peer id must not be zero")
		}

		if peer.GetId() == node.GetId() {
			return fmt.Errorf("peer %d is ourselves", peer.GetId())
		}

		if _, dup := seen[peer.GetId()]; dup {
			return fmt.Errorf("peer %d appears twice", peer.GetId())
		}

		seen[peer.GetId()] = struct{}{}

		if peer.GetDevice() == "" {
			return fmt.Errorf("peer %d has no device path", peer.GetId())
		}
	}

	// R5: a universe meant to be replicated has non-empty peers. A catalog with
	// more than one distinct node means this node cannot serve every group
	// alone, so with no peers every group it does not hold is unavailable.
	//
	// The exception is a universe that carries nothing: no zones to route to and
	// no extents to serve. That is the bootstrap shape a node publishes to get
	// racer to create the universe's fabric device, which is the device its
	// peers are attached from and so the thing that has to exist before there
	// can be any peers at all. It answers no reads, so there is nothing for a
	// missing peer to make unavailable.
	if nodes := catalogNodeCount(universe.GetCatalog()); len(seen) == 0 && nodes > 1 {
		if len(universe.GetExtents()) > 0 || len(universe.GetZones()) > 0 {
			return fmt.Errorf("catalog names %d nodes but the universe has no peers", nodes)
		}
	}

	return nil
}

func validateExtents(
	node *racerconfig.Node,
	universe *racerconfig.Universe,
	zones map[uint32]struct{},
	seenExtent map[uint32]struct{},
) error {
	extents := universe.GetExtents()

	var previousEnd uint64

	for i, extent := range extents {
		id := extent.GetId()

		if id == 0 {
			return fmt.Errorf("extent id must not be zero")
		}

		// Extent ids come from one global space, so uniqueness is checked across
		// every universe in the config, not just within this one.
		if _, dup := seenExtent[id]; dup {
			return fmt.Errorf("extent %d appears twice", id)
		}

		seenExtent[id] = struct{}{}

		if extent.GetPages() == 0 {
			return fmt.Errorf("extent %d has no pages", id)
		}

		huge := KindIsHuge(extent.GetKind())

		if huge && extent.GetBaseLba()%HugeBlocks != 0 {
			return fmt.Errorf(
				"extent %d is IMMUTABLE_4M with base_lba %d, not a multiple of %d",
				id, extent.GetBaseLba(), uint64(HugeBlocks),
			)
		}

		span := extent.GetPages()
		if huge {
			span *= HugeBlocks
		}

		end := extent.GetBaseLba() + span
		if end > MaxLBA || end < extent.GetBaseLba() {
			return fmt.Errorf("extent %d spans past 2^38 blocks", id)
		}

		// Sorted and non-overlapping. Sorted is not cosmetic: racer resolves an
		// address by binary search over this list.
		if i > 0 && extent.GetBaseLba() < previousEnd {
			return fmt.Errorf(
				"extent %d starts at %d, before the previous extent ends at %d",
				id, extent.GetBaseLba(), previousEnd,
			)
		}

		previousEnd = end

		if err := validateExtentZones(node, extent, zones); err != nil {
			return err
		}

		if err := validateExtentPolicy(extent); err != nil {
			return err
		}
	}

	return nil
}

func validateExtentZones(node *racerconfig.Node, extent *racerconfig.Extent, zones map[uint32]struct{}) error {
	id := extent.GetId()

	if extent.GetZone() == 0 {
		return fmt.Errorf("extent %d has zone zero", id)
	}

	if _, ok := zones[extent.GetZone()]; !ok {
		return fmt.Errorf(
			"extent %d has home zone %d, which is neither our zone %d nor one the universe names",
			id, extent.GetZone(), node.GetZone(),
		)
	}

	if next := extent.GetNextZone(); next != 0 {
		if next == extent.GetZone() {
			return fmt.Errorf("extent %d has next_zone equal to zone %d", id, next)
		}

		if _, ok := zones[next]; !ok {
			return fmt.Errorf("extent %d has next_zone %d, which the universe does not name", id, next)
		}
	}

	return nil
}

func validateExtentPolicy(extent *racerconfig.Extent) error {
	id := extent.GetId()

	if extent.GetCacheAdmit() > MaxCacheAdmit {
		return fmt.Errorf("extent %d has cache_admit %d, above %d", id, extent.GetCacheAdmit(), MaxCacheAdmit)
	}

	warm := extent.GetWarmZones()
	if len(warm) == 0 {
		return nil
	}

	// Warming pushes pages to a zone that does not own them. That is only sound
	// when the pages cannot change under the copy, so it is confined to the
	// immutable kinds.
	if !KindIsImmutable(extent.GetKind()) {
		return fmt.Errorf("extent %d is %s and must not name warm zones", id, extent.GetKind())
	}

	if len(warm) > MaxWarmZones {
		return fmt.Errorf("extent %d names %d warm zones, more than %d", id, len(warm), MaxWarmZones)
	}

	seen := make(map[uint32]struct{}, len(warm))

	for _, zone := range warm {
		if zone == 0 {
			return fmt.Errorf("extent %d names warm zone zero", id)
		}

		if zone == extent.GetZone() {
			return fmt.Errorf("extent %d names its own home zone %d as a warm zone", id, zone)
		}

		if zone == extent.GetNextZone() {
			return fmt.Errorf("extent %d names its next_zone %d as a warm zone", id, zone)
		}

		if _, dup := seen[zone]; dup {
			return fmt.Errorf("extent %d names warm zone %d twice", id, zone)
		}

		seen[zone] = struct{}{}
	}

	return nil
}

func validateDevices(cfg *racerconfig.NodeConfig) error {
	extents := make(map[uint32]*racerconfig.Extent)

	for _, universe := range cfg.GetUniverses() {
		for _, extent := range universe.GetExtents() {
			extents[extent.GetId()] = extent
		}
	}

	// The exported-device budget covers both kinds of export: one ublk device per
	// configured device and one per universe for that universe's fabric
	// namespace.
	exports := len(cfg.GetUniverses()) + len(cfg.GetDevices())
	if exports > MaxExports {
		return fmt.Errorf(
			"config exports %d devices (%d universes plus %d devices), more than %d",
			exports, len(cfg.GetUniverses()), len(cfg.GetDevices()), MaxExports,
		)
	}

	minors := make(map[uint32]string, exports)

	for _, universe := range cfg.GetUniverses() {
		minor := universe.GetFabricDeviceId()
		if minor == 0 {
			return fmt.Errorf("universe %d has no fabric device id", universe.GetId())
		}

		if owner, dup := minors[minor]; dup {
			return fmt.Errorf("device id %d is claimed by both %s and universe %d fabric", minor, owner, universe.GetId())
		}

		minors[minor] = fmt.Sprintf("universe %d fabric", universe.GetId())
	}

	for _, device := range cfg.GetDevices() {
		id := device.GetId()

		if id == 0 {
			return fmt.Errorf("device id must not be zero")
		}

		if owner, dup := minors[id]; dup {
			return fmt.Errorf("device id %d is claimed by both %s and device %d", id, owner, id)
		}

		minors[id] = fmt.Sprintf("device %d", id)

		if len(device.GetExtents()) == 0 {
			return fmt.Errorf("device %d names no extents", id)
		}

		seen := make(map[uint32]struct{}, len(device.GetExtents()))

		for _, extentID := range device.GetExtents() {
			if extentID == 0 {
				return fmt.Errorf("device %d names extent zero", id)
			}

			if _, dup := seen[extentID]; dup {
				return fmt.Errorf("device %d names extent %d twice", id, extentID)
			}

			seen[extentID] = struct{}{}

			if _, ok := extents[extentID]; !ok {
				return fmt.Errorf("device %d names extent %d, which this config does not carry", id, extentID)
			}
		}
	}

	return nil
}

func validatePolicy(cfg *racerconfig.NodeConfig) error {
	policy := cfg.GetPolicy()
	if policy == nil || policy.MaxIndexBytes == nil {
		return nil
	}

	// The index holds one entry per 4 KiB page the node could be asked to
	// address. Sizing it below that is not a degradation, it is a config racer
	// will refuse, so the control plane refuses it first.
	var pages uint64

	for _, universe := range cfg.GetUniverses() {
		for _, extent := range universe.GetExtents() {
			if !KindIsHuge(extent.GetKind()) {
				pages += extent.GetPages()
			}
		}
	}

	needed := pages * IndexEntryBytes
	if policy.GetMaxIndexBytes() < needed {
		return fmt.Errorf(
			"policy.max_index_bytes is %d but %d 4 KiB pages need %d at %d bytes each",
			policy.GetMaxIndexBytes(), pages, needed, uint64(IndexEntryBytes),
		)
	}

	if policy.RepairsPerReplay != nil && policy.GetRepairsPerReplay() == 0 {
		return fmt.Errorf("policy.repairs_per_replay must not be zero")
	}

	return nil
}

// ValidateTransition checks the rules that only make sense against the config a
// node is already running. R5's monotonicity rules live here, along with R3's
// "frozen for life" rules, because a fact about a single config cannot tell you
// whether a value moved backwards.
//
// prev may be nil, meaning this is the node's first config.
func ValidateTransition(prev, next *racerconfig.NodeConfig) error {
	if next == nil {
		return fmt.Errorf("config is nil")
	}

	if prev == nil {
		return nil
	}

	if next.GetGeneration() <= prev.GetGeneration() {
		return fmt.Errorf(
			"generation %d does not advance past %d",
			next.GetGeneration(), prev.GetGeneration(),
		)
	}

	if next.GetNode().GetId() != prev.GetNode().GetId() {
		return fmt.Errorf(
			"node id changed from %d to %d",
			prev.GetNode().GetId(), next.GetNode().GetId(),
		)
	}

	if next.GetNode().GetCohort() != prev.GetNode().GetCohort() {
		return fmt.Errorf(
			"node cohort changed from %s to %s",
			prev.GetNode().GetCohort(), next.GetNode().GetCohort(),
		)
	}

	// R4: never lower size_bytes. The store is formatted for a size; shrinking it
	// would strand the allocator's tail.
	if next.GetNode().GetStore().GetSizeBytes() < prev.GetNode().GetStore().GetSizeBytes() {
		return fmt.Errorf(
			"store size dropped from %d to %d",
			prev.GetNode().GetStore().GetSizeBytes(),
			next.GetNode().GetStore().GetSizeBytes(),
		)
	}

	// A step is the transition between consecutive generations. Anything wider is
	// a node that missed generations, and the schema holds such a node to no
	// per-step rule: it is being handed a settled state, not a step.
	step := next.GetGeneration() == prev.GetGeneration()+1

	if err := validateUniverseTransitions(prev, next, step); err != nil {
		return err
	}

	return validateDeviceTransitions(prev, next)
}

func validateUniverseTransitions(prev, next *racerconfig.NodeConfig, step bool) error {
	before := indexUniverses(prev)

	for _, universe := range next.GetUniverses() {
		old, ok := before[universe.GetId()]
		if !ok {
			continue
		}

		if universe.GetEpoch() < old.GetEpoch() {
			return fmt.Errorf(
				"universe %d epoch went backwards from %d to %d",
				universe.GetId(), old.GetEpoch(), universe.GetEpoch(),
			)
		}

		if len(universe.GetCatalog()) != len(old.GetCatalog()) {
			return fmt.Errorf(
				"universe %d catalog resized from %d groups to %d; len(catalog) is fixed for the life of a zone",
				universe.GetId(), len(old.GetCatalog()), len(universe.GetCatalog()),
			)
		}

		if step {
			if err := validateMembershipStep(old, universe); err != nil {
				return err
			}
		}

		if err := validateExtentTransitions(old, universe); err != nil {
			return err
		}
	}

	return nil
}

// validateMembershipStep enforces R6's membership rule: between consecutive
// generations a catalog's membership changes by at most one id. The dataplane
// heals by handing one node's groups to one other node; two changes at once
// would leave a group with no surviving replica to replay from.
func validateMembershipStep(old, next *racerconfig.Universe) error {
	before := catalogMembers(old)
	after := catalogMembers(next)

	var joined, left int

	for id := range after {
		if _, ok := before[id]; !ok {
			joined++
		}
	}

	for id := range before {
		if _, ok := after[id]; !ok {
			left++
		}
	}

	if joined > 1 || left > 1 {
		return fmt.Errorf(
			"universe %d catalog changed by %d joins and %d departures in one generation; at most one of each is allowed",
			next.GetId(), joined, left,
		)
	}

	return nil
}

// TransitionStride is how far a node's generation has to advance to get from
// prev to next.
//
// Consecutive generations are a step, and a step is held to R6's one-in-one-out
// rule because the dataplane heals a step by handing one node's groups to one
// other node. A wider change is not a transient it can reason about, so it is
// delivered as a settled state instead, by skipping a generation: a node that
// missed a generation is being told where the universe ended up rather than how
// it got there.
//
// A catalog resize is the case that needs it. Its move is one node per cohort,
// which is three joins and three departures at once, and publishing that at the
// next generation is rejected by both this package and the dataplane, so the
// catalog would never resize at all.
//
// The decision is made by asking the step validator, so the stride and the rule
// it exists to satisfy cannot drift apart.
func TransitionStride(prev, next *racerconfig.NodeConfig) uint64 {
	if prev == nil {
		return 1
	}

	before := indexUniverses(prev)

	for _, universe := range next.GetUniverses() {
		old, ok := before[universe.GetId()]
		if !ok {
			continue
		}

		if validateMembershipStep(old, universe) != nil {
			return 2
		}
	}

	return 1
}

func validateExtentTransitions(old, next *racerconfig.Universe) error {
	before := make(map[uint32]*racerconfig.Extent, len(old.GetExtents()))
	for _, extent := range old.GetExtents() {
		before[extent.GetId()] = extent
	}

	for _, extent := range next.GetExtents() {
		previous, ok := before[extent.GetId()]
		if !ok {
			continue
		}

		// Geometry is frozen for the extent's life: base_lba, pages and kind are
		// baked into every page register that ever addressed it.
		if extent.GetBaseLba() != previous.GetBaseLba() {
			return fmt.Errorf(
				"extent %d moved from base_lba %d to %d",
				extent.GetId(), previous.GetBaseLba(), extent.GetBaseLba(),
			)
		}

		if extent.GetPages() != previous.GetPages() {
			return fmt.Errorf(
				"extent %d resized from %d pages to %d",
				extent.GetId(), previous.GetPages(), extent.GetPages(),
			)
		}

		if extent.GetKind() != previous.GetKind() {
			return fmt.Errorf(
				"extent %d changed kind from %s to %s",
				extent.GetId(), previous.GetKind(), extent.GetKind(),
			)
		}

		// Collection is destructive. A tombstone_epoch that went backwards would
		// resurrect registers the cluster has already agreed are dead.
		if extent.GetTombstoneEpoch() < previous.GetTombstoneEpoch() {
			return fmt.Errorf(
				"extent %d tombstone_epoch went backwards from %d to %d",
				extent.GetId(), previous.GetTombstoneEpoch(), extent.GetTombstoneEpoch(),
			)
		}

		if err := validateHomeZoneMove(previous, extent); err != nil {
			return err
		}
	}

	return nil
}

// validateHomeZoneMove enforces R5's rule that a home zone only ever moves to
// the value next_zone previously held. Migration is a two-step: the control
// plane sets next_zone to start it, and declares it complete by promoting that
// same value into zone. A move to anywhere else means the control plane declared
// a migration complete that it never started.
func validateHomeZoneMove(previous, extent *racerconfig.Extent) error {
	if extent.GetZone() == previous.GetZone() {
		return nil
	}

	if previous.GetNextZone() == 0 {
		return fmt.Errorf(
			"extent %d home zone moved from %d to %d with no migration in flight",
			extent.GetId(), previous.GetZone(), extent.GetZone(),
		)
	}

	if extent.GetZone() != previous.GetNextZone() {
		return fmt.Errorf(
			"extent %d home zone moved from %d to %d, but the migration in flight targeted %d",
			extent.GetId(), previous.GetZone(), extent.GetZone(), previous.GetNextZone(),
		)
	}

	if extent.GetNextZone() != 0 {
		return fmt.Errorf(
			"extent %d completed a migration to %d but still names next_zone %d",
			extent.GetId(), extent.GetZone(), extent.GetNextZone(),
		)
	}

	return nil
}

func validateDeviceTransitions(prev, next *racerconfig.NodeConfig) error {
	before := make(map[uint32][]uint32, len(prev.GetDevices()))
	for _, device := range prev.GetDevices() {
		before[device.GetId()] = device.GetExtents()
	}

	for _, device := range next.GetDevices() {
		previous, ok := before[device.GetId()]
		if !ok {
			continue
		}

		// A device is a view, and its extent list is its geometry. Changing it
		// would move every block past the change under an open file descriptor.
		if !sameUint32Slice(previous, device.GetExtents()) {
			return fmt.Errorf(
				"device %d changed its extent list from %v to %v; it is frozen once the device exists",
				device.GetId(), previous, device.GetExtents(),
			)
		}
	}

	return nil
}

func indexUniverses(cfg *racerconfig.NodeConfig) map[uint32]*racerconfig.Universe {
	index := make(map[uint32]*racerconfig.Universe, len(cfg.GetUniverses()))
	for _, universe := range cfg.GetUniverses() {
		index[universe.GetId()] = universe
	}

	return index
}

func catalogMembers(universe *racerconfig.Universe) map[uint32]struct{} {
	members := make(map[uint32]struct{})

	for _, trio := range universe.GetCatalog() {
		members[trio.GetCohort_0()] = struct{}{}
		members[trio.GetCohort_1()] = struct{}{}
		members[trio.GetCohort_2()] = struct{}{}
	}

	return members
}

func sameUint32Slice(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}
