// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package racerctrl

import (
	"fmt"
	"sort"
)

// Sequenced operations.
//
// R6 names five operations that cannot be expressed as a single config edit.
// Each one is a state machine whose transitions are gated on what the dataplane
// reports, because each one is a step that is only safe once the previous step
// has finished, and racer has no way to say "I have finished" other than through
// its metrics.
//
// Everything here is a predicate over published state: no clock, no I/O, no
// stored progress. That is deliberate. A sequencer that remembered where it was
// would have to be restarted correctly; one that re-derives its position from
// what the cluster currently reports is correct after any restart, including one
// that happened between the two halves of a step.
//
// The predicates return a reason as well as a verdict. A stalled sequence is the
// normal way these fail, and a stall with no explanation is indistinguishable
// from a bug.

// Gate is the outcome of a sequencing decision: whether to proceed, and why not
// if the answer is no.
type Gate struct {
	OK     bool
	Reason string
}

// Allow is a Gate that permits the next step.
func Allow() Gate { return Gate{OK: true} }

// Block is a Gate that holds the sequence where it is.
func Block(format string, args ...any) Gate {
	return Gate{Reason: fmt.Sprintf(format, args...)}
}

// String renders the gate for a status condition.
func (g Gate) String() string {
	if g.OK {
		return "ready"
	}

	return g.Reason
}

// HealingQuiesced reports whether every node has finished the healing the last
// membership change set off.
//
// This is R6's membership gate. racer_heal_groups_replaying counts groups
// pulling state they have just been made responsible for;
// racer_heal_groups_shedding counts groups handing state away. Starting a second
// replacement while either is nonzero would ask a node to shed a group it is
// still replaying, and the group would lose its only complete copy.
func HealingQuiesced(nodes []NodeState) Gate {
	for _, node := range orderedNodes(nodes) {
		if node.Health.Generation == 0 {
			return Block("node %s has not reported yet", node.Name)
		}

		if node.Health.Replaying > 0 {
			return Block("node %s is still replaying %d groups", node.Name, node.Health.Replaying)
		}

		if node.Health.Shedding > 0 {
			return Block("node %s is still shedding %d groups", node.Name, node.Health.Shedding)
		}
	}

	return Allow()
}

// GenerationConverged reports whether every node has loaded at least the given
// generation, and rejected none.
//
// racer_config_rejected_total is the only signal that a node refused a config.
// A node that rejected ours is running the previous generation, so the cluster
// is split across two generations and no further step is safe until it is fixed.
func GenerationConverged(nodes []NodeState, generation uint64) Gate {
	for _, node := range orderedNodes(nodes) {
		if node.Health.RejectedTotal > 0 {
			return Block("node %s has rejected %d configs", node.Name, node.Health.RejectedTotal)
		}

		if node.Health.Generation < generation {
			return Block(
				"node %s is at generation %d, not yet %d",
				node.Name, node.Health.Generation, generation,
			)
		}
	}

	return Allow()
}

// MigrationComplete reports whether an extent migration has landed.
//
// R6 says to judge from the destination's racer_extent_live_pages against the
// source's. There is no completion signal in the dataplane: migration is copy
// plus a control plane declaration, and the declaration is only honest once the
// destination holds everything the source does. Comparing counts rather than
// waiting for a flag is what makes the decision restartable.
func MigrationComplete(volume VolumeState, nodes []NodeState) Gate {
	if volume.NextZone == 0 {
		return Block("volume %s has no migration in flight", volume.Name)
	}

	for _, segment := range volume.Composition {
		source := livePagesInZone(nodes, volume.Zone, segment.ExtentID)
		destination := livePagesInZone(nodes, volume.NextZone, segment.ExtentID)

		if destination < source {
			return Block(
				"volume %s extent %d: zone %d holds %d live pages, zone %d holds %d",
				volume.Name, segment.ExtentID,
				volume.NextZone, destination, volume.Zone, source,
			)
		}
	}

	return Allow()
}

