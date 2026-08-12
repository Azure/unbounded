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

// DefaultCatalogSize is the catalog length a zone is pinned to when its universe
// is first published. It is fixed for the life of the zone, and the number of
// nodes per cohort must divide it, so the useful property is having many
// divisors rather than being large: 360 admits cohort sizes 1, 2, 3, 4, 5, 6, 8,
// 9, 10, 12, 15, 18, 20, 24, 30, 36, 40, 45, 60, 72, 90, 120, 180 and 360, which
// is 3 to 1080 nodes in a zone.
const DefaultCatalogSize = 360
