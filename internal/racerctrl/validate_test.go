// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package racerctrl

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	racerconfig "github.com/Azure/unbounded/api/racer"
)

// The bootstrap shape exists so racer will create a universe's fabric device
// before any peer can be attached from it. It carries nothing, so there is
// nothing for a missing peer to make unavailable.
func TestValidateAcceptsThePeerlessBootstrapShape(t *testing.T) {
	cfg := bootstrapShape(t)

	require.NoError(t, Validate(cfg))
}

// The exemption is for a universe that carries nothing. A peerless universe that
// names extents cannot serve the groups it does not hold, which is R5's rule and
// stays enforced.
func TestValidateRejectsAPeerlessUniverseThatCarriesExtents(t *testing.T) {
	cfg := bootstrapShape(t)
	cfg.Universes[0].Extents = []*racerconfig.Extent{
		{Id: 1, BaseLba: 0, Pages: 16, Kind: racerconfig.Kind_IMMUTABLE_4M, Zone: 1},
	}

	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no peers")
}

// A universe that names a remote zone is routing, and routing needs a gateway it
// holds a link to.
func TestValidateRejectsAPeerlessUniverseThatNamesZones(t *testing.T) {
	cfg := bootstrapShape(t)
	cfg.Universes[0].Zones = []*racerconfig.Zone{{Id: 2, Gateways: []uint32{4}}}

	require.Error(t, Validate(cfg))
}

// Promotion out of the bootstrap shape keeps the catalog and the fabric device
// id, so it is an ordinary one generation step.
func TestValidateTransitionAllowsPromotionOutOfBootstrap(t *testing.T) {
	previous := bootstrapShape(t)

	next := fixtureConfig(t, 2)
	next.Node.Store.SizeBytes = previous.GetNode().GetStore().GetSizeBytes()
	next.Universes[0].FabricDeviceId = previous.GetUniverses()[0].GetFabricDeviceId()

	require.NoError(t, Validate(next))
	require.NoError(t, ValidateTransition(previous, next))
}

// bootstrapShape is the inert config a node publishes to bring a universe's
// fabric device up: a catalog and a fabric device id, and nothing else.
func bootstrapShape(t *testing.T) *racerconfig.NodeConfig {
	t.Helper()

	cfg := fixtureConfig(t, 1)
	cfg.Universes[0].Peers = nil
	cfg.Universes[0].Zones = nil
	cfg.Universes[0].Extents = nil
	cfg.Devices = nil

	return cfg
}

// The ordinary case is a handoff: one group changes one of its three nodes, so
// the two that stayed can serve reads while the newcomer replays from them.
func TestValidateTransitionAllowsAHandoff(t *testing.T) {
	previous := fixtureConfig(t, 1)

	next := fixtureConfig(t, 2)
	next.Universes[0].Catalog = []*racerconfig.Trio{{Cohort_0: 1, Cohort_1: 2, Cohort_2: 4}}

	require.NoError(t, ValidateTransition(previous, next))
}

// A group that changed two of its three nodes is running on one copy, and one
// that changed all three has no copy at all while reporting itself healthy,
// because three empty replicas agree with each other.
func TestValidateTransitionRefusesAGroupThatLostItsQuorum(t *testing.T) {
	previous := fixtureConfig(t, 1)

	next := fixtureConfig(t, 2)
	next.Universes[0].Catalog = []*racerconfig.Trio{{Cohort_0: 1, Cohort_1: 5, Cohort_2: 4}}

	require.ErrorContains(t, ValidateTransition(previous, next), "kept only 1 of its 3 nodes")
}

// Skipping a generation used to excuse a config from the per-step rules. It no
// longer does: a node that missed a generation is a node that missed a handoff,
// and handing it a catalog that shares nothing with what it holds is the same
// data loss whether or not the number in between was ever published.
func TestValidateTransitionHoldsASkippedGenerationToTheSameRule(t *testing.T) {
	const catalogSize = 2

	small, err := BuildCatalog(Membership{
		{NodeID: 1, Cohort: 0}, {NodeID: 2, Cohort: 1}, {NodeID: 3, Cohort: 2},
	}, catalogSize)
	require.NoError(t, err)

	grown, err := BuildCatalog(Membership{
		{NodeID: 1, Cohort: 0},
		{NodeID: 4, Cohort: 0},
		{NodeID: 2, Cohort: 1},
		{NodeID: 5, Cohort: 1},
		{NodeID: 3, Cohort: 2},
		{NodeID: 6, Cohort: 2},
	}, catalogSize)
	require.NoError(t, err)

	previous := fixtureConfig(t, 1)
	previous.Universes[0].Catalog = small

	next := fixtureConfig(t, 0)
	next.Universes[0].Catalog = grown

	for _, generation := range []uint64{2, 3, 17} {
		next.Generation = generation
		require.Error(t, ValidateTransition(previous, next),
			"generation %d", generation)
	}
}

// A step may move slots without changing who is in the catalog, which is what
// levelling an uneven column does.
func TestValidateTransitionAllowsASlotMoveWithinTheMembership(t *testing.T) {
	previous := fixtureConfig(t, 1)
	previous.Universes[0].Catalog = []*racerconfig.Trio{
		{Cohort_0: 1, Cohort_1: 2, Cohort_2: 3},
		{Cohort_0: 1, Cohort_1: 2, Cohort_2: 3},
		{Cohort_0: 4, Cohort_1: 2, Cohort_2: 3},
	}

	next := fixtureConfig(t, 2)
	next.Universes[0].Catalog = []*racerconfig.Trio{
		{Cohort_0: 1, Cohort_1: 2, Cohort_2: 3},
		{Cohort_0: 4, Cohort_1: 2, Cohort_2: 3},
		{Cohort_0: 4, Cohort_1: 2, Cohort_2: 3},
	}

	require.NoError(t, ValidateTransition(previous, next))
}

