// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package racerctrl

import (
	"fmt"
	"path/filepath"
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

// The epoch has to be the one the catalog was published with. Taking it from the
// class would let a node run a catalog under an epoch that was bumped for some
// other zone, and then run the next catalog under that same epoch.
func TestDeriveTakesTheEpochFromTheZoneMembership(t *testing.T) {
	d := attachedDerivation()
	d.Cluster.Universes[0].MemberEpochs = map[uint32]uint32{1: 9}

	cfg, err := Derive(d)
	require.NoError(t, err)

	assert.Equal(t, uint32(9), cfg.GetUniverses()[0].GetEpoch(),
		"the epoch published with the catalog is the epoch the catalog runs at")
}

// A membership written before the epoch travelled with it dates from whatever
// the class said, so that is what those nodes keep running until the operator
// stamps the map.
func TestEpochForFallsBackToTheClassEpoch(t *testing.T) {
	state := UniverseState{
		Epoch:        4,
		MemberEpochs: map[uint32]uint32{2: 7},
	}

	assert.Equal(t, uint32(7), state.EpochFor(2))
	assert.Equal(t, uint32(4), state.EpochFor(1), "a zone with no dated membership uses the class epoch")

	undated := UniverseState{Epoch: 4}
	assert.Equal(t, uint32(4), undated.EpochFor(1))
}

// Growing a zone is a stream of single slot moves, and this walks the path a
// node walks for one of them: derive from the catalog the operator published,
// and publish the next generation.
func TestPublishInstallsASlotMove(t *testing.T) {
	d := attachedDerivation()
	d.Cluster.Universes[0].CatalogSize = 2
	d.Cluster.Universes[0].Catalogs = map[uint32]Catalog{1: {{1, 2, 3}, {1, 2, 3}}}

	previous, err := Derive(d)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "racer.binpb")

	changed, err := Publish(path, nil, previous)
	require.NoError(t, err)
	require.True(t, changed)

	moved := resizedDerivation()
	moved.Cluster.Universes[0].CatalogSize = 2
	moved.Cluster.Universes[0].Catalogs = map[uint32]Catalog{1: {{1, 2, 3}, {4, 2, 3}}}

	next, err := Derive(moved)
	require.NoError(t, err)

	next.Generation = previous.GetGeneration() + 1

	changed, err = Publish(path, previous, next)
	require.NoError(t, err, "a zone that cannot hand one group over cannot grow")
	assert.True(t, changed)

	assert.Equal(t, uint32(4), next.GetUniverses()[0].GetCatalog()[1].GetCohort_0(),
		"the second group is what moved")
}

// The old resize published a whole new membership and skipped a generation to
// get past the transition rule. Two of the three nodes answering for a group
// have to survive the change, so a catalog rebuilt wholesale from a wider
// membership is now refused however far the generation moves.
func TestPublishRefusesAWholesaleRegrownCatalog(t *testing.T) {
	d := attachedDerivation()
	d.Cluster.Universes[0].CatalogSize = 2

	previous, err := Derive(d)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "racer.binpb")

	changed, err := Publish(path, nil, previous)
	require.NoError(t, err)
	require.True(t, changed)

	grown := resizedDerivation()
	grown.Cluster.Universes[0].CatalogSize = 2

	next, err := Derive(grown)
	require.NoError(t, err)

	next.Generation = previous.GetGeneration() + 1

	_, err = Publish(path, previous, next)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kept only 0 of its 3 nodes")
}

// A volume with a mutable head and an immutable tail is the shape of every
// volume that has a mutable head, and racer refuses a whole config that names
// warm zones on a mutable extent. Applying the volume's warm list to both
// segments made such a volume unpublishable, which is not a warning: the node
// keeps running its previous generation and reports a counter.
func TestDeriveWarmsOnlyTheImmutableSegments(t *testing.T) {
	d := warmDerivation()

	cfg, err := Derive(d)
	require.NoError(t, err)
	require.NoError(t, Validate(cfg), "a mixed volume with warm zones has to be publishable")

	extents := cfg.GetUniverses()[0].GetExtents()
	require.Len(t, extents, 2)

	byID := map[uint32]*racerconfig.Extent{}
	for _, extent := range extents {
		byID[extent.GetId()] = extent
	}

	assert.Empty(t, byID[1].GetWarmZones(), "the mutable head cannot carry warm zones")
	assert.Equal(t, []uint32{2}, byID[2].GetWarmZones(), "the immutable tail is what warming is for")
}

// A warm zone the universe does not name is a rejected config for the same
// reason, and the zone a volume asks to be warmed into is not necessarily one
// this node can route to.
func TestDeriveDropsUnreachableWarmZones(t *testing.T) {
	d := warmDerivation()
	d.Cluster.Universes[0].Volumes[0].WarmZones = []uint32{2, 9}

	cfg, err := Derive(d)
	require.NoError(t, err)
	require.NoError(t, Validate(cfg))

	for _, extent := range cfg.GetUniverses()[0].GetExtents() {
		assert.NotContains(t, extent.GetWarmZones(), uint32(9),
			"zone 9 has no gateways here, so racer could not route a warm to it")
	}
}

