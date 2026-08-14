// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package racerctrl

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loadedNode is a node whose agent has installed a config and whose racer is
// running it, carrying one universe at one epoch.
func loadedNode(name string, id, zone, universe, epoch uint32) NodeState {
	return NodeState{
		Name: name,
		ID:   id,
		Zone: zone,
		Applied: Applied{
			Generation: 10,
			Epochs:     map[uint32]uint32{universe: epoch},
			Extents:    map[uint32]AppliedExtent{},
		},
		Health: Health{Generation: 10},
		Live:   map[uint32]LiveExtent{},
	}
}

func TestConfigLoadedNeedsBothHalvesOfTheProof(t *testing.T) {
	cases := []struct {
		name    string
		node    NodeState
		ok      bool
		because string
	}{
		{
			name: "running what the agent installed",
			node: loadedNode("n1", 1, 1, 1, 1),
			ok:   true,
		},
		{
			name: "agent has installed nothing",
			node: NodeState{Name: "n1", Health: Health{Generation: 4}},
			// A generation with no agent behind it describes a topology
			// nothing is maintaining.
			because: "has not published a config yet",
		},
		{
			name:    "racer has not reported",
			node:    NodeState{Name: "n1", Applied: Applied{Generation: 4}},
			because: "has not reported yet",
		},
		{
			name: "racer is behind, which is also what a refusal looks like",
			node: NodeState{
				Name:    "n1",
				Applied: Applied{Generation: 9},
				Health:  Health{Generation: 4},
			},
			because: "not yet the 9 its agent installed",
		},
	}

	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			gate := ConfigLoaded(item.node)

			assert.Equal(t, item.ok, gate.OK, "gate said %q", gate)

			if !item.ok {
				assert.Contains(t, gate.Reason, item.because)
			}
		})
	}
}

// A node quiet about last week's catalog is not quiet about this week's change.
// The counters are derived from the catalog racer holds, so they mean nothing
// until the node is running the configuration the change was published as.
func TestHealingQuiescedWaitsForTheEpochTheCountersDescribe(t *testing.T) {
	behind := loadedNode("n1", 1, 1, 1, 3)

	gate := HealingQuiesced([]NodeState{behind}, 1, 4)
	assert.False(t, gate.OK)
	assert.Contains(t, gate.Reason, "at epoch 3, not yet 4")

	assert.True(t, HealingQuiesced([]NodeState{behind}, 1, 3).OK)
}

func TestHealingQuiescedBlocksOnBothCounters(t *testing.T) {
	replaying := loadedNode("n1", 1, 1, 1, 1)
	replaying.Health.Replaying = 2

	gate := HealingQuiesced([]NodeState{replaying}, 1, 1)
	assert.False(t, gate.OK)
	assert.Contains(t, gate.Reason, "replaying 2 groups")

	shedding := loadedNode("n2", 2, 1, 1, 1)
	shedding.Health.Shedding = 1

	gate = HealingQuiesced([]NodeState{shedding}, 1, 1)
	assert.False(t, gate.OK)
	assert.Contains(t, gate.Reason, "shedding 1 groups")
}

// migrating builds a volume mid-move and the two zones' worth of nodes that
// carry it.
func migrating() (VolumeState, []NodeState) {
	volume := VolumeState{
		Name:        "pv-a",
		Composition: Composition{{ExtentID: 7, Pages: 64}},
		Zone:        1,
		NextZone:    2,
	}

	var nodes []NodeState

	for _, item := range []struct {
		name string
		id   uint32
		zone uint32
	}{{"n1", 1, 1}, {"n2", 2, 2}} {
		node := loadedNode(item.name, item.id, item.zone, 1, 1)
		node.Applied.Extents[7] = AppliedExtent{NextZone: 2}
		node.Live[7] = LiveExtent{}
		nodes = append(nodes, node)
	}

	return volume, nodes
}

// The failure this guards is the whole reason the gate exists: an unreported
// extent reads as zero pages out of the map, and zero at the destination
// compares equal to zero at the source, so a migration nobody has started
// declares itself complete.
func TestMigrationCompleteNeedsAnExplicitReport(t *testing.T) {
	volume, nodes := migrating()

	nodes[0].Live[7] = LiveExtent{Pages: 64}

	gate := MigrationComplete(volume, nodes)
	assert.False(t, gate.OK, "destination holds nothing but the gate opened")

	delete(nodes[1].Live, 7)

	gate = MigrationComplete(volume, nodes)
	assert.False(t, gate.OK)
	assert.Contains(t, gate.Reason, "has not reported volume pv-a extent 7")
}

