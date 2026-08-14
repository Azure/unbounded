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
// Two things are load-bearing and easy to lose.
//
// The first is that a counter of zero and no counter at all are different
// facts, and every gate here is waiting for a zero. A node that has not
// reported, a node whose scrape is failing, and an extent racer has never
// mentioned all read as zero out of a map, and treating that as agreement is
// how a destructive step gets taken against a dataplane nobody has heard from.
// So a gate has to see the report, not the absence of one, which is why
// FormatLive writes explicit zeros for the extents an operation is waiting on
// and why the lookups here take the second return value.
//
// The second is that a generation is not a fact about content. Racer publishes
// which generation is in force and nothing about what is in it, so on its own
// it cannot say a node is acting on the catalog, the migration or the tombstone
// epoch being waited on. NodeState.Applied closes that: the agent says which
// generation carried which facts, racer says which generation is running, and
// the pair is proof. Hence the loaded() check under every gate below.
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

// ConfigLoaded reports whether racer on this node is running the configuration
// its agent last installed.
//
// This is the base of every other gate. Applied.Generation is what the agent
// wrote; Health.Generation is what racer says is in force. Equal or later means
// racer has accepted it, so whatever the agent put in that file is what the
// dataplane is acting on. Earlier means the file is not in force yet, either
// because racer has not got to it or because it refused it - and a refusal is
// exactly the case where reading the counters as agreement would be worst, so
// the same check covers both and racer_config_rejected_total needs no separate
// gate.
func ConfigLoaded(node NodeState) Gate {
	if node.Applied.Generation == 0 {
		return Block("node %s has not published a config yet", node.Name)
	}

	if node.Health.Generation == 0 {
		return Block("node %s has not reported yet", node.Name)
	}

	if node.Health.Generation < node.Applied.Generation {
		return Block(
			"node %s is running generation %d, not yet the %d its agent installed",
			node.Name, node.Health.Generation, node.Applied.Generation,
		)
	}

	return Allow()
}

