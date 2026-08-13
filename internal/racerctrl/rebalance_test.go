// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package racerctrl

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// zone drives PlanMembership the way the operator does: publish the step, feed
// it back in, repeat. Nothing is remembered between passes except what a step
// publishes, which is the property that lets a restarted operator resume.
type zone struct {
	t        *testing.T
	universe uint32
	epoch    uint32
	size     int
	batch    int

	catalog  Catalog
	current  Membership
	draining Membership

	belongs Membership
	ready   map[uint32]bool
}

// nodeIDs are laid out so a node's cohort is derivable from its id, because a
// cohort is frozen for a node's life and a test that reuses an id in two
// cohorts is testing something the cluster cannot do.
func member(cohort, index uint32) Member {
	return Member{NodeID: cohort*8 + index + 1, Cohort: cohort}
}

// zoneOf is a membership of width nodes per cohort.
func zoneOf(width uint32) Membership {
	var out Membership

	for cohort := range uint32(Cohorts) {
		for index := range width {
			out = append(out, member(cohort, index))
		}
	}

	return out
}

func newZone(t *testing.T, size int, belongs Membership) *zone {
	t.Helper()

	z := &zone{
		t:        t,
		universe: 1,
		epoch:    1,
		size:     size,
		belongs:  belongs,
		ready:    map[uint32]bool{},
	}

	for _, m := range belongs {
		z.ready[m.NodeID] = true
	}

	return z
}

func (z *zone) plan() MembershipPlan {
	admissible := make(Membership, 0, len(z.belongs))

	for _, m := range z.belongs {
		if z.ready[m.NodeID] {
			admissible = append(admissible, m)
		}
	}

	var nodes []NodeState

	for _, m := range append(append(Membership(nil), z.belongs...), z.draining...) {
		nodes = append(nodes, loadedNode(
			fmt.Sprintf("n%d", m.NodeID), m.NodeID, 1, z.universe, z.epoch,
		))
	}

	return MembershipPlan{
		Universe:    z.universe,
		Epoch:       z.epoch,
		CatalogSize: z.size,
		Catalog:     z.catalog,
		Current:     z.current,
		Draining:    z.draining,
		Candidates:  z.belongs,
		Admissible:  admissible,
		Nodes:       nodes,
		Batch:       z.batch,
	}
}

// settle runs passes until the zone stops moving, checking every step against
// the two rules on the way. It fails rather than looping forever, because a
// planner that never settles is a zone that never stops replaying.
func (z *zone) settle(passes int) {
	z.t.Helper()

	for range passes {
		step, gate, err := PlanMembership(z.plan())
		require.NoError(z.t, err)
		require.True(z.t, gate.OK, "gate said %q", gate)

		z.apply(step)

		if step.Done {
			return
		}
	}

	z.t.Fatalf("zone never settled: %s", FormatCatalog(z.catalog))
}

// apply publishes a step the way the operator does, spending an epoch only when
// something actually changed.
func (z *zone) apply(step MembershipStep) {
	z.t.Helper()

	if len(step.Catalog) == 0 {
		return
	}

	if len(z.catalog) > 0 {
		assertStepIsLegal(z.t, z.catalog, step.Catalog, z.current, step.Next)
	}

	require.NoError(z.t, step.Catalog.Validate())

	changed := !step.Catalog.Equal(z.catalog) ||
		!step.Next.Equal(z.current) ||
		!step.Draining.Equal(z.draining)

	z.catalog = step.Catalog
	z.current = step.Next
	z.draining = step.Draining

	if changed && !step.Seeded {
		z.epoch++
	}
}

// assertStepIsLegal is the whole safety argument, written as an assertion: two
// of every group's three nodes survive the change, and the zone as a whole takes
// on at most one newcomer and lets go of at most one node.
func assertStepIsLegal(t *testing.T, before, after Catalog, was, is Membership) {
	t.Helper()

	require.Len(t, after, len(before), "the number of groups a slot folds onto is fixed")

	for i := range before {
		assert.GreaterOrEqualf(t, after[i].Survivors(before[i]), Quorum,
			"group %d went from %v to %v", i, before[i], after[i])
	}

	joins, departures := membershipDelta(was, is)
	assert.LessOrEqual(t, joins, 1, "more than one node joined at once")
	assert.LessOrEqual(t, departures, 1, "more than one node left at once")
}

// balance reports the widest gap between two nodes of the same cohort. Zero or
// one means the zone is as even as its arithmetic allows.
func (z *zone) balance() int {
	load := z.catalog.Load()

	var worst int

	for cohort := range Cohorts {
		holders := columnHolders(z.catalog, cohort)
		if len(holders) == 0 {
			continue
		}

		low, high := load[holders[0]], load[holders[0]]

		for _, id := range holders {
			low = min(low, load[id])
			high = max(high, load[id])
		}

		worst = max(worst, high-low)
	}

	return worst
}

// A zone with no catalog builds one from the nodes it has, and that first
// publication is not a change to anything: every node was already deriving the
// same catalog from the membership, so it costs no epoch.
func TestPlanMembershipSeedsAZoneWithoutMovingIt(t *testing.T) {
	z := newZone(t, 6, zoneOf(1))
	z.current = zoneOf(1)

	step, gate, err := PlanMembership(z.plan())
	require.NoError(t, err)
	require.True(t, gate.OK, "gate said %q", gate)

	require.True(t, step.Done)
	assert.True(t, step.Seeded, "writing down what the nodes already derive is not a change")

	trios, err := BuildCatalog(z.current, z.size)
	require.NoError(t, err)
	assert.Equal(t, CatalogOf(trios), step.Catalog, "seeding moved a group")
}

