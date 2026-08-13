// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package racerctrl

// Limits transcribed from the dataplane. Every one of these is enforced by
// cmd/racer/src/config.rs on load, so a control plane that exceeds one produces
// a config the node refuses whole, with the rejected-configs counter as the only
// signal. We check them here so that never happens.
const (
	// SmallPage is the 4 KiB page every kind but IMMUTABLE_4M is stored in, and
	// the block size a universe address counts in.
	SmallPage = 4096

	// HugePage is the 4 MiB page IMMUTABLE_4M is stored in.
	HugePage = 4 << 20

	// HugeBlocks is how many universe blocks one huge page spans. A huge extent's
	// base_lba must be a multiple of it.
	HugeBlocks = HugePage / SmallPage

	// MaxUniverse bounds a universe id: an address is `universe:26 | lba:38`.
	MaxUniverse = 1 << 26

	// MaxLBA bounds `base_lba + pages * blocks_per_page` within one universe,
	// which is 2^38 4 KiB blocks, or 1 PiB.
	MaxLBA = 1 << 38

	// MaxExports caps `len(universes) + len(devices)` on one node, because each
	// costs one exported ublk device. This, not MaxExtents, is what limits how
	// many volumes a node can serve.
	MaxExports = 256

	// MaxExtents caps the extents one node may be told about, because the
	// per-extent metrics table is statically sized to it.
	MaxExtents = 1024

	// MaxZones caps a universe's foreign zone list.
	MaxZones = 64

	// MaxGateways caps one zone's gateway list.
	MaxGateways = 64

	// MaxWarmZones caps an extent's warm zone list.
	MaxWarmZones = 16

	// MaxCacheAdmit is the largest cache admission class.
	MaxCacheAdmit = 15

	// IndexEntryBytes is what one 4 KiB page costs in the index, which is what
	// Policy.max_index_bytes has to admit.
	IndexEntryBytes = 52
)

// Cohorts is how many cohorts a catalog trio has, one node from each.
const Cohorts = 3

// Quorum is how many of a group's three nodes have to agree, and so how many
// have to survive a membership change. Two of three can serve every version the
// group ever agreed and replay the third from what they hold.
const Quorum = Cohorts/2 + 1

// DefaultCatalogSize is the catalog length a zone is pinned to when its universe
// is first published. It is fixed for the life of the zone, and the number of
// nodes per cohort must divide it, so the useful property is having many
// divisors rather than being large.
//
// 2520 is 2^3 * 3^2 * 5 * 7, which has 48 divisors. The ones that matter are the
// large ones: a zone filling toward the 1000-node target admits cohort sizes 280,
// 315 and 360, so it wastes at most a handful of nodes on the way up rather than
// the 46% that 360's jump from 180 to 360 costs. At full stretch it is 2520 nodes
// per cohort, well past the zone target.
//
// A larger catalog means more consensus groups per node, and the dataplane's
// superblock only records the promised term of 128 of them (MAX_TERMS in
// cmd/racer/src/layout.rs). That table is a cache, not a ledger: paxos::persist
// truncates it silently, a group missing from it starts at term zero, and the
// real term is recovered from the group's peers on first use. Partial coverage
// is already the case at 360, so this does not change in kind.
const DefaultCatalogSize = 2520

// Placement tuning. None of these are dataplane limits; they are the knobs the
// operator uses to decide which zone a node lands in and how much two zones
// overlap.
const (
	// DefaultZoneTarget is how many nodes a zone is filled to before a new one
	// is minted. A zone is a single catalog, a single anti-entropy domain and a
	// single blast radius, so it is sized for the failure the operator wants to
	// survive rather than for the largest catalog that would fit.
	DefaultZoneTarget = 1000

	// MinZoneSeed is how many unplaced nodes must share a fabric before that
	// fabric is worth a zone of its own. Below it the nodes join an existing
	// zone instead, because a zone of one or two nodes cannot form a trio and
	// would hold no groups at all.
	MinZoneSeed = 3

	// DefaultGatewayCount is how many of a zone's members other zones may route
	// through, and so how much two zones overlap.
	//
	// Six because the dataplane walks at most three gateways before calling a
	// zone unavailable (GATEWAY_TRIES in cmd/racer/src/paxos.rs), so three is
	// the useful width and six leaves headroom for half the list being
	// unreachable. Wider is not free: every node holds an NVMe-oF controller per
	// gateway per foreign zone, so the cost is (zones-1) * count per node.
	DefaultGatewayCount = 6
)
