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