func TestMigrationCompleteNeedsTheDestinationInTheRunningConfig(t *testing.T) {
	volume, nodes := migrating()

	nodes[1].Applied.Extents[7] = AppliedExtent{}

	gate := MigrationComplete(volume, nodes)
	assert.False(t, gate.OK)
	assert.Contains(t, gate.Reason, "not yet migrating volume pv-a extent 7 to zone 2")
}

func TestMigrationCompleteOpensOnceTheDestinationHasCaughtUp(t *testing.T) {
	volume, nodes := migrating()

	nodes[0].Live[7] = LiveExtent{Pages: 64}
	nodes[1].Live[7] = LiveExtent{Pages: 64}

	assert.True(t, MigrationComplete(volume, nodes).OK)
}

func TestMigrationCompleteIgnoresNodesTheExtentNeverReaches(t *testing.T) {
	volume, nodes := migrating()

	nodes[0].Live[7] = LiveExtent{Pages: 64}
	nodes[1].Live[7] = LiveExtent{Pages: 64}

	// A node in a third zone that has never been sent the extent. Waiting on it
	// would block the migration forever.
	nodes = append(nodes, loadedNode("n9", 9, 3, 1, 1))

	assert.True(t, MigrationComplete(volume, nodes).OK)
}

func TestMigrationCompleteBlocksWithNothingInFlight(t *testing.T) {
	volume, nodes := migrating()
	volume.NextZone = 0

	gate := MigrationComplete(volume, nodes)
	assert.False(t, gate.OK)
	assert.Contains(t, gate.Reason, "no migration in flight")
}

// collecting builds a volume whose tombstone epoch has been advanced, and the
// nodes of its zone.
func collecting() (VolumeState, []NodeState) {
	volume := VolumeState{
		Name:            "pv-a",
		Composition:     Composition{{ExtentID: 7, Pages: 64}},
		Zone:            1,
		Phase:           PhaseCollecting,
		TombstoneEpochs: map[uint32]uint32{7: 5},
	}

	var nodes []NodeState

	for i, name := range []string{"n1", "n2", "n3"} {
		node := loadedNode(name, uint32(i)+1, 1, 1, 1)
		node.Applied.Extents[7] = AppliedExtent{TombstoneEpoch: 5}
		node.Live[7] = LiveExtent{}
		nodes = append(nodes, node)
	}

	return volume, nodes
}

// This gate is the last thing standing between a PersistentVolume and having
// its finalizer removed. Once the object is gone nothing names the extent, so
// anything still holding it holds it forever.
func TestCollectionDrainedNeedsEveryCarrierToHaveLoadedTheEpoch(t *testing.T) {
	volume, nodes := collecting()

	nodes[2].Applied.Extents[7] = AppliedExtent{TombstoneEpoch: 4}

	gate := CollectionDrained(volume, nodes)
	assert.False(t, gate.OK)
	assert.Contains(t, gate.Reason, "tombstone epoch 4, not yet 5")
}

func TestCollectionDrainedNeedsAnExplicitReport(t *testing.T) {
	volume, nodes := collecting()

	delete(nodes[1].Live, 7)

	gate := CollectionDrained(volume, nodes)
	assert.False(t, gate.OK)
	assert.Contains(t, gate.Reason, "has not reported volume pv-a extent 7")
}

func TestCollectionDrainedWaitsForTombstonesAndPages(t *testing.T) {
	volume, nodes := collecting()

	nodes[0].Live[7] = LiveExtent{Tombstones: 3}

	gate := CollectionDrained(volume, nodes)
	assert.False(t, gate.OK)
	assert.Contains(t, gate.Reason, "still has 3 tombstones")

	nodes[0].Live[7] = LiveExtent{Pages: 2}

	gate = CollectionDrained(volume, nodes)
	assert.False(t, gate.OK)
	assert.Contains(t, gate.Reason, "still has 2 live pages")
}

func TestCollectionDrainedOpensWhenEveryCarrierIsEmpty(t *testing.T) {
	volume, nodes := collecting()

	assert.True(t, CollectionDrained(volume, nodes).OK)
}

