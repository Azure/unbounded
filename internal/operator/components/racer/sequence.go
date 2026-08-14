// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package racer

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/Azure/unbounded/internal/racerctrl"
)

// sequence runs the state machines R6 describes.
//
// Every one of them is gated on what the nodes publish about the dataplane, and
// none of them is gated on a timer. That is the whole point: the control plane
// cannot see a replication group, so the only honest way to know a step landed
// is to watch the counters the dataplane exports and refuse to take the next
// step until they say so.
func (p *pass) sequence(ctx context.Context) error {
	p.reportStoreGrowth()

	if err := p.finishMigrations(ctx); err != nil {
		return err
	}

	if err := p.collectDeletedVolumes(ctx); err != nil {
		return err
	}

	if err := p.reclaimDeadExtents(ctx); err != nil {
		return err
	}

	if err := p.recoverOrphanVolumes(ctx); err != nil {
		return err
	}

	if err := p.collectDeletedUniverses(ctx); err != nil {
		return err
	}

	return p.retireDecommissionedNodes(ctx)
}

// reportStoreGrowth surfaces nodes whose allocator has run out of backing.
//
// The operator cannot fix this itself and pretending otherwise would be worse
// than saying so. Store size is derived by each node from its own placement and
// written to its own annotation, and Node.store is cold: a larger size takes
// effect at the next process start. So the honest report is the list of nodes
// that need restarting.
func (p *pass) reportStoreGrowth() {
	needed := racerctrl.StoreGrowthNeeded(p.nodeStates())
	if len(needed) == 0 {
		return
	}

	p.wait("nodes need a larger store and a restart to pick it up: %s", strings.Join(needed, ", "))
}

// finishMigrations declares an extent migration complete.
//
// The dataplane never declares this itself. Setting next_zone starts the copy;
// the control plane is what decides it is done, by comparing the destination's
// live pages against the source's, and only then moves zone to what next_zone
// held. R5 permits the home zone to move to exactly that value and no other,
// which is why the move and the clear happen in the same write.
func (p *pass) finishMigrations(ctx context.Context) error {
	states := p.nodeStates()

	for i := range p.universes {
		view := &p.universes[i]

		for j := range view.volumes {
			volume := &view.volumes[j]
			if volume.state.NextZone == 0 || volume.pv.DeletionTimestamp != nil {
				continue
			}

			gate := racerctrl.MigrationComplete(volume.state, states)
			if !gate.OK {
				p.wait("volume %s migration: %s", volume.pv.Name, gate)

				continue
			}

			annotations := map[string]string{
				racerctrl.VolumeZoneAnnotation: formatUint(uint64(volume.state.NextZone)),
				racerctrl.NextZoneAnnotation:   "",
				racerctrl.PhaseAnnotation:      racerctrl.PhaseActive,
			}

			if err := p.patchVolume(ctx, volume, annotations, nil); err != nil {
				return err
			}
		}
	}

	return nil
}

// reclaimDeadExtents is R6's garbage collection: it advances the tombstone
// epoch of an extent every node already reports as holding no live pages.
//
// This is the counterpart to collectDeletedVolumes and the opposite case. There,
// live pages are destroyed because a human asked for the volume to go. Here
// nothing live is destroyed, because a guest has already trimmed every page and
// the only thing an advance releases is the tombstones those trims left behind.
// racer holds a trimmed 4 MiB page's slot until the epoch moves, so without this
// a guest that fills and trims an extent can never reuse the space.
//
// A trimmed page is unreadable and unwritable already, so the advance changes
// nothing a guest can observe except that the addresses become writable again.
// That is why it needs no phase, no drain and no finalizer: the guest performed
// the destructive step itself when it issued the discard, and it is the guest's
// job to have stopped reading the extent first.
func (p *pass) reclaimDeadExtents(ctx context.Context) error {
	states := p.nodeStates()

	for i := range p.universes {
		view := &p.universes[i]
		if view.state.Deleting {
			continue
		}

		for j := range view.volumes {
			volume := &view.volumes[j]
			if volume.pv.DeletionTimestamp != nil || len(volume.state.Composition) == 0 {
				continue
			}

			advanced := racerctrl.ReclaimableExtents(volume.state, states)
			if len(advanced) == 0 {
				continue
			}

			epochs := make(map[uint32]uint32, len(volume.state.Composition))
			for id, epoch := range volume.state.TombstoneEpochs {
				epochs[id] = epoch
			}

			for id, epoch := range advanced {
				epochs[id] = epoch
			}

			annotations := map[string]string{
				racerctrl.TombstoneEpochAnnotation: racerctrl.FormatTombstoneEpochs(epochs),
			}

			if err := p.patchVolume(ctx, volume, annotations, nil); err != nil {
				return err
			}
		}
	}

	return nil
}

