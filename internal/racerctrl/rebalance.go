// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package racerctrl

import (
	"fmt"
	"sort"
)

// This file plans how a zone's groups move between its nodes.
//
// Every membership change is the same operation repeated: one node hands one
// group to another node of the same cohort. Joining, leaving, replacing and
// rebalancing are all that, and nothing else, which is why there is one planner
// rather than one per case.
//
// Two rules bound a step, and they are the whole safety argument:
//
//   - Per group, at most one of the three nodes changes. The two that stayed
//     hold every version the group ever agreed, so they can serve reads while
//     the newcomer replays from them. A group that changed two nodes runs on
//     one copy; a group that changed all three has no copy at all, and, worse,
//     looks converged, because three empty replicas agree with each other.
//   - Across the zone, at most one id joins the catalog and at most one leaves.
//     This is not needed for durability - the per-group rule covers that - but
//     it keeps how much of the zone is replaying at once proportional to what
//     one node can take, and it is what makes a departure a thing the control
//     plane can wait on.
//
// A step is therefore never large in ids and may be large in slots. Batch
// bounds the slots, so a node joining a zone of a couple of thousand groups
// arrives over several generations rather than replaying everything at once.

// DefaultMoveBatch is how many groups one step may move.
//
// The bound is about replay, not safety: every group a step moves runs two of
// three until the new node has caught up, and the node taking them replays all
// of them at once. A few hundred is enough for a rebalance to finish in a
// reasonable number of generations and small enough that a zone is never mostly
// degraded.
const DefaultMoveBatch = 256

// MembershipPlan is one zone's membership question: what the catalog holds now,
// who is still draining out of it, who wants in, and what every node reports.
type MembershipPlan struct {
	// Universe and Epoch identify the configuration a step has to have landed
	// before the next one may be taken.
	Universe uint32
	Epoch    uint32

	// CatalogSize is how many groups the zone has. It is fixed for the life of
	// the zone: the group a slot folds onto is derived from this number.
	CatalogSize int

	// Catalog is what is published now, empty before the zone has one.
	Catalog Catalog

	// Current is the membership published alongside that catalog.
	Current Membership

	// Draining are the nodes the catalog has stopped naming that have not yet
	// handed over what they held.
	Draining Membership

	// Candidates is every node that belongs in this zone, whether or not it is
	// healthy right now. Belonging is what decides who is not removed.
	Candidates Membership

	// Admissible is the subset healthy enough to be given groups. A node that
	// has gone briefly unready still belongs, and draining it because a probe
	// blinked would move a quarter of the zone for nothing.
	Admissible Membership

	// Nodes is every node's published state.
	Nodes []NodeState

	// Batch bounds how many groups one step moves. Zero means DefaultMoveBatch.
	Batch int
}

