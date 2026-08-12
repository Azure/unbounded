// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package racer

import (
	"context"
	"fmt"
	"strings"

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

// collectDeletedVolumes drives a deleted volume's extents to nothing and then
// lets the object go.
//
// Advancing tombstone_epoch is the destructive act: it declares every page
// written before that epoch dead, and nothing brings them back. It is done here,
// and only here, because deleting the PersistentVolume under a Delete reclaim
// policy is the one place a human has actually asked for the data to be
// destroyed. This is deliberately not the same thing as R6's garbage collection,
// which advances the epoch only once every node already reports zero live pages;
// here the pages are live and destroying them is the request.
//
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
		epoch := view.state.Epoch
		if epoch <= volume.state.TombstoneEpoch {
			epoch = volume.state.TombstoneEpoch + 1
		}

		annotations := map[string]string{
			racerctrl.PhaseAnnotation:          racerctrl.PhaseCollecting,
			racerctrl.TombstoneEpochAnnotation: formatUint(uint64(epoch)),
		}

		if err := p.patchVolume(ctx, volume, annotations, nil); err != nil {
			return err
		}

		p.wait("volume %s tombstoned at epoch %d", volume.pv.Name, epoch)

		return nil
	}

	if gate := racerctrl.CollectionDrained(volume.state, states); !gate.OK {
		p.wait("volume %s collection: %s", volume.pv.Name, gate)

		return nil
	}

	return p.patchVolume(ctx, volume, nil, withoutFinalizer(volume.pv.Finalizers, racerctrl.VolumeFinalizer))
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
// a node.
func (p *pass) membershipsHolding(nodeID uint32) []string {
	var held []string

	for i := range p.universes {
		view := &p.universes[i]

		for _, members := range view.state.Members {
			if members.Contains(nodeID) {
				held = append(held, view.class.Name)

				break
			}
		}
	}

	return held
}
