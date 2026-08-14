// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package racerctrl

import (
	"testing"

	"github.com/stretchr/testify/assert"

	racerconfig "github.com/Azure/unbounded/api/racer"
)

// storeUniverse is a universe carrying one small and one huge extent in zone 1,
// over the catalog it is given.
func storeUniverse(catalog Catalog) *racerconfig.Universe {
	return &racerconfig.Universe{
		Id:      1,
		Epoch:   1,
		Catalog: catalog.Trios(),
		Extents: []*racerconfig.Extent{
			{Id: 1, BaseLba: 0, Pages: 900, Kind: racerconfig.Kind_LWW, Zone: 1},
			{
				Id:      2,
				BaseLba: HugeBlocks,
				Pages:   90,
				Kind:    racerconfig.Kind_IMMUTABLE_4M,
				Zone:    1,
			},
		},
	}
}

func TestCountStorePagesSizesTheSlotsANodeHolds(t *testing.T) {
	// Nine groups over three nodes per column: every node holds three.
	catalog := Catalog{
		{1, 2, 3},
		{1, 2, 3},
		{1, 2, 3},
		{4, 5, 6},
		{4, 5, 6},
		{4, 5, 6},
		{7, 8, 9},
		{7, 8, 9},
		{7, 8, 9},
	}
	universe := storeUniverse(catalog)

	pages := CountStorePages(1, 1, []*racerconfig.Universe{universe})

	// Three of nine groups, so a third of the zone.
	assert.Equal(t, uint64(300), pages.Small)
	assert.Equal(t, uint64(30), pages.Huge)
}

func TestCountStorePagesFollowsAnUnevenShare(t *testing.T) {
	// Node 1 holds two of the three groups in its column, node 4 holds one.
	catalog := Catalog{{1, 2, 3}, {1, 2, 3}, {4, 2, 3}}
	universe := storeUniverse(catalog)

	heavy := CountStorePages(1, 1, []*racerconfig.Universe{universe})
	light := CountStorePages(4, 1, []*racerconfig.Universe{universe})

	assert.Equal(t, uint64(600), heavy.Small, "two of three groups")
	assert.Equal(t, uint64(60), heavy.Huge)
	assert.Equal(t, uint64(300), light.Small, "one of three groups")
	assert.Equal(t, uint64(30), light.Huge)

	assert.Equal(t, uint64(900), heavy.Small+light.Small,
		"the column stores the whole zone however it is split")
}

func TestCountStorePagesSizesAnUnnamedNodeAsAnEvenShare(t *testing.T) {
	catalog := Catalog{{1, 2, 3}, {1, 2, 3}, {1, 2, 3}}
	universe := storeUniverse(catalog)

	// Node 9 is not in the catalog: it is arriving or draining out, and either
	// way the store may not shrink under it.
	pages := CountStorePages(9, 1, []*racerconfig.Universe{universe})

	assert.Equal(t, uint64(900), pages.Small, "nine slots over three nodes is three groups")
	assert.Equal(t, uint64(90), pages.Huge)
}

func TestCountStorePagesCountsBothEndsOfAMigration(t *testing.T) {
	catalog := Catalog{{1, 2, 3}}
	universe := storeUniverse(catalog)
	universe.Extents[0].Zone = 2
	universe.Extents[0].NextZone = 1

	pages := CountStorePages(1, 1, []*racerconfig.Universe{universe})

	assert.Equal(t, uint64(900), pages.Small,
		"an extent moving in is held by the destination before the flip")
}

func TestCountStorePagesIgnoresAnotherZone(t *testing.T) {
	catalog := Catalog{{1, 2, 3}}
	universe := storeUniverse(catalog)
	universe.Extents[0].Zone = 2
	universe.Extents[1].Zone = 2

	pages := CountStorePages(1, 1, []*racerconfig.Universe{universe})

	assert.Zero(t, pages.Small)
	assert.Zero(t, pages.Huge)
}

func TestCountStorePagesSumsTheUniversesANodeJoined(t *testing.T) {
	first := storeUniverse(Catalog{{1, 2, 3}})
	second := storeUniverse(Catalog{{1, 2, 3}})
	second.Id = 2

	pages := CountStorePages(1, 1, []*racerconfig.Universe{first, second})

	assert.Equal(t, uint64(1800), pages.Small)
	assert.Equal(t, uint64(180), pages.Huge)
}

func TestCountStorePagesRoundsAShareUp(t *testing.T) {
	// Two groups, one held: half of 901 pages is not a whole page.
	catalog := Catalog{{1, 2, 3}, {4, 2, 3}}
	universe := storeUniverse(catalog)
	universe.Extents[0].Pages = 901

	pages := CountStorePages(1, 1, []*racerconfig.Universe{universe})

	assert.Equal(t, uint64(451), pages.Small, "a partial page still needs a page")
}

func TestCountStorePagesIsNothingWithoutACatalog(t *testing.T) {
	universe := storeUniverse(nil)

	pages := CountStorePages(1, 1, []*racerconfig.Universe{universe})

	assert.Zero(t, pages.Small)
	assert.Zero(t, pages.Huge)
}