// PlanMembership works out a zone's next catalog, membership and draining set.
//
// The gate is checked before any move is chosen, so a zone in the middle of
// healing is told to wait rather than handed a step on top of the last one.
// Everything after the gate is a pure function of the published state, so a
// restarted operator resumes exactly where it left off without having recorded
// anything.
func PlanMembership(plan MembershipPlan) (MembershipStep, Gate, error) {
	if plan.CatalogSize <= 0 {
		return MembershipStep{}, Gate{}, fmt.Errorf("catalog size must be positive, got %d", plan.CatalogSize)
	}

	byID := make(map[uint32]NodeState, len(plan.Nodes))
	for _, node := range plan.Nodes {
		byID[node.ID] = node
	}

	// Whoever has finished draining leaves the set. Whoever has not stays, and
	// is also what the quiesce gate below is asked about: the departing node is
	// where the shedding is, and leaving it out of the question is how the next
	// change starts on top of the last one.
	var draining Membership

	for _, member := range plan.Draining.Normalized() {
		node, ok := byID[member.NodeID]
		if !ok {
			// The Node object is gone. Nothing else will ever report on it, so
			// holding the set open forever would block every later step.
			continue
		}

		if gate := DrainComplete(node, plan.Universe, plan.Epoch); gate.OK {
			continue
		}

		draining = append(draining, member)
	}

	// A node on its way out must not be handed back the groups it is in the
	// middle of giving away.
	belongs := withoutMembers(plan.Candidates, draining)
	admissible := withoutMembers(plan.Admissible, draining)

	seeded, err := seedCatalog(plan, admissible)
	if err != nil {
		return MembershipStep{}, Gate{}, err
	}

	if len(seeded) == 0 {
		// The zone has no catalog and not enough nodes to form one. It waits.
		return MembershipStep{Done: true, Draining: draining}, Allow(), nil
	}

	plan.Catalog = seeded

	step, err := moveGroups(plan, belongs, admissible)
	if err != nil {
		return MembershipStep{}, Gate{}, err
	}

	step.Draining = draining

	// Seeding writes down what the zone's nodes were already deriving for
	// themselves, so on its own it is not a change to anything and does not
	// spend an epoch. It only counts as one if this same pass then moves a slot.
	step.Seeded = len(plan.Current) > 0 && step.Done

	if step.Done && draining.Equal(plan.Draining) {
		return step, Allow(), nil
	}

	// The gate asks the nodes that hold groups now, plus the ones still handing
	// theirs over. Asking the candidates instead would let a node that has never
	// run racer block the very step that would put it to work.
	holding := append(append(Membership(nil), plan.Current...), draining...)

	if gate := HealingQuiesced(membersOf(holding, plan.Nodes), plan.Universe, plan.Epoch); !gate.OK {
		return MembershipStep{}, gate, nil
	}

	if step.Done {
		// Only the draining set shrank. Publish that and nothing else. It is
		// still worth an epoch: the draining set decides who the survivors link
		// to and admit, so it is part of what a node has to have loaded.
		step.Next = plan.Current
		step.Catalog = plan.Catalog
		step.Done = false
		step.Seeded = false
		step.Reason = "retiring a drained node"

		return step, Allow(), nil
	}

	// Whoever the step drops has not drained yet by definition, so it joins the
	// set the next pass will wait on.
	step.Draining = append(draining, departed(plan.Current, step.Next)...).Normalized()

	return step, Allow(), nil
}

// seedCatalog is the catalog a zone starts a pass from: the published one, or a
// freshly built one when the zone has never published any.
//
// Building from a member list is safe exactly once, when there is nothing yet to
// preserve. It also covers a zone published before catalogs were written down:
// its members are already agreed, so building from them reproduces exactly the
// catalog every node was deriving for itself and moves nothing.
//
// An empty result means the zone has no catalog and not enough nodes to form
// one.
func seedCatalog(plan MembershipPlan, admissible Membership) (Catalog, error) {
	if len(plan.Catalog) > 0 {
		return plan.Catalog, nil
	}

	seed := plan.Current
	if len(seed) == 0 {
		seed = DesiredMembership(admissible, plan.CatalogSize)
	}

	if len(seed) == 0 {
		return nil, nil
	}

	trios, err := BuildCatalog(seed, plan.CatalogSize)
	if err != nil {
		return nil, fmt.Errorf("seed catalog: %w", err)
	}

	catalog := CatalogOf(trios)
	if err := catalog.Validate(); err != nil {
		return nil, fmt.Errorf("seed catalog: %w", err)
	}

	return catalog, nil
}

// moveGroups picks this step's slot moves.
//
// Exactly one kind of move is chosen per step, in priority order: emptying a
// node that no longer belongs, filling a node that has just arrived, then
// levelling what is left. Working in one column at a time is what keeps the
// per-group rule: a column holds one cohort's slot in every group, so touching
// only one column cannot change two members of the same group.
func moveGroups(plan MembershipPlan, belongs, admissible Membership) (MembershipStep, error) {
	catalog := plan.Catalog.Clone()

	if len(catalog) != plan.CatalogSize {
		return MembershipStep{}, fmt.Errorf(
			"published catalog has %d groups, universe says %d; len(catalog) is fixed for the life of a zone",
			len(catalog), plan.CatalogSize,
		)
	}

	batch := plan.Batch
	if batch <= 0 {
		batch = DefaultMoveBatch
	}

	if moved, reason := evictOne(catalog, belongs, admissible, batch); moved {
		return finishStep(catalog, reason)
	}

	if moved, reason := admitOne(catalog, admissible, batch); moved {
		return finishStep(catalog, reason)
	}

	if moved, reason := levelColumns(catalog, admissible, batch); moved {
		return finishStep(catalog, reason)
	}

	return MembershipStep{
		Next:    catalog.Members(),
		Catalog: catalog,
		Done:    true,
		Reason:  "settled",
	}, nil
}