// warmDerivation is the attached zone with a second zone to warm into and a
// volume that has both a mutable head and an immutable tail.
func warmDerivation() Derivation {
	d := attachedDerivation()
	d.Cluster.Universes[0].Gateways = map[uint32][]uint32{2: {4}}
	d.Attachments[Attachment{Universe: 1, Peer: 4}] = "/dev/nvme4n1"
	d.Cluster.Universes[0].Volumes[0] = VolumeState{
		Name:      "pv-1",
		Zone:      1,
		WarmZones: []uint32{2},
		Composition: Composition{
			{ExtentID: 1, BaseLBA: 0, Pages: 16, Kind: racerconfig.Kind_LWW},
			{ExtentID: 2, BaseLBA: HugeBlocks, Pages: 64, Kind: racerconfig.Kind_IMMUTABLE_4M},
		},
	}

	return d
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

// resizedDerivation is the same zone after three more nodes joined its catalog,
// which is a move of one node per cohort at once.
func resizedDerivation() Derivation {
	d := attachedDerivation()
	d.Cluster.Universes[0].Members[1] = Membership{
		{NodeID: 1, Cohort: 0},
		{NodeID: 4, Cohort: 0},
		{NodeID: 2, Cohort: 1},
		{NodeID: 5, Cohort: 1},
		{NodeID: 3, Cohort: 2},
		{NodeID: 6, Cohort: 2},
	}

	for peer := uint32(2); peer <= 6; peer++ {
		d.Attachments[Attachment{Universe: 1, Peer: peer}] = fmt.Sprintf("/dev/nvme%dn1", peer)
	}

	return d
}

// A node the catalog has stopped naming has to keep deriving the universe, with
// itself absent from its catalog. That configuration is what makes racer walk
// the groups it still holds and hand them over; a node that simply lost the
// universe would hold them forever, with no fabric export and nothing to shed
// against.
func TestDeriveKeepsAUniverseTheNodeIsDrainingOutOf(t *testing.T) {
	derivation := attachedDerivation()
	state := &derivation.Cluster.Universes[0]

	state.Members[1] = Membership{
		{NodeID: 4, Cohort: 0}, {NodeID: 2, Cohort: 1}, {NodeID: 3, Cohort: 2},
	}
	state.Draining = map[uint32]Membership{1: {{NodeID: 1, Cohort: 0}}}
	derivation.Attachments[Attachment{Universe: 1, Peer: 4}] = "/dev/nvme3n1"

	cfg, err := Derive(derivation)
	require.NoError(t, err)
	require.NoError(t, Validate(cfg))

	require.Len(t, cfg.GetUniverses(), 1, "the draining node lost the universe it still holds")

	for _, group := range cfg.GetUniverses()[0].GetCatalog() {
		assert.NotContains(t,
			[]uint32{group.GetCohort_0(), group.GetCohort_1(), group.GetCohort_2()},
			uint32(1),
			"a draining node must not be in its own catalog, or it is not draining")
	}

	assert.NotEmpty(t, cfg.GetUniverses()[0].GetPeers(),
		"the handover is a query the draining node makes against the new members")
}

// Once it has drained, the node is in no catalog and no draining set, so there
// is nothing left to derive. Racer refuses a config naming no universe, so the
// caller has to recognise this rather than try to publish it.
func TestDeriveYieldsNothingOnceTheNodeHasDrained(t *testing.T) {
	derivation := attachedDerivation()
	state := &derivation.Cluster.Universes[0]

	state.Members[1] = Membership{
		{NodeID: 4, Cohort: 0}, {NodeID: 2, Cohort: 1}, {NodeID: 3, Cohort: 2},
	}

	cfg, err := Derive(derivation)
	require.NoError(t, err)
	assert.Empty(t, cfg.GetUniverses())
	assert.Error(t, Validate(cfg), "racer refuses a config that names no universe")
}

// The agent has to be able to say which facts the generation it installed
// carries, because racer only ever says which generation is in force.
func TestAppliedFromCarriesTheFactsGatesWaitOn(t *testing.T) {
	cfg, err := Derive(attachedDerivation())
	require.NoError(t, err)

	cfg.Generation = 12
	cfg.Universes[0].Extents[0].NextZone = 2
	cfg.Universes[0].Extents[0].TombstoneEpoch = 6

	applied := AppliedFrom(cfg)

	assert.Equal(t, uint64(12), applied.Generation)
	assert.Equal(t, uint32(1), applied.Epochs[1])
	assert.Equal(t, AppliedExtent{NextZone: 2, TombstoneEpoch: 6}, applied.Extents[1])
}

// Only extents an operation is in flight for are carried. A node holds on the
// order of a thousand, and the annotation this ends up in has a size ceiling.
func TestAppliedFromOmitsSettledExtents(t *testing.T) {
	cfg, err := Derive(attachedDerivation())
	require.NoError(t, err)

	applied := AppliedFrom(cfg)
	assert.Empty(t, applied.Extents)
}