// CollectionSafe reports whether a volume's tombstone epoch may be advanced.
//
// Advancing tombstone_epoch is the one destructive edit in the schema: it tells
// every node that registers below the new epoch may be dropped. R6 allows it
// only once every node reports zero live pages for the extent, because a live
// page under a collected epoch is data that is gone and that nothing will say is
// gone. The check is over every node, not every node in the home zone: a node
// that still routes for the extent still holds cached registers.
func CollectionSafe(volume VolumeState, nodes []NodeState) Gate {
	for _, segment := range volume.Composition {
		for _, node := range orderedNodes(nodes) {
			if node.Health.Generation == 0 {
				return Block("node %s has not reported yet", node.Name)
			}

			if live := node.Live[segment.ExtentID]; live.Pages > 0 {
				return Block(
					"volume %s extent %d still has %d live pages on node %s",
					volume.Name, segment.ExtentID, live.Pages, node.Name,
				)
			}
		}
	}

	return Allow()
}

// CollectionDrained reports whether the tombstones an advanced epoch released
// have actually been reclaimed. This is the second half of collection: the epoch
// bump authorises the drop, and racer_extent_tombstones falling to zero is the
// evidence it happened. A volume is only safe to forget once both are true.
func CollectionDrained(volume VolumeState, nodes []NodeState) Gate {
	for _, segment := range volume.Composition {
		for _, node := range orderedNodes(nodes) {
			if live := node.Live[segment.ExtentID]; live.Tombstones > 0 {
				return Block(
					"volume %s extent %d still has %d tombstones on node %s",
					volume.Name, segment.ExtentID, live.Tombstones, node.Name,
				)
			}
		}
	}

	return Allow()
}

// StoreGrowthNeeded reports the nodes whose allocator has run out of backing.
//
// racer_alloc_unbacked_pages counts pages the allocator wanted and could not
// back. It is the only evidence that R4's sizing estimate was low, and the only
// remedy is a larger store, which is cold: the node has to restart to pick it
// up. Naming the nodes rather than returning a bare bool is what lets the caller
// restart exactly those.
func StoreGrowthNeeded(nodes []NodeState) []string {
	var needed []string

	for _, node := range orderedNodes(nodes) {
		if node.Health.UnbackedPages > 0 {
			needed = append(needed, node.Name)
		}
	}

	return needed
}

// DecommissionComplete reports whether a node has finished shedding what it
// held. R6's rule is to keep serving the config that removes the node until it
// has shed; pulling the node out earlier strands whatever it had not yet handed
// over.
func DecommissionComplete(node NodeState) Gate {
	if node.Health.Generation == 0 {
		return Block("node %s has not reported yet", node.Name)
	}

	if node.Health.Shedding > 0 {
		return Block("node %s is still shedding %d groups", node.Name, node.Health.Shedding)
	}

	if node.Health.Replaying > 0 {
		return Block("node %s is still replaying %d groups", node.Name, node.Health.Replaying)
	}

	for _, live := range node.Live {
		if live.Pages > 0 {
			return Block("node %s still holds live pages", node.Name)
		}
	}

	return Allow()
}

// PlanMembership works out a zone's next catalog membership.
//
// It is the composition of the two rules that govern membership: R3 wants the
// balanced set the candidates allow, and R6 wants to get there one id at a time
// with the dataplane quiet between steps. The gate is checked first so that a
// cluster in the middle of healing is told to wait rather than handed a step it
// cannot take.
func PlanMembership(current, candidates Membership, catalogSize int, nodes []NodeState) (MembershipStep, Gate, error) {
	desired := DesiredMembership(candidates, catalogSize)

	step, err := NextMembership(current, desired, catalogSize)
	if err != nil {
		return MembershipStep{}, Gate{}, err
	}

	if step.Done {
		return step, Allow(), nil
	}

	if gate := HealingQuiesced(nodes); !gate.OK {
		return MembershipStep{}, gate, nil
	}

	return step, Allow(), nil
}

// livePagesInZone sums an extent's live pages across the nodes of one zone. The
// sum rather than the maximum is what makes the comparison meaningful: an
// extent's pages are spread across its zone's catalog, so no single node holds
// them all and no single node's count says anything on its own.
func livePagesInZone(nodes []NodeState, zone, extent uint32) uint64 {
	var total uint64

	for _, node := range nodes {
		if node.Zone != zone {
			continue
		}

		total += node.Live[extent].Pages
	}

	return total
}

// orderedNodes gives a stable iteration order so that a stalled sequence always
// names the same node as the reason. A reason that rotates between nodes reads
// as flapping when nothing is flapping.
func orderedNodes(nodes []NodeState) []NodeState {
	ordered := append([]NodeState(nil), nodes...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })

	return ordered
}