func finishStep(catalog Catalog, reason string) (MembershipStep, error) {
	if err := catalog.Validate(); err != nil {
		return MembershipStep{}, err
	}

	return MembershipStep{
		Next:    catalog.Members(),
		Catalog: catalog,
		Reason:  reason,
	}, nil
}

// evictOne moves groups off the lowest-numbered node the zone no longer wants.
//
// One node at a time, because a departure is a thing the control plane has to
// wait on: the node keeps running a configuration that names the universe until
// it has confirmed the new holders have everything, and two of those at once
// would make the wait ambiguous.
func evictOne(catalog Catalog, belongs, admissible Membership, batch int) (bool, string) {
	load := catalog.Load()

	for cohort := range Cohorts {
		var (
			victim uint32
			held   int
		)

		for _, id := range sortedIDs(load) {
			if belongs.Contains(id) || !inColumn(catalog, cohort, id) {
				continue
			}

			victim, held = id, load[id]

			break
		}

		if victim == 0 {
			continue
		}

		takers := columnTakers(cohort, admissible, victim)
		if len(takers) == 0 {
			// Nobody left in this cohort to hand the groups to. Refusing to move
			// is the right answer: a group cannot be left with a hole.
			continue
		}

		moved := redistribute(catalog, cohort, load, takers, func(id uint32) bool {
			return id == victim
		}, batch, 0)

		if moved == 0 {
			continue
		}

		if moved >= held {
			return true, fmt.Sprintf("node %d handed over its last %d groups", victim, moved)
		}

		return true, fmt.Sprintf("moving %d of node %d's %d groups off it", moved, victim, held)
	}

	return false, ""
}

// admitOne starts giving groups to one node that belongs but holds none.
//
// The groups come from whichever holder in its column has the most, so the
// column levels out as the newcomer fills up rather than after it.
func admitOne(catalog Catalog, admissible Membership, batch int) (bool, string) {
	load := catalog.Load()

	for _, member := range admissible.Normalized() {
		if load[member.NodeID] > 0 || member.Cohort >= Cohorts {
			continue
		}

		cohort := int(member.Cohort)

		holders := columnHolders(catalog, cohort)
		if len(holders) == 0 || len(holders) >= len(catalog) {
			// A column with as many nodes as groups has nothing left to give:
			// racer refuses a catalog naming a node that holds nothing.
			continue
		}

		target := len(catalog) / (len(holders) + 1)
		if target <= 0 {
			continue
		}

		want := min(target, batch)

		moved := take(catalog, cohort, load, member.NodeID, want)
		if moved == 0 {
			continue
		}

		return true, fmt.Sprintf("node %d took its first %d groups", member.NodeID, moved)
	}

	return false, ""
}

// levelColumns evens out a column whose nodes hold unequal shares.
//
// A node's share of a zone is exactly the groups it holds, so an uneven column
// is uneven capacity and uneven load. Levelling never changes who is in the
// catalog, which is why it comes last: admissions and departures are the moves
// that have somebody waiting on them.
func levelColumns(catalog Catalog, admissible Membership, batch int) (bool, string) {
	load := catalog.Load()

	for cohort := range Cohorts {
		holders := columnHolders(catalog, cohort)
		if len(holders) < 2 {
			continue
		}

		fair := len(catalog) / len(holders)

		takers := make([]uint32, 0, len(holders))

		for _, id := range holders {
			if load[id] < fair && admissible.Contains(id) {
				takers = append(takers, id)
			}
		}

		if len(takers) == 0 {
			continue
		}

		// Slots move off whoever is above the fair share and stop as soon as
		// the node taking them reaches it. Without that ceiling on the taker,
		// an uneven split moves a slot back and forth forever: somebody has to
		// hold the remainder, and every pass would decide it should be somebody
		// else.
		moved := redistribute(catalog, cohort, load, takers, func(id uint32) bool {
			return load[id] > fair
		}, batch, fair)

		if moved == 0 {
			continue
		}

		return true, fmt.Sprintf("levelling cohort %d, %d groups moved", cohort, moved)
	}

	return false, ""
}

