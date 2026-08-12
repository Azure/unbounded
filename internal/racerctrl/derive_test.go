// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package racerctrl

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	racerconfig "github.com/Azure/unbounded/api/racer"
)

// A cold cluster has no fabric: racer creates a universe's fabric device only
// once it has accepted a config naming that universe, and the peers of that
// universe are attached from exactly those devices. The first config a node
// publishes therefore has to be one it can publish with nothing attached.
func TestDeriveBootstrapsAUniverseWithNoAttachments(t *testing.T) {
	cfg, err := Derive(bootstrapDerivation())
	require.NoError(t, err)
	require.NoError(t, Validate(cfg), "the bootstrap config has to be one racer will accept")

	require.Len(t, cfg.GetUniverses(), 1)
	universe := cfg.GetUniverses()[0]

	assert.NotEmpty(t, universe.GetCatalog(), "the catalog names the universe this device belongs to")
	assert.Equal(t, uint32(7), universe.GetFabricDeviceId(), "the fabric minor is the whole point of the bootstrap config")
	assert.Empty(t, universe.GetPeers())
	assert.Empty(t, universe.GetZones())
	assert.Empty(t, universe.GetExtents(), "an inert universe carries no data and answers no reads")
}

// The store is cold: racer formats it at startup and only a restart picks up a
// larger one. Sizing it to the inert config would mean every bootstrap ended in
// a restart to grow the store back to what the topology actually needs.
func TestDeriveSizesTheBootstrapStoreForTheFullTopology(t *testing.T) {
	bootstrap, err := Derive(bootstrapDerivation())
	require.NoError(t, err)

	full, err := Derive(attachedDerivation())
	require.NoError(t, err)

	assert.Equal(t,
		full.GetNode().GetStore().GetSizeBytes(),
		bootstrap.GetNode().GetStore().GetSizeBytes(),
		"the bootstrap config must reserve the store the full topology needs")

	assert.Equal(t, full.GetPolicy().GetMaxIndexBytes(), bootstrap.GetPolicy().GetMaxIndexBytes())
}

// Once the bootstrap config has brought the fabric devices up and the peers have
// been attached, the very next derivation publishes the universe in full, and
// the move from one to the other is a legal single generation step.
func TestDerivePublishesInFullOnceAttached(t *testing.T) {
	bootstrap, err := Derive(bootstrapDerivation())
	require.NoError(t, err)

	d := attachedDerivation()
	d.Generation = bootstrap.GetGeneration() + 1
	d.Established = EstablishedUniverses(bootstrap)

	full, err := Derive(d)
	require.NoError(t, err)
	require.NoError(t, Validate(full))

	universe := full.GetUniverses()[0]
	assert.Len(t, universe.GetPeers(), 2)
	assert.NotEmpty(t, universe.GetExtents())

	require.NoError(t, ValidateTransition(bootstrap, full),
		"promotion out of the bootstrap shape must be a step racer accepts")
}

// Demotion is only ever for a universe this node has not served. Dropping the
// extents of one it is serving because a peer went away would take data off a
// running node, which is not a thing a degraded group asks for.
func TestDeriveNeverDemotesAnEstablishedUniverse(t *testing.T) {
	d := bootstrapDerivation()
	d.Established = map[uint32]bool{1: true}

	cfg, err := Derive(d)
	require.NoError(t, err)

	universe := cfg.GetUniverses()[0]
	assert.NotEmpty(t, universe.GetExtents(), "an established universe keeps the extents it serves")

	// It is unpublishable in this state, which is the correct outcome: Publish
	// keeps the config the node already has rather than taking its data away.
	assert.Error(t, Validate(cfg))
}

// A remote zone we hold no gateway link to is a zone we cannot route to, which
// is the other half of the same cold start: the gateway's fabric device does not
// exist yet either.
func TestDeriveBootstrapsWhenNoGatewayIsAttached(t *testing.T) {
	d := bootstrapDerivation()
	d.Cluster.Universes[0].Gateways = map[uint32][]uint32{2: {4, 5}}

	cfg, err := Derive(d)
	require.NoError(t, err)
	require.NoError(t, Validate(cfg))

	assert.Empty(t, cfg.GetUniverses()[0].GetZones())
}

// A volume staged while its universe is still coming up must not take the
// bootstrap config down with it, because the bootstrap config is what creates
// the fabric device the volume is waiting on.
func TestDeriveSkipsDevicesOnABootstrappingUniverse(t *testing.T) {
	d := bootstrapDerivation()
	d.Self.Devices = []DeviceBinding{{DeviceID: 9, Volume: "pv-1"}}

	cfg, err := Derive(d)
	require.NoError(t, err)
	require.NoError(t, Validate(cfg))

	assert.Empty(t, cfg.GetDevices(), "there is nothing to export until the universe is published in full")
}

// The same binding on an attached universe is exported, so skipping is confined
// to the bootstrap window rather than being a way to lose a device.
func TestDeriveExportsDevicesOnceTheUniverseIsPublished(t *testing.T) {
	d := attachedDerivation()
	d.Self.Devices = []DeviceBinding{{DeviceID: 9, Volume: "pv-1"}}

	cfg, err := Derive(d)
	require.NoError(t, err)
	require.NoError(t, Validate(cfg))

	require.Len(t, cfg.GetDevices(), 1)
	assert.Equal(t, uint32(9), cfg.GetDevices()[0].GetId())
	assert.Equal(t, []uint32{1}, cfg.GetDevices()[0].GetExtents())
}

// A binding on a universe that is published in full and still does not carry the
// extents is a real inconsistency, not a cold start, and stays an error.
func TestDeriveRejectsADeviceForAnUnknownVolume(t *testing.T) {
	d := attachedDerivation()
	d.Self.Devices = []DeviceBinding{{DeviceID: 9, Volume: "pv-missing"}}

	_, err := Derive(d)
	require.Error(t, err)
}

// bootstrapDerivation is a three node zone with one volume where nothing has
// been attached yet, which is what every node sees on a cold cluster.
func bootstrapDerivation() Derivation {
	return Derivation{
		Cluster: ClusterState{
			Universes: []UniverseState{{
				Class:       "fast",
				ID:          1,
				Epoch:       1,
				CatalogSize: 3,
				Members: map[uint32]Membership{1: {
					{NodeID: 1, Cohort: 0},
					{NodeID: 2, Cohort: 1},
					{NodeID: 3, Cohort: 2},
				}},
				Volumes: []VolumeState{{
					Name: "pv-1",
					Zone: 1,
					Composition: Composition{
						{ExtentID: 1, BaseLBA: 0, Pages: 64, Kind: racerconfig.Kind_IMMUTABLE_4M},
					},
				}},
			}},
		},
		Self: NodeState{
			Name:   "n1",
			ID:     1,
			Zone:   1,
			Cohort: 0,
			Fabric: []FabricExport{{UniverseID: 1, DeviceID: 7}},
		},
		Generation: 1,
	}
}

// attachedDerivation is the same zone once the fabric devices exist and the
// peers have been attached.
func attachedDerivation() Derivation {
	d := bootstrapDerivation()
	d.Attachments = map[Attachment]string{
		{Universe: 1, Peer: 2}: "/dev/nvme1n1",
		{Universe: 1, Peer: 3}: "/dev/nvme2n1",
	}

	return d
}
