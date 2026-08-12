// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package racer

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Azure/unbounded/internal/racerctrl"
)

// allocate hands out every identifier the dataplane needs and places every
// volume's extents.
//
// It runs in dependency order and each step tolerates the next not being ready
// yet: a node without a zone cannot join a catalog, a universe without a catalog
// cannot host a volume, and in both cases the answer is to record why and look
// again rather than to invent a value.
func (p *pass) allocate(ctx context.Context) error {
	if err := p.ensureDefaultClass(ctx); err != nil {
		return err
	}

	if err := p.allocateNodeIdentities(ctx); err != nil {
		return err
	}

	if err := p.allocateUniverses(ctx); err != nil {
		return err
	}

	if err := p.reconcileMembership(ctx); err != nil {
		return err
	}

	return p.allocateVolumes(ctx)
}

// ensureDefaultClass creates one universe the first time racer is enabled.
//
// It is created rather than applied from a manifest because a StorageClass is
// the universe: its annotations hold the universe id, the catalog size and the
// LBA cursor, and reapplying the manifest every pass would fight the allocator
// for them. Once it exists the operator only ever adds annotations to it.
func (p *pass) ensureDefaultClass(ctx context.Context) error {
	if len(p.universes) > 0 {
		return nil
	}

	binding := storagev1.VolumeBindingWaitForFirstConsumer
	reclaim := corev1.PersistentVolumeReclaimDelete
	expansion := false

	class := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:   defaultClassName,
			Labels: map[string]string{"app.kubernetes.io/part-of": "racer"},
			Annotations: map[string]string{
				racerctrl.CatalogSizeAnnotation: formatUint(uint64(racerctrl.DefaultCatalogSize)),
			},
		},
		Provisioner: racerctrl.DriverName,
		// The node service is the only CSI service racer runs, so nothing
		// creates volumes on demand. Volumes are PersistentVolumes an
		// administrator or a higher-level controller writes, and binding waits
		// for a consumer so the scheduler picks the zone.
		VolumeBindingMode: &binding,
		ReclaimPolicy:     &reclaim,
		// An extent's size is frozen for its life by the address space, not by a
		// driver limitation, so expansion is refused rather than unimplemented.
		AllowVolumeExpansion: &expansion,
	}

	if err := p.env.Client.Create(ctx, class); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}

		return fmt.Errorf("create default racer storage class: %w", err)
	}

	p.wait("created default storage class %s", defaultClassName)

	return nil
}