// collectDeletedVolumes drives a deleted volume's extents to nothing and then

// The finalizer is what keeps the composition readable while that happens. Drop
// it early and the extent ids, base addresses and page counts vanish with the
// object, leaving every node holding pages that nothing can name.
func (p *pass) collectDeletedVolumes(ctx context.Context) error {
	states := p.nodeStates()

	for i := range p.universes {
		view := &p.universes[i]

		for j := range view.volumes {
			volume := &view.volumes[j]
			if volume.pv.DeletionTimestamp == nil || !hasFinalizer(volume.pv.Finalizers, racerctrl.VolumeFinalizer) {
				continue
			}

			if err := p.collectVolume(ctx, view, volume, states); err != nil {
				return err
			}
		}
	}

	return nil
}

func (p *pass) collectVolume(ctx context.Context, view *universeView, volume *volumeView, states []racerctrl.NodeState) error {
	if len(volume.state.Composition) == 0 {
		// Nothing was ever placed, so there is nothing to collect.
		return p.patchVolume(ctx, volume, nil, withoutFinalizer(volume.pv.Finalizers, racerctrl.VolumeFinalizer))
	}

	if volume.state.Phase != racerctrl.PhaseCollecting {
		// Every extent goes at once: the request was to destroy the volume, so
		// there is no extent worth keeping. The universe's topology epoch is a
		// number nothing else in this volume can already be at, which saves
		// picking one per extent.
		epochs := make(map[uint32]uint32, len(volume.state.Composition))

		highest := view.state.Epoch

		for _, segment := range volume.state.Composition {
			if at := volume.state.TombstoneEpochs[segment.ExtentID]; at >= highest {
				highest = at + 1
			}
		}

		for _, segment := range volume.state.Composition {
			epochs[segment.ExtentID] = highest
		}

		annotations := map[string]string{
			racerctrl.PhaseAnnotation:          racerctrl.PhaseCollecting,
			racerctrl.TombstoneEpochAnnotation: racerctrl.FormatTombstoneEpochs(epochs),
		}

		if err := p.patchVolume(ctx, volume, annotations, nil); err != nil {
			return err
		}

		p.wait("volume %s tombstoned at epoch %d", volume.pv.Name, highest)

		return nil
	}

	if gate := racerctrl.CollectionDrained(volume.state, states); !gate.OK {
		p.wait("volume %s collection: %s", volume.pv.Name, gate)

		return nil
	}

	return p.patchVolume(ctx, volume, nil, withoutFinalizer(volume.pv.Finalizers, racerctrl.VolumeFinalizer))
}

// recoverOrphanVolumes releases finalizers that can no longer protect data.
//
// A volume cannot be associated with a universe after its StorageClass is gone:
// the class was the only object carrying the universe id. Nodes have already
// stopped deriving that universe, so retaining the finalizer only makes a
// deleting PV permanent. Non-deleting orphans are left intact and reported.
func (p *pass) recoverOrphanVolumes(ctx context.Context) error {
	for i := range p.orphans {
		volume := &p.orphans[i]
		if volume.pv.DeletionTimestamp != nil && hasFinalizer(volume.pv.Finalizers, racerctrl.VolumeFinalizer) {
			if err := p.patchVolume(ctx, volume, nil,
				withoutFinalizer(volume.pv.Finalizers, racerctrl.VolumeFinalizer)); err != nil {
				return err
			}

			p.wait("volume %s lost racer storage class %s; released its collection finalizer",
				volume.pv.Name, volume.pv.Spec.StorageClassName)

			continue
		}

		p.wait("volume %s references missing racer storage class %s",
			volume.pv.Name, volume.pv.Spec.StorageClassName)
	}

	return nil
}

