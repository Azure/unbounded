// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package racerctrl

import (
	"math"

	racerconfig "github.com/Azure/unbounded/api/racer"
)

// Store layout constants, transcribed from cmd/racer/src/layout.rs. The store is
// formatted from these, so a control plane that computes a size from anything
// else hands the node a store it cannot fit its share into, and the node reports
// unbacked pages rather than failing.
const (
	// metaBlock is the size of one metadata block.
	metaBlock = 4096

	// smallPagesPerMblock is how many 4 KiB data pages one metadata block covers.
	smallPagesPerMblock = 112

	// hugePagesPerMblock is how many 4 MiB data pages one metadata block covers.
	hugePagesPerMblock = 126

	// superblockRegion is the four superblock copies at the head of the store.
	superblockRegion = 4 * 4096

	// zeroRegion is the shared all-zero page the layout keeps for dedup.
	zeroRegion = HugePage

	// metaBlocksPerEntry is how many metadata blocks each accounted block costs;
	// the layout keeps two copies.
	metaBlocksPerEntry = 2

	// smallFloorPages and hugeFloorPages are the per-class floors the layout adds
	// on top of the proportional overprovision, so a tiny share still gets enough
	// slack to relocate within.
	smallFloorPages = 64
	hugeFloorPages  = 4

	// overprovisionDivisor makes the proportional slack 5%.
	overprovisionDivisor = 20

	// defaultCacheTailDivisor reserves a tenth of the allocated region again as
	// the read cache, which lives in whatever the store has past the allocator.
	// The dataplane will run with no tail at all; it simply never caches.
	defaultCacheTailDivisor = 10
)

// StorePages is the share of a zone's pages one node has to be able to hold, in
// pages of each class.
type StorePages struct {
	// Small is 4 KiB pages.
	Small uint64

	// Huge is 4 MiB pages.
	Huge uint64
}

// CountStorePages computes a node's share of the pages in the universes it has
// joined.
//
// A zone's nodes are homogeneous and its catalog is balanced, so a node's share
// is simply the zone's pages replicated three ways and split evenly across the
// nodes named in the catalog. Extents migrating into the zone are counted from
// the moment the migration starts, because both ends hold the pages until the
// control plane declares the move complete.
func CountStorePages(zone uint32, universes []*racerconfig.Universe) StorePages {
	var total StorePages

	for _, universe := range universes {
		nodes := catalogNodeCount(universe.GetCatalog())
		if nodes == 0 {
			nodes = 1
		}

		var small, huge uint64

		for _, extent := range universe.GetExtents() {
			if extent.GetZone() != zone && extent.GetNextZone() != zone {
				continue
			}

			if KindIsHuge(extent.GetKind()) {
				huge += extent.GetPages()

				continue
			}

			small += extent.GetPages()
		}

		total.Small += divCeil(small*Cohorts, nodes)
		total.Huge += divCeil(huge*Cohorts, nodes)
	}

	return total
}

// catalogNodeCount is how many distinct nodes a catalog names.
func catalogNodeCount(catalog []*racerconfig.Trio) uint64 {
	seen := make(map[uint32]struct{}, len(catalog)*Cohorts)

	for _, trio := range catalog {
		seen[trio.GetCohort_0()] = struct{}{}
		seen[trio.GetCohort_1()] = struct{}{}
		seen[trio.GetCohort_2()] = struct{}{}
	}

	delete(seen, 0)

	return uint64(len(seen))
}

// overprovision adds the layout's slack to a page count: five percent plus a
// per-class floor. A store sized to the bare page count has nowhere to relocate
// a page to and wedges the allocator.
func overprovision(pages, floor uint64) uint64 {
	if pages == 0 {
		return 0
	}

	return pages + pages/overprovisionDivisor + floor
}

// metaBlocksWanted is how many metadata blocks of each class the layout needs to
// cover a node's share.
func metaBlocksWanted(pages StorePages) (small, huge uint64) {
	return divCeil(overprovision(pages.Small, smallFloorPages), smallPagesPerMblock),
		divCeil(overprovision(pages.Huge, hugeFloorPages), hugePagesPerMblock)
}

// StoreSizeBytes is the store size that holds a node's share, including the
// layout's own overhead and a read cache tail.
//
// The store is cold: racer formats or grows it at startup and a change takes
// effect on the next process start, never on a reload. It also never shrinks -
// see GrowStore.
func StoreSizeBytes(pages StorePages) uint64 {
	allocEnd := allocatorEndBytes(pages)

	tail := allocEnd / defaultCacheTailDivisor
	if tail < HugePage {
		tail = HugePage
	}

	return alignUp(allocEnd+tail, HugePage)
}

// allocatorEndBytes walks the same layout cmd/racer/src/layout.rs plans, and
// returns the first byte past the allocator. Everything after it is the read
// cache.
func allocatorEndBytes(pages StorePages) uint64 {
	smallMblocks, hugeMblocks := metaBlocksWanted(pages)

	at := uint64(superblockRegion)
	at += zeroRegion
	at += smallMblocks * metaBlocksPerEntry * metaBlock
	at += hugeMblocks * metaBlocksPerEntry * metaBlock

	at = alignUp(at, SmallPage)
	at += smallMblocks * smallPagesPerMblock * SmallPage

	at = alignUp(at, HugePage)
	at += hugeMblocks * hugePagesPerMblock * HugePage

	return at
}

// GrowStore is the store size to publish given what the node has already
// formatted for.
//
// Store size only ever rises. Shrinking it would ask the node to give back space
// it has already put pages in, and the layout has no way to do that: the size is
// baked into the superblock at format time and grow_if_needed only extends.
func GrowStore(current, wanted uint64) uint64 {
	if current > wanted {
		return current
	}

	return wanted
}

func divCeil(numerator, denominator uint64) uint64 {
	if denominator == 0 {
		return 0
	}

	return (numerator + denominator - 1) / denominator
}

// alignUp rounds up to a multiple of alignment, saturating rather than wrapping.
// A wrap here would turn a value at the top of the range into a small one, and
// every caller reads the result as an address or a count.
func alignUp(value, alignment uint64) uint64 {
	if alignment == 0 {
		return value
	}

	remainder := value % alignment
	if remainder == 0 {
		return value
	}

	padding := alignment - remainder
	if value > math.MaxUint64-padding {
		return math.MaxUint64
	}

	return value + padding
}