// allocateNodeIdentities gives every enrolled node the three values racer needs
// before it can serve anything: an id, a zone, and a cohort.
//
// All three are written here rather than claimed by the node itself. Two nodes
// claiming concurrently would each patch a different Node object and both would
// succeed, which is how you end up with two machines answering to the same id
// and a replication group that silently has two copies instead of three.
func (p *pass) allocateNodeIdentities(ctx context.Context) error {
	for i := range p.nodes {
		view := &p.nodes[i]

		if !view.enrolled || view.state.ID != 0 {
			continue
		}

		zoneName := view.node.Labels[ZoneLabel]
		if zoneName == "" {
			// A racer zone is a failure domain, and inventing one for a node
			// that has not declared one would put it in a catalog whose blast
			// radius nobody has reasoned about.
			p.wait("node %s has no %s label", view.node.Name, ZoneLabel)

			continue
		}

		zone, err := p.cursors.ZoneID(zoneName)
		if err != nil {
			return fmt.Errorf("allocate zone id for %s: %w", zoneName, err)
		}

		id, err := p.cursors.AllocateNodeID()
		if err != nil {
			return fmt.Errorf("allocate node id for %s: %w", view.node.Name, err)
		}

		cohort := p.leastLoadedCohort(zone)

		if err := p.commitCursors(ctx); err != nil {
			return err
		}

		err = p.patchNode(ctx, view, map[string]string{
			racerctrl.NodeIDAnnotation:     formatUint(uint64(id)),
			racerctrl.NodeZoneAnnotation:   formatUint(uint64(zone)),
			racerctrl.NodeCohortAnnotation: formatUint(uint64(cohort)),
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// leastLoadedCohort picks the cohort a new node in a zone should join.
//
// A trio takes one node from each cohort, so a zone's catalog can only be
// balanced when its three cohorts are the same size. Filling the smallest cohort
// first is what keeps them level as nodes arrive one at a time.
func (p *pass) leastLoadedCohort(zone uint32) uint32 {
	var counts [racerctrl.Cohorts]int

	for _, view := range p.nodes {
		if view.state.ID == 0 || view.state.Zone != zone {
			continue
		}

		if int(view.state.Cohort) < racerctrl.Cohorts {
			counts[view.state.Cohort]++
		}
	}

	best := 0

	for cohort := 1; cohort < racerctrl.Cohorts; cohort++ {
		if counts[cohort] < counts[best] {
			best = cohort
		}
	}

	return uint32(best)
}

// allocateUniverses stamps the fields that make a StorageClass a universe.
//
// A universe id is spent the moment it is written and never comes back: it is
// the top 26 bits of every page address in that universe, so reusing one would
// alias a new universe's addresses onto a retired one's page registers.
func (p *pass) allocateUniverses(ctx context.Context) error {
	for i := range p.universes {
		view := &p.universes[i]
		annotations := map[string]string{}

		if view.state.ID == 0 {
			id, err := p.cursors.AllocateUniverseID()
			if err != nil {
				return fmt.Errorf("allocate universe id for %s: %w", view.class.Name, err)
			}

			if err := p.commitCursors(ctx); err != nil {
				return err
			}

			annotations[racerctrl.UniverseIDAnnotation] = formatUint(uint64(id))
		}

		if view.state.CatalogSize == 0 {
			annotations[racerctrl.CatalogSizeAnnotation] = formatUint(uint64(racerctrl.DefaultCatalogSize))
		}

		if view.state.Epoch == 0 {
			annotations[racerctrl.EpochAnnotation] = "1"
		}

		if _, ok := view.class.Annotations[racerctrl.NextLBAAnnotation]; !ok {
			// Zero is a legal base_lba, so the cursor starts there; it is the
			// extent id that may never be zero, not the address.
			annotations[racerctrl.NextLBAAnnotation] = "0"
		}

		if len(annotations) == 0 {
			continue
		}

		if err := p.patchClass(ctx, view, annotations); err != nil {
			return err
		}
	}

	return nil
}

// reconcileMembership advances each universe's catalog membership one zone at a
// time.
//
// The catalog is the normative placement: position in it decides which trio owns
// a slot, so a change reshuffles where pages live. R6 allows exactly one id to
// change between consecutive generations and forbids starting the next change
// while the dataplane is still healing, which is why the step and the gate come
// from racerctrl.PlanMembership rather than from a diff computed here.
func (p *pass) reconcileMembership(ctx context.Context) error {
	states := p.nodeStates()

	for i := range p.universes {
		view := &p.universes[i]
		if view.state.ID == 0 || view.state.CatalogSize == 0 {
			continue
		}

		for _, zone := range p.zones() {
			if err := p.reconcileZoneMembership(ctx, view, zone, states); err != nil {
				return err
			}
		}
	}

	return nil
}

func (p *pass) reconcileZoneMembership(ctx context.Context, view *universeView, zone uint32, states []racerctrl.NodeState) error {
	candidates := p.candidates(zone)
	current := view.state.Members[zone]

	if len(candidates) == 0 && len(current) == 0 {
		return nil
	}

	step, gate, err := racerctrl.PlanMembership(current, candidates, view.state.CatalogSize, states)
	if err != nil {
		return fmt.Errorf("plan membership for universe %s zone %d: %w", view.class.Name, zone, err)
	}

	if !gate.OK {
		p.wait("universe %s zone %d membership: %s", view.class.Name, zone, gate)

		return nil
	}

	if step.Done {
		return p.reconcileGateways(ctx, view, zone, current)
	}

	annotations := map[string]string{
		racerctrl.MembersAnnotation(zone): racerctrl.FormatMembership(step.Next),
		// Every membership change is a new configuration of the universe, and
		// the epoch is what orders those configurations. It moves with the
		// membership rather than on its own schedule so a node can tell a stale
		// catalog from a current one without comparing the catalogs themselves.
		racerctrl.EpochAnnotation: formatUint(uint64(view.state.Epoch) + 1),
	}

	if err := p.patchClass(ctx, view, annotations); err != nil {
		return err
	}

	p.wait("universe %s zone %d membership stepped to %d nodes", view.class.Name, zone, len(step.Next))

	return p.reconcileGateways(ctx, view, zone, step.Next)
}

// reconcileGateways publishes the nodes other zones may route through.
//
// A zone needs at least one gateway that is also a peer or its neighbours cannot
// reach it at all, and the schema caps the list at 64. Publishing the whole
// membership up to that cap spreads cross-zone traffic instead of funnelling it
// through whichever node happened to be listed first.
func (p *pass) reconcileGateways(ctx context.Context, view *universeView, zone uint32, members racerctrl.Membership) error {
	ids := members.NodeIDs()
	if len(ids) > racerctrl.MaxGateways {
		ids = ids[:racerctrl.MaxGateways]
	}

	desired := racerctrl.FormatUint32List(ids)
	if view.class.Annotations[racerctrl.GatewaysAnnotation(zone)] == desired {
		return nil
	}

	return p.patchClass(ctx, view, map[string]string{racerctrl.GatewaysAnnotation(zone): desired})
}

// zones returns every zone that has at least one node with an identity, lowest
// id first.
func (p *pass) zones() []uint32 {
	seen := map[uint32]struct{}{}

	var zones []uint32

	for _, view := range p.nodes {
		if view.state.ID == 0 || view.state.Zone == 0 {
			continue
		}

		if _, ok := seen[view.state.Zone]; ok {
			continue
		}

		seen[view.state.Zone] = struct{}{}
		zones = append(zones, view.state.Zone)
	}

	sort.Slice(zones, func(i, j int) bool { return zones[i] < zones[j] })

	return zones
}

// candidates lists the nodes eligible for a zone's catalog: enrolled, given an
// identity, and reporting Ready.
//
// A NotReady node is excluded so that a machine that has gone away stops being
// counted, but excluding it only proposes the removal. The removal itself still
// goes through the one-at-a-time step and the healing gate, so a brief blip
// cannot evict a node faster than the dataplane can hand its data over.
func (p *pass) candidates(zone uint32) racerctrl.Membership {
	var members racerctrl.Membership

	for _, view := range p.nodes {
		if !view.enrolled || view.state.ID == 0 || view.state.Zone != zone {
			continue
		}

		if !nodeReady(view.node) {
			continue
		}

		members = append(members, racerctrl.Member{NodeID: view.state.ID, Cohort: view.state.Cohort})
	}

	return members.Normalized()
}

func nodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}

// allocateVolumes places each volume's extents exactly once.
//
// The composition it writes is the whole of the placement: extent ids, base
// addresses, page counts and kinds. Once written it is frozen, because base_lba,
// pages and kind are frozen for an extent's life in the schema, and a rewrite
// would point the same device at a different piece of the address space while
// pages already live at the old one.
func (p *pass) allocateVolumes(ctx context.Context) error {
	for i := range p.universes {
		view := &p.universes[i]
		if view.state.ID == 0 {
			continue
		}

		for j := range view.volumes {
			volume := &view.volumes[j]

			if volume.pv.DeletionTimestamp != nil {
				continue
			}

			if len(volume.state.Composition) > 0 {
				continue
			}

			if err := p.placeVolume(ctx, view, volume); err != nil {
				return err
			}
		}
	}

	return nil
}

func (p *pass) placeVolume(ctx context.Context, view *universeView, volume *volumeView) error {
	zone := p.homeZone(view)
	if zone == 0 {
		p.wait("volume %s has no zone with a catalog in universe %s", volume.pv.Name, view.class.Name)

		return nil
	}

	capacity, ok := volume.pv.Spec.Capacity[corev1.ResourceStorage]
	if !ok || capacity.Value() <= 0 {
		return fmt.Errorf("persistent volume %s has no storage capacity", volume.pv.Name)
	}

	specs, err := racerctrl.ParseGeometry(uint64(capacity.Value()), volume.pv.Spec.CSI.VolumeAttributes)
	if err != nil {
		return fmt.Errorf("persistent volume %s geometry: %w", volume.pv.Name, err)
	}

	nextLBA, err := racerctrl.NextLBA(view.class.Annotations)
	if err != nil {
		return fmt.Errorf("storage class %s: %w", view.class.Name, err)
	}

	composition, advanced, err := racerctrl.Allocate(specs, &p.cursors, nextLBA)
	if err != nil {
		return fmt.Errorf("place volume %s in universe %s: %w", volume.pv.Name, view.class.Name, err)
	}

	// Both cursors move before the composition is written, for the same reason
	// in both cases: a crash here has to burn address space and extent ids, not
	// hand them to a second volume.
	if err := p.commitCursors(ctx); err != nil {
		return err
	}

	if err := p.patchClass(ctx, view, map[string]string{racerctrl.NextLBAAnnotation: formatUint(advanced)}); err != nil {
		return err
	}

	finalizers := volume.pv.Finalizers
	if !hasFinalizer(finalizers, racerctrl.VolumeFinalizer) {
		finalizers = append(append([]string{}, finalizers...), racerctrl.VolumeFinalizer)
	}

	annotations := map[string]string{
		racerctrl.CompositionAnnotation: racerctrl.FormatComposition(composition),
		racerctrl.VolumeZoneAnnotation:  formatUint(uint64(zone)),
		racerctrl.PhaseAnnotation:       racerctrl.PhaseActive,
	}

	return p.patchVolume(ctx, volume, annotations, finalizers)
}

// homeZone picks the zone a new volume's extents live in: the lowest-numbered
// zone that actually has a catalog in this universe.
//
// Lowest wins because it is stable. Anything cleverer - least loaded, most
// recently used - would move new volumes around as the cluster changes, and a
// volume's home zone is the thing R5 will only ever let us change through a
// declared migration.
func (p *pass) homeZone(view *universeView) uint32 {
	best := uint32(0)

	for zone, members := range view.state.Members {
		if len(members) == 0 {
			continue
		}

		if best == 0 || zone < best {
			best = zone
		}
	}

	return best
}

func hasFinalizer(finalizers []string, name string) bool {
	for _, finalizer := range finalizers {
		if finalizer == name {
			return true
		}
	}

	return false
}

func withoutFinalizer(finalizers []string, name string) []string {
	kept := make([]string, 0, len(finalizers))

	for _, finalizer := range finalizers {
		if finalizer != name {
			kept = append(kept, finalizer)
		}
	}

	return kept
}