// collectDeletedUniverses removes a universe only after every volume is gone.
// The class finalizer keeps its id, topology, and membership readable until
// volume collection has completed.
func (p *pass) collectDeletedUniverses(ctx context.Context) error {
	for i := range p.universes {
		view := &p.universes[i]
		if !view.state.Deleting || !hasFinalizer(view.class.Finalizers, racerctrl.UniverseFinalizer) {
			continue
		}

		if len(view.volumes) > 0 {
			names := make([]string, 0, len(view.volumes))
			for j := range view.volumes {
				names = append(names, view.volumes[j].pv.Name)
			}

			p.wait("universe %s is deleting and still owns volumes: %s", view.class.Name, strings.Join(names, ", "))

			continue
		}

		for key, membership := range p.memberships {
			if key.universe != view.state.ID {
				continue
			}

			if err := p.env.Client.Delete(ctx, membership); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete membership %s/%s: %w", membership.Namespace, membership.Name, err)
			}

			delete(p.memberships, key)
		}

		if err := p.patchClassMeta(ctx, view, nil,
			withoutFinalizer(view.class.Finalizers, racerctrl.UniverseFinalizer)); err != nil {
			return err
		}
	}

	return nil
}

// retireDecommissionedNodes takes a node's identity away once it holds nothing.
//
// Un-enrolling a node drops it out of every catalog's candidate set, which makes
// the membership sequencer step it out one universe at a time. Only when the
// node reports that it has finished shedding and holds no live pages is it safe
// to forget who it was; doing it earlier would strand whatever it had not handed
// over yet, and R6 is explicit that we keep serving the config that removes the
// node until it has shed.
//
// The workload label goes in the same write. Up to this point the node has been
// running racer precisely so it could shed; this is the first moment at which
// there is nothing left for it to serve, so it is also the first moment at which
// the pod can go.
func (p *pass) retireDecommissionedNodes(ctx context.Context) error {
	for i := range p.nodes {
		view := &p.nodes[i]

		if view.enrolled || view.state.ID == 0 {
			continue
		}

		if held := p.membershipsHolding(view.state.ID); len(held) > 0 {
			p.wait("node %s is still in the catalog of %s", view.node.Name, strings.Join(held, ", "))

			continue
		}

		if gate := racerctrl.DecommissionComplete(view.state); !gate.OK {
			p.wait("node %s decommission: %s", view.node.Name, gate)

			continue
		}

		annotations := map[string]string{
			racerctrl.NodeIDAnnotation:     "",
			racerctrl.NodeZoneAnnotation:   "",
			racerctrl.NodeCohortAnnotation: "",
		}

		labels := map[string]string{WorkloadLabel: ""}

		if err := p.patchNodeMeta(ctx, view, annotations, labels); err != nil {
			return fmt.Errorf("retire node %s: %w", view.node.Name, err)
		}
	}

	return nil
}

// membershipsHolding names the universes whose published membership still lists
// a node, either as a catalog member or as one still draining out of it.
//
// The draining half is what keeps a decommission honest. A node the catalog has
// stopped naming is still holding registers until it has handed them over, and
// its identity is what the survivors' allowed hosts and peer lists are written
// against, so clearing it while the set still names it would cut the links the
// handover runs over.
func (p *pass) membershipsHolding(nodeID uint32) []string {
	var held []string

	for i := range p.universes {
		view := &p.universes[i]

		if universeHolds(view.state, nodeID) {
			held = append(held, view.class.Name)
		}
	}

	return held
}

// universeHolds reports whether any of a universe's zones still names a node.
func universeHolds(state racerctrl.UniverseState, nodeID uint32) bool {
	for _, members := range state.Members {
		if members.Contains(nodeID) {
			return true
		}
	}

	for _, draining := range state.Draining {
		if draining.Contains(nodeID) {
			return true
		}
	}

	return false
}
