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

// The ordinary case is a handoff between consecutive generations, which is what
// the dataplane heals incrementally.
func TestTransitionStrideIsOneForASwap(t *testing.T) {
	previous := fixtureConfig(t, 1)

	next := fixtureConfig(t, 0)
	next.Universes[0].Catalog = []*racerconfig.Trio{{Cohort_0: 1, Cohort_1: 2, Cohort_2: 4}}

	stride := TransitionStride(previous, next)
	assert.Equal(t, uint64(1), stride)

	next.Generation = previous.GetGeneration() + stride
	require.NoError(t, ValidateTransition(previous, next))
}

// A resize moves one node per cohort at once. Publishing that at the next
// generation is rejected as an illegal step by this package and by racer, so
// without the skip the catalog could never be resized at all.
func TestTransitionStrideSkipsAGenerationForAResize(t *testing.T) {
	// The catalog keeps its length across a resize: what changes is how many
	// nodes each cohort spreads it over.
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

	stride := TransitionStride(previous, next)
	require.Equal(t, uint64(2), stride)

	next.Generation = previous.GetGeneration() + 1
	require.Error(t, ValidateTransition(previous, next),
		"a resize published as a step is exactly what the stride exists to avoid")

	next.Generation = previous.GetGeneration() + stride
	require.NoError(t, ValidateTransition(previous, next))
}

// A universe this node has not run before has no membership to have changed, so
// there is nothing to skip a generation for.
func TestTransitionStrideIsOneForNewGround(t *testing.T) {
	assert.Equal(t, uint64(1), TransitionStride(nil, fixtureConfig(t, 1)))

	previous := fixtureConfig(t, 1)
	previous.Universes = nil

	assert.Equal(t, uint64(1), TransitionStride(previous, fixtureConfig(t, 0)))
}