// A zone with nothing published and not enough nodes to form a catalog waits,
// rather than publishing a catalog it would have to tear up later.
func TestPlanMembershipWaitsForAZoneToExist(t *testing.T) {
	z := newZone(t, 6, Membership{member(0, 0), member(1, 0)})

	step, gate, err := PlanMembership(z.plan())
	require.NoError(t, err)
	require.True(t, gate.OK, "gate said %q", gate)

	assert.True(t, step.Done)
	assert.Empty(t, step.Catalog)
}

// Growing a zone was the unsafe case: the old planner rebuilt the whole catalog
// and skipped a generation to get the result past validation, which could hand a
// group to three nodes that had never held it.
func TestPlanMembershipGrowsAZoneOneGroupAtATime(t *testing.T) {
	z := newZone(t, 12, zoneOf(1))
	z.current = zoneOf(1)
	z.settle(4)

	z.widen(2)
	z.settle(64)

	assert.Len(t, z.catalog.Load(), 2*Cohorts, "the zone did not grow")
	assert.LessOrEqual(t, z.balance(), 1, "the zone grew but stayed lopsided")
}

// Shrinking is the same operation run the other way, and the node on its way out
// has to be named as draining until it says it has handed everything over.
func TestPlanMembershipShrinksAZoneThroughTheDrainingSet(t *testing.T) {
	z := newZone(t, 12, zoneOf(2))
	z.current = zoneOf(2)
	z.settle(32)

	z.belongs = zoneOf(1)

	step, gate, err := PlanMembership(z.plan())
	require.NoError(t, err)
	require.True(t, gate.OK, "gate said %q", gate)
	require.False(t, step.Done)

	z.apply(step)
	z.settle(64)

	assert.Len(t, z.catalog.Load(), Cohorts)
	assert.Empty(t, z.draining, "the zone shrank but never let the drained nodes go")
}

// A zone whose smallest cohort has one node cannot lose it: a group has to name
// somebody in every column. The planner leaves it alone rather than producing a
// catalog with a hole in it.
func TestPlanMembershipWillNotEmptyAColumn(t *testing.T) {
	z := newZone(t, 6, zoneOf(1))
	z.current = zoneOf(1)
	z.settle(4)

	z.belongs = Membership{member(1, 0), member(2, 0)}

	step, gate, err := PlanMembership(z.plan())
	require.NoError(t, err)
	require.True(t, gate.OK, "gate said %q", gate)

	assert.True(t, step.Done, "the planner emptied a cohort")
	assert.True(t, step.Catalog.Members().Contains(member(0, 0).NodeID))
}

// The batch is what keeps a newcomer from replaying a whole zone at once. It
// bounds a step, not the change: the zone still arrives where it was going.
func TestPlanMembershipHonoursTheBatch(t *testing.T) {
	z := newZone(t, 60, zoneOf(1))
	z.current = zoneOf(1)
	z.batch = 4
	z.settle(4)

	z.widen(2)

	before := z.catalog.Clone()

	step, gate, err := PlanMembership(z.plan())
	require.NoError(t, err)
	require.True(t, gate.OK, "gate said %q", gate)
	require.False(t, step.Done)

	var moved int

	for i := range before {
		moved += Cohorts - step.Catalog[i].Survivors(before[i])
	}

	assert.LessOrEqual(t, moved, 4, "a step moved more than the batch")

	z.apply(step)
	z.settle(400)

	assert.Len(t, z.catalog.Load(), 2*Cohorts)
}

// A node that has gone briefly unready still belongs to the zone. Draining it
// because a probe blinked would move a third of the zone and then move it back,
// so an unready node is only ever withheld work.
func TestPlanMembershipKeepsAnUnreadyMember(t *testing.T) {
	z := newZone(t, 12, zoneOf(2))
	z.current = zoneOf(2)
	z.settle(32)

	spare := member(0, 1).NodeID

	held := z.catalog.Load()[spare]
	require.Positive(t, held)

	z.ready[spare] = false

	step, gate, err := PlanMembership(z.plan())
	require.NoError(t, err)
	require.True(t, gate.OK, "gate said %q", gate)

	assert.True(t, step.Done, "an unready node was evicted")
	assert.Equal(t, held, step.Catalog.Load()[spare])
}

// widen sets the zone's membership to width nodes per cohort, all healthy.
func (z *zone) widen(width uint32) {
	z.belongs = zoneOf(width)
	for _, m := range z.belongs {
		z.ready[m.NodeID] = true
	}
}

// Random growth, shrinkage and replacement, checked against the two rules at
// every step. This is the test that would have caught the resize: it is only a
// property of the whole sequence that a group never loses its quorum.
func TestPlanMembershipHoldsTheRulesUnderRandomChange(t *testing.T) {
	for seed := range 64 {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(int64(seed))) //nolint:gosec // reproducible, not cryptographic

			z := newZone(t, 12, zoneOf(1))
			z.current = zoneOf(1)
			z.batch = 1 + rng.Intn(6)
			z.settle(8)

			width := uint32(1)

			for range 6 {
				if width > 1 && rng.Intn(2) == 0 {
					width--
				} else {
					width++
				}

				z.widen(width)
				z.settle(600)

				assert.Len(t, z.catalog.Load(), int(width)*Cohorts,
					"zone did not reach width %d", width)
				assert.LessOrEqualf(t, z.balance(), 1,
					"zone at width %d is lopsided: %s", width, FormatCatalog(z.catalog))
				assert.Empty(t, z.draining, "a node was left draining forever")
			}
		})
	}
}