// A node exporting the volume to a pod holds registers for it even if it is in
// another zone, so it has to be asked before the extent is forgotten.
func TestCollectionDrainedAsksTheExportingNode(t *testing.T) {
	volume, nodes := collecting()

	consumer := loadedNode("n9", 9, 4, 1, 1)
	consumer.Devices = []DeviceBinding{{DeviceID: 3, Volume: "pv-a"}}

	gate := CollectionDrained(volume, append(nodes, consumer))
	assert.False(t, gate.OK)
	assert.Contains(t, gate.Reason, "node n9")
}

func TestDrainCompleteWaitsForTheConfigThatDropsTheNode(t *testing.T) {
	node := loadedNode("n1", 1, 1, 1, 3)

	gate := DrainComplete(node, 1, 4)
	assert.False(t, gate.OK)
	assert.Contains(t, gate.Reason, "not yet the 4 that drops it")

	node.Applied.Epochs[1] = 4
	node.Health.Shedding = 2

	gate = DrainComplete(node, 1, 4)
	assert.False(t, gate.OK)
	assert.Contains(t, gate.Reason, "shedding 2 groups")

	node.Health.Shedding = 0
	assert.True(t, DrainComplete(node, 1, 4).OK)
}

func TestDecommissionCompleteWaitsForAnIdleNode(t *testing.T) {
	node := loadedNode("n1", 1, 1, 1, 4)
	node.Live[7] = LiveExtent{Pages: 1}

	gate := DecommissionComplete(node)
	assert.False(t, gate.OK)
	assert.Contains(t, gate.Reason, "still holds live pages")

	node.Live[7] = LiveExtent{}
	assert.True(t, DecommissionComplete(node).OK)
}

func TestDecommissionCompleteRefusesANodeWhoseAgentIsGone(t *testing.T) {
	gate := DecommissionComplete(NodeState{Name: "n1", Health: Health{Generation: 4}})
	assert.False(t, gate.OK)
	assert.Contains(t, gate.Reason, "has not published a config yet")
}

func TestStoreGrowthNeededNamesTheNodesThatAreShort(t *testing.T) {
	short := loadedNode("n2", 2, 1, 1, 1)
	short.Health.UnbackedPages = 900

	names := StoreGrowthNeeded([]NodeState{short, loadedNode("n1", 1, 1, 1, 1)})
	require.Equal(t, []string{"n2"}, names)
}

// zonePlan is a settled three-node zone plus a fourth candidate, with every
// member running the current epoch.
func zonePlan(current, draining, candidates Membership) MembershipPlan {
	var nodes []NodeState

	for i, name := range []string{"n1", "n2", "n3", "n4"} {
		nodes = append(nodes, loadedNode(name, uint32(i)+1, 1, 1, 2))
	}

	return MembershipPlan{
		Universe:    1,
		Epoch:       2,
		CatalogSize: 3,
		Current:     current,
		Draining:    draining,
		Candidates:  candidates,
		Admissible:  candidates,
		Nodes:       nodes,
	}
}

func members(ids ...uint32) Membership {
	out := make(Membership, 0, len(ids))
	for i, id := range ids {
		out = append(out, Member{NodeID: id, Cohort: uint32(i) % Cohorts})
	}

	return out
}

// The whole point of the draining set: the id the step drops has to be named
// somewhere, or the node it names never learns it was dropped and never sheds.
func TestPlanMembershipNamesTheDepartingNode(t *testing.T) {
	plan := zonePlan(members(1, 2, 3), nil, Membership{
		{NodeID: 4, Cohort: 0}, {NodeID: 2, Cohort: 1}, {NodeID: 3, Cohort: 2},
	})

	step, gate, err := PlanMembership(plan)
	require.NoError(t, err)
	require.True(t, gate.OK, "gate said %q", gate)
	require.False(t, step.Done)

	assert.True(t, step.Next.Contains(4), "step did not take the candidate")
	assert.False(t, step.Next.Contains(1), "step kept the departing node in the catalog")
	assert.True(t, step.Draining.Contains(1), "the departing node was not named as draining")
}