// Two ids arriving at once is legal per group but not per zone: it puts two
// nodes' worth of replay on the zone at the same time and makes a departure
// something the control plane cannot wait on.
func TestValidateTransitionRefusesTwoJoinsAtOnce(t *testing.T) {
	previous := fixtureConfig(t, 1)
	previous.Universes[0].Catalog = []*racerconfig.Trio{
		{Cohort_0: 1, Cohort_1: 2, Cohort_2: 3},
		{Cohort_0: 1, Cohort_1: 2, Cohort_2: 3},
	}

	next := fixtureConfig(t, 2)
	next.Universes[0].Catalog = []*racerconfig.Trio{
		{Cohort_0: 4, Cohort_1: 2, Cohort_2: 3},
		{Cohort_0: 1, Cohort_1: 5, Cohort_2: 3},
	}

	require.ErrorContains(t, ValidateTransition(previous, next), "2 joins and 0 departures")
}

// R2 freezes the zone with the id and the cohort. racer does not refuse a moved
// node: it would simply start answering for groups in a zone whose catalog
// never named it, while the zone it left still expects it.
func TestValidateTransitionRefusesAMovedNode(t *testing.T) {
	previous := fixtureConfig(t, 1)

	next := fixtureConfig(t, 2)
	next.Node.Zone = 2

	require.ErrorContains(t, ValidateTransition(previous, next), "zone changed")
}

// The fabric device id is the minor a universe's namespace is published on, so
// every peer's attachment is a path to /dev/ublkb<id>. Moving it points every
// peer at a path that is no longer the universe.
func TestValidateTransitionRefusesAMovedFabricMinor(t *testing.T) {
	previous := fixtureConfig(t, 1)

	next := fixtureConfig(t, 2)
	next.Universes[0].FabricDeviceId = 9

	require.ErrorContains(t, ValidateTransition(previous, next), "fabric_device_id changed")
}

// Extent ids come from one global space precisely so that an id names one
// extent everywhere. A check scoped to a single universe let an id vanish from
// one and reappear in another, which is the same extent at a different global
// address.
func TestValidateTransitionRefusesACrossUniverseExtentMove(t *testing.T) {
	previous := fixtureConfig(t, 1)

	next := fixtureConfig(t, 2)
	next.Universes[0].Extents = nil
	next.Universes = append(next.Universes, &racerconfig.Universe{
		Id:    2,
		Epoch: 1,
		Catalog: []*racerconfig.Trio{
			{Cohort_0: 1, Cohort_1: 2, Cohort_2: 3},
		},
		Peers: []*racerconfig.Peer{
			{Id: 2, Device: "/dev/nvme3n1"},
			{Id: 3, Device: "/dev/nvme4n1"},
		},
		Extents: []*racerconfig.Extent{
			{Id: 1, BaseLba: 0, Pages: 16, Kind: racerconfig.Kind_IMMUTABLE_4M, Zone: 1},
		},
		FabricDeviceId: 3,
	})

	require.ErrorContains(t, ValidateTransition(previous, next), "moved from universe 1 to universe 2")
}

// An extent that stays where it is across a config that gains a universe is the
// ordinary case, and must not trip the cross-universe check.
func TestValidateTransitionAllowsANewUniverseAlongsideAnExtent(t *testing.T) {
	previous := fixtureConfig(t, 1)

	next := fixtureConfig(t, 2)
	next.Universes = append(next.Universes, &racerconfig.Universe{
		Id:             2,
		Epoch:          1,
		Catalog:        []*racerconfig.Trio{{Cohort_0: 1, Cohort_1: 2, Cohort_2: 3}},
		FabricDeviceId: 3,
	})

	require.NoError(t, ValidateTransition(previous, next))
}

// racer refuses a whole config that names warm zones on a mutable extent, and a
// refused config is a node stuck at its previous generation. This package has to
// catch it first.
func TestValidateRefusesWarmZonesOnAMutableExtent(t *testing.T) {
	cfg := fixtureConfig(t, 1)
	cfg.Universes[0].Zones = []*racerconfig.Zone{{Id: 2, Gateways: []uint32{4}}}
	cfg.Universes[0].Peers = append(cfg.Universes[0].Peers, &racerconfig.Peer{Id: 4, Device: "/dev/nvme3n1"})
	cfg.Universes[0].Extents[0].Kind = racerconfig.Kind_LWW
	cfg.Universes[0].Extents[0].WarmZones = []uint32{2}

	require.ErrorContains(t, Validate(cfg), "must not name warm zones")
}

// A warm zone is a zone pages are pushed to, so racer has to be able to route to
// it, and refuses the config when it cannot.
func TestValidateRefusesAnUnknownWarmZone(t *testing.T) {
	cfg := fixtureConfig(t, 1)
	cfg.Universes[0].Extents[0].WarmZones = []uint32{9}

	require.ErrorContains(t, Validate(cfg), "which the universe does not name")
}

// The same warm zone on an immutable extent the universe does name is the case
// warming exists for.
func TestValidateAcceptsAKnownWarmZone(t *testing.T) {
	cfg := fixtureConfig(t, 1)
	cfg.Universes[0].Zones = []*racerconfig.Zone{{Id: 2, Gateways: []uint32{4}}}
	cfg.Universes[0].Peers = append(cfg.Universes[0].Peers, &racerconfig.Peer{Id: 4, Device: "/dev/nvme3n1"})
	cfg.Universes[0].Extents[0].WarmZones = []uint32{2}

	require.NoError(t, Validate(cfg))
}