// HealingQuiesced reports whether every node has loaded a universe's current
// catalog and finished the healing the last membership change set off.
//
// This is R6's membership gate. racer_heal_groups_replaying counts groups
// pulling state they have just been made responsible for;
// racer_heal_groups_shedding counts groups handing state away. Starting a second
// replacement while either is nonzero would ask a node to shed a group it is
// still replaying, and the group would lose its only complete copy.
//
// The epoch check is what makes the counters mean anything. Both counters are
// derived from the catalog racer currently holds, so a node still running the
// catalog from before the last step reports quiet about the wrong topology: it
// has nothing to replay because it has not been told to. Requiring the node's
// installed configuration to carry this universe at this epoch, and racer to be
// running it, is what turns "quiet" into "quiet about the change we made".
func HealingQuiesced(nodes []NodeState, universe, epoch uint32) Gate {
	for _, node := range orderedNodes(nodes) {
		if gate := ConfigLoaded(node); !gate.OK {
			return gate
		}

		if applied := node.Applied.Epochs[universe]; applied < epoch {
			return Block(
				"node %s is running universe %d at epoch %d, not yet %d",
				node.Name, universe, applied, epoch,
			)
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

// MigrationComplete reports whether an extent migration has landed.
//
// R6 says to judge from the destination's racer_extent_live_pages against the
// source's. There is no completion signal in the dataplane: migration is copy
// plus a control plane declaration, and the declaration is only honest once the
// destination holds everything the source does. Comparing counts rather than
// waiting for a flag is what makes the decision restartable.
//
// Every node on either side of the move has to have loaded a configuration that
// actually points the extent at the destination, and to have reported that
// extent by name. Without both, a destination that has never heard of the
// extent reports the same zero as a destination that has finished, and the
// comparison declares a migration complete before it has started.
func MigrationComplete(volume VolumeState, nodes []NodeState) Gate {
	if volume.NextZone == 0 {
		return Block("volume %s has no migration in flight", volume.Name)
	}

	carriers := carriers(volume, nodes)

	for _, segment := range volume.Composition {
		for _, node := range carriers {
			if gate := ConfigLoaded(node); !gate.OK {
				return gate
			}

			if applied := node.Applied.Extents[segment.ExtentID]; applied.NextZone != volume.NextZone {
				return Block(
					"node %s is not yet migrating volume %s extent %d to zone %d",
					node.Name, volume.Name, segment.ExtentID, volume.NextZone,
				)
			}

			if _, ok := node.Live[segment.ExtentID]; !ok {
				return Block(
					"node %s has not reported volume %s extent %d",
					node.Name, volume.Name, segment.ExtentID,
				)
			}
		}

		source := livePagesInZone(carriers, volume.Zone, segment.ExtentID)
		destination := livePagesInZone(carriers, volume.NextZone, segment.ExtentID)

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

// CollectionDrained reports whether the tombstones an advanced epoch released
// have actually been reclaimed. This is the second half of collection: the epoch
// bump authorises the drop, and racer_extent_tombstones falling to zero is the
// evidence it happened. A volume is only safe to forget once both are true.
//
// It is the last gate before a PersistentVolume's finalizer comes off, which
// makes it the most destructive one here: once the object is gone there is
// nothing left that names the extent, so anything still holding it holds it
// forever. So it demands the full proof. Every node that carries the extent has
// to be running a configuration its agent installed, that configuration has to
// carry the tombstone epoch being collected, and the node has to have reported
// this extent by name. A node that has not loaded the epoch has not been asked
// to drop anything, and its zero is the zero of a node that was never told.
func CollectionDrained(volume VolumeState, nodes []NodeState) Gate {
	for _, segment := range volume.Composition {
		want := volume.TombstoneEpochs[segment.ExtentID]

		for _, node := range carriers(volume, nodes) {
			if gate := ConfigLoaded(node); !gate.OK {
				return gate
			}

			applied, ok := node.Applied.Extents[segment.ExtentID]
			if !ok || applied.TombstoneEpoch < want {
				return Block(
					"node %s is running volume %s extent %d at tombstone epoch %d, not yet %d",
					node.Name, volume.Name, segment.ExtentID,
					applied.TombstoneEpoch, want,
				)
			}

			live, ok := node.Live[segment.ExtentID]
			if !ok {
				return Block(
					"node %s has not reported volume %s extent %d",
					node.Name, volume.Name, segment.ExtentID,
				)
			}

			if live.Tombstones > 0 {
				return Block(
					"volume %s extent %d still has %d tombstones on node %s",
					volume.Name, segment.ExtentID, live.Tombstones, node.Name,
				)
			}

			if live.Pages > 0 {
				return Block(
					"volume %s extent %d still has %d live pages on node %s",
					volume.Name, segment.ExtentID, live.Pages, node.Name,
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

// DrainComplete reports whether a node the catalog has stopped naming has
// finished handing over the groups it held in one universe.
//
// A shedding node walks each orphaned group, asks the new members whether they
// hold every version, and only then drops its registers. The counter reaching
// zero is the whole of that: it is derived from the catalog racer currently
// holds, so it means something only once the node is running the catalog that
// orphaned the groups, which is what the epoch check establishes.
func DrainComplete(node NodeState, universe, epoch uint32) Gate {
	if gate := ConfigLoaded(node); !gate.OK {
		return gate
	}

	if applied := node.Applied.Epochs[universe]; applied < epoch {
		return Block(
			"node %s is running universe %d at epoch %d, not yet the %d that drops it",
			node.Name, universe, applied, epoch,
		)
	}

	if node.Health.Shedding > 0 {
		return Block("node %s is still shedding %d groups", node.Name, node.Health.Shedding)
	}

	if node.Health.Replaying > 0 {
		return Block("node %s is still replaying %d groups", node.Name, node.Health.Replaying)
	}

	return Allow()
}

// DecommissionComplete reports whether a node has finished shedding what it
// held. R6's rule is to keep serving the config that removes the node until it
// has shed; pulling the node out earlier strands whatever it had not yet handed
// over.
//
// By the time this is asked the node is in no catalog and no draining set, so
// what it is being asked is the weaker question of whether the node is idle. It
// still has to be running its own agent's configuration: a node whose agent
// stopped publishing is a node whose counters describe a topology that no
// longer exists.
func DecommissionComplete(node NodeState) Gate {
	if gate := ConfigLoaded(node); !gate.OK {
		return gate
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

// departed is the members of before that after no longer names.
func departed(before, after Membership) Membership {
	var gone Membership

	for _, member := range before {
		if !after.Contains(member.NodeID) {
			gone = append(gone, member)
		}
	}

	return gone
}

// withoutMembers removes a set of ids from a membership.
func withoutMembers(members, remove Membership) Membership {
	kept := make(Membership, 0, len(members))

	for _, member := range members {
		if !remove.Contains(member.NodeID) {
			kept = append(kept, member)
		}
	}

	return kept
}

// membersOf picks out the nodes a membership names.
//
// The healing gate asks whether the last membership change has settled, which
// is a question about the nodes that hold groups. Asking it of every node in
// the cluster instead would let a node that has never run racer block the very
// step that would put it to work: a candidate's health generation is zero until
// something gives it a config, and nothing gives it a config until a membership
// names it. A node already in the catalog is a different matter - it holds
// groups now, and stepping while it is still replaying is exactly what R6
// forbids - so those are still gated on, and so is anything still draining.
func membersOf(members Membership, nodes []NodeState) []NodeState {
	named := make([]NodeState, 0, len(members))

	for _, node := range nodes {
		if members.Contains(node.ID) {
			named = append(named, node)
		}
	}

	return named
}

// ReclaimableExtents reports the extents of a volume whose pages are all dead
// and can therefore have their tombstone epoch advanced. This is R6's garbage
// collection, as distinct from the collection a volume deletion asks for: here
// nothing live is destroyed, because there is nothing live left.
//
// The rule is the one config.proto states: advance only once every node reports
// racer_extent_live_pages == 0 at the current epoch, with
// racer_extent_tombstones above zero as the evidence there is something to
// collect. A guest that has trimmed every page of an extent produces exactly
// that state and no other, so this needs no channel the node agent does not
// already publish.
//
// An absent reading is not a zero, but for this gate it does not have to be
// treated as one: FormatLive omits an extent only when it has neither live
// pages nor tombstones on that node, which is a node holding nothing and having
// nothing to lose. What matters is that no node reports a live page, and a node
// that reports nothing reports no live page.
//
// The returned epochs are the value each extent should move to, which is one
// past where it is now.
func ReclaimableExtents(volume VolumeState, nodes []NodeState) map[uint32]uint32 {
	if volume.Phase == PhaseCollecting || volume.NextZone != 0 {
		// A volume mid-collection or mid-migration has a destructive edit
		// already in flight; stacking a second one on the same extents would
		// race the gate that is watching the first.
		return nil
	}

	carrying := carriers(volume, nodes)
	if len(carrying) == 0 {
		return nil
	}

	var reclaimable map[uint32]uint32

	for _, segment := range volume.Composition {
		if !KindIsImmutable(segment.Kind) {
			// A mutable page's version is not a function of the epoch, so an
			// advance would not release anything and a tombstone count is not
			// how a mutable class reports free space.
			continue
		}

		var (
			live       uint64
			tombstones uint64
			reported   bool
		)

		for _, node := range carrying {
			if gate := ConfigLoaded(node); !gate.OK {
				// A node whose agent has not installed its configuration is
				// reporting on some earlier one. Its zero is not evidence.
				return nil
			}

			counts, ok := node.Live[segment.ExtentID]
			if !ok {
				continue
			}

			reported = true
			live += counts.Pages
			tombstones += counts.Tombstones
		}

		if !reported || live > 0 || tombstones == 0 {
			continue
		}

		if reclaimable == nil {
			reclaimable = map[uint32]uint32{}
		}

		reclaimable[segment.ExtentID] = volume.TombstoneEpochs[segment.ExtentID] + 1
	}

	return reclaimable
}

// carriers picks the nodes racer ships a volume's extents to: its home zone,
// the zone it is migrating to, and whatever exports it.
//
// This mirrors deriveExtents exactly, and it has to. A gate that asked the
// whole cluster would block on nodes that have never been sent the extent and
// never will be; one that asked only the home zone would miss the destination
// of a migration and the node exporting the volume to a pod, both of which hold
// registers for it.
func carriers(volume VolumeState, nodes []NodeState) []NodeState {
	var carrying []NodeState

	for _, node := range orderedNodes(nodes) {
		if node.Zone == volume.Zone || (volume.NextZone != 0 && node.Zone == volume.NextZone) {
			carrying = append(carrying, node)
			continue
		}

		for _, binding := range node.Devices {
			if binding.Volume == volume.Name {
				carrying = append(carrying, node)
				break
			}
		}
	}

	return carrying
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