// A node in the middle of handing groups over must not be handed them back.
func TestPlanMembershipWillNotRenameADrainingNode(t *testing.T) {
	plan := zonePlan(members(4, 2, 3), members(1), Membership{
		{NodeID: 1, Cohort: 0},
		{NodeID: 4, Cohort: 0},
		{NodeID: 2, Cohort: 1},
		{NodeID: 3, Cohort: 2},
	})

	plan.Nodes[0].Health.Shedding = 4

	step, gate, err := PlanMembership(plan)
	require.NoError(t, err)
	require.True(t, gate.OK, "gate said %q", gate)

	assert.False(t, step.Next.Contains(1))
	assert.True(t, step.Draining.Contains(1))
}

// The departing node is where the shedding is. Leaving it out of the gate is
// exactly how the next replacement starts on top of the last one.
func TestPlanMembershipWaitsOnTheNodeItJustDropped(t *testing.T) {
	plan := zonePlan(members(4, 2, 3), members(1), Membership{
		{NodeID: 4, Cohort: 0}, {NodeID: 5, Cohort: 1}, {NodeID: 3, Cohort: 2},
	})

	plan.Nodes[0].Health.Shedding = 4
	plan.Nodes = append(plan.Nodes, loadedNode("n5", 5, 1, 1, 2))

	_, gate, err := PlanMembership(plan)
	require.NoError(t, err)
	assert.False(t, gate.OK, "a second swap started while the first was still shedding")
	assert.Contains(t, gate.Reason, "n1")
}

// A node that has finished draining leaving the set is a change worth
// publishing on its own: it is what finally lets the node stop deriving the
// universe and lets the operator retire its identity.
func TestPlanMembershipRetiresADrainedNode(t *testing.T) {
	plan := zonePlan(members(4, 2, 3), members(1), Membership{
		{NodeID: 4, Cohort: 0}, {NodeID: 2, Cohort: 1}, {NodeID: 3, Cohort: 2},
	})

	step, gate, err := PlanMembership(plan)
	require.NoError(t, err)
	require.True(t, gate.OK, "gate said %q", gate)

	assert.False(t, step.Done, "retiring a drained node is a publishable change")
	assert.Empty(t, step.Draining)
	assert.Equal(t, plan.Current, step.Next, "the catalog changed while only a drain finished")
}

// A node whose object is gone will never report again, so holding the set open
// for it would block every later step forever.
func TestPlanMembershipForgetsAVanishedNode(t *testing.T) {
	plan := zonePlan(members(4, 2, 3), members(9), Membership{
		{NodeID: 4, Cohort: 0}, {NodeID: 2, Cohort: 1}, {NodeID: 3, Cohort: 2},
	})

	step, gate, err := PlanMembership(plan)
	require.NoError(t, err)
	require.True(t, gate.OK, "gate said %q", gate)
	assert.Empty(t, step.Draining)
}

func TestPlanMembershipIsDoneWhenNothingIsMoving(t *testing.T) {
	plan := zonePlan(members(1, 2, 3), nil, members(1, 2, 3))

	step, gate, err := PlanMembership(plan)
	require.NoError(t, err)
	require.True(t, gate.OK, "gate said %q", gate)
	assert.True(t, step.Done)
	assert.Empty(t, step.Draining)
}

// A candidate has no health until a membership names it and something hands it
// a config, so gating on its silence would make it impossible ever to add one.
func TestPlanMembershipAddsANodeThatHasNeverReported(t *testing.T) {
	plan := zonePlan(members(1, 2, 3), nil, Membership{
		{NodeID: 4, Cohort: 0}, {NodeID: 2, Cohort: 1}, {NodeID: 3, Cohort: 2},
	})

	plan.Nodes[3] = NodeState{Name: "n4", ID: 4, Zone: 1}

	step, gate, err := PlanMembership(plan)
	require.NoError(t, err)
	require.True(t, gate.OK, "gate said %q", gate)
	assert.True(t, step.Next.Contains(4))
}

// A member still replaying holds the next step, because a replaying group is
// already running two of three.
func TestPlanMembershipWaitsWhileAMemberHeals(t *testing.T) {
	plan := zonePlan(members(1, 2, 3), nil, Membership{
		{NodeID: 4, Cohort: 0}, {NodeID: 2, Cohort: 1}, {NodeID: 3, Cohort: 2},
	})

	plan.Nodes[1].Health.Replaying = 1

	_, gate, err := PlanMembership(plan)
	require.NoError(t, err)
	assert.False(t, gate.OK)
	assert.Contains(t, gate.Reason, "n2")
}