// redistribute moves up to batch of one column's slots away from the nodes over
// picks, handing each to whichever taker holds the fewest at that moment.
//
// A ceiling of zero means a taker will take whatever it is given, which is what
// emptying a departing node needs: its groups have to land somewhere.
func redistribute(
	catalog Catalog,
	cohort int,
	load map[uint32]int,
	takers []uint32,
	over func(uint32) bool,
	batch int,
	ceiling int,
) int {
	var moved int

	for i := range catalog {
		if moved >= batch {
			break
		}

		holder := catalog[i][cohort]
		if holder == 0 || !over(holder) {
			continue
		}

		taker := leastLoadedTaker(takers, load, catalog[i], ceiling)
		if taker == 0 {
			continue
		}

		catalog[i][cohort] = taker
		load[holder]--
		load[taker]++
		moved++
	}

	return moved
}

// take moves up to want of a column's slots onto one node, always from whoever
// currently holds the most.
func take(catalog Catalog, cohort int, load map[uint32]int, taker uint32, want int) int {
	var moved int

	for moved < want {
		donor := mostLoadedInColumn(catalog, cohort, load, taker)
		if donor == 0 || load[donor] <= 1 {
			break
		}

		var took bool

		for i := range catalog {
			if catalog[i][cohort] != donor || catalog[i].Contains(taker) {
				continue
			}

			catalog[i][cohort] = taker
			load[donor]--
			load[taker]++
			moved++
			took = true

			break
		}

		if !took {
			break
		}
	}

	return moved
}

// leastLoadedTaker picks the taker holding the fewest groups that is not already in
// the group, because a group may not name the same node twice.
func leastLoadedTaker(takers []uint32, load map[uint32]int, group Group, ceiling int) uint32 {
	var (
		best  uint32
		count int
	)

	for _, id := range takers {
		if group.Contains(id) {
			continue
		}

		if ceiling > 0 && load[id] >= ceiling {
			continue
		}

		if best == 0 || load[id] < count {
			best, count = id, load[id]
		}
	}

	return best
}

// mostLoadedInColumn is the node holding the most of a column's slots, ignoring
// the one being filled.
func mostLoadedInColumn(catalog Catalog, cohort int, load map[uint32]int, skip uint32) uint32 {
	var (
		best  uint32
		count int
	)

	for _, id := range columnHolders(catalog, cohort) {
		if id == skip {
			continue
		}

		if best == 0 || load[id] > count {
			best, count = id, load[id]
		}
	}

	return best
}

// columnHolders is every node appearing in one column, sorted.
func columnHolders(catalog Catalog, cohort int) []uint32 {
	seen := map[uint32]struct{}{}

	for _, group := range catalog {
		if group[cohort] != 0 {
			seen[group[cohort]] = struct{}{}
		}
	}

	ids := make([]uint32, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}

	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })

	return ids
}

// columnTakers is who in a cohort may be given groups, excluding one id.
func columnTakers(cohort int, admissible Membership, exclude uint32) []uint32 {
	var takers []uint32

	for _, member := range admissible.Normalized() {
		if member.Cohort != uint32(cohort) || member.NodeID == exclude {
			continue
		}

		takers = append(takers, member.NodeID)
	}

	return takers
}

// inColumn reports whether a node appears in one column of the catalog.
func inColumn(catalog Catalog, cohort int, id uint32) bool {
	for _, group := range catalog {
		if group[cohort] == id {
			return true
		}
	}

	return false
}

// sortedIDs orders a load map so a plan is the same every time it is computed.
func sortedIDs(load map[uint32]int) []uint32 {
	ids := make([]uint32, 0, len(load))
	for id := range load {
		ids = append(ids, id)
	}

	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })

	return ids
}
