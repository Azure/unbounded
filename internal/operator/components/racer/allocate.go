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

	"github.com/Azure/unbounded/internal/operator/component"
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
//
// The zone and the cohort are worked out by racerctrl.NewPlacer unless the node
// named a zone itself, in which case that wins. Placement is a function of the
// cursors and of what is already placed, and the cursors are committed before
// any node is stamped, so a crash in the middle leaks ids rather than placing a
// node twice.
func (p *pass) allocateNodeIdentities(ctx context.Context) error {
	placer := racerctrl.NewPlacer(&p.cursors, p.placedNodes(), p.unplacedNodes())

	for i := range p.nodes {
		view := &p.nodes[i]

		if !view.enrolled || view.state.ID != 0 {
			continue
		}

		placement, err := placer.Place(p.placementNode(view))
		if err != nil {
			return err
		}

		id, err := p.cursors.AllocateNodeID()
		if err != nil {
			return fmt.Errorf("allocate node id for %s: %w", view.node.Name, err)
		}

		// Committed before the node is stamped, always. A crash in between
		// leaks a zone id and a node id, both of which were never reusable
		// anyway; the other order would hand the same id to two machines.
		if err := p.commitCursors(ctx); err != nil {
			return err
		}

		err = p.patchNode(ctx, view, map[string]string{
			racerctrl.NodeIDAnnotation:     formatUint(uint64(id)),
			racerctrl.NodeZoneAnnotation:   formatUint(uint64(placement.Zone)),
			racerctrl.NodeCohortAnnotation: formatUint(uint64(placement.Cohort)),
		})
		if err != nil {
			return err
		}
	}

	p.reportPlacementDrift()

	return nil
}

// placementNode reads the inputs placement takes off a Node object.
func (p *pass) placementNode(view *nodeView) racerctrl.PlacementNode {
	return racerctrl.PlacementNode{
		Name:     view.node.Name,
		Site:     nodeSite(view.node),
		AZ:       view.node.Labels[ZoneLabel],
		Fabric:   view.node.Annotations[racerctrl.NodeFabricIDAnnotation],
		ZoneName: view.node.Annotations[racerctrl.NodeZoneNameAnnotation],
	}
}

// placedNodes is the census placement balances against: every node that already
// holds an identity, whether or not it is still enrolled. A node on its way out
// still occupies its cohort until the catalog has let go of it.
func (p *pass) placedNodes() []racerctrl.PlacedNode {
	placed := make([]racerctrl.PlacedNode, 0, len(p.nodes))

	for _, view := range p.nodes {
		if view.state.ID == 0 {
			continue
		}

		placed = append(placed, racerctrl.PlacedNode{
			Zone:   view.state.Zone,
			Cohort: view.state.Cohort,
			AZ:     view.node.Labels[ZoneLabel],
			Fabric: view.state.FabricID,
		})
	}

	return placed
}

// unplacedNodes is every enrolled node still waiting for an identity. It is
// what decides whether a fabric has enough nodes waiting to be worth a zone of
// its own, so it has to be the whole batch rather than the one node in hand.
func (p *pass) unplacedNodes() []racerctrl.PlacementNode {
	unplaced := make([]racerctrl.PlacementNode, 0, len(p.nodes))

	for i := range p.nodes {
		view := &p.nodes[i]
		if !view.enrolled || view.state.ID != 0 {
			continue
		}

		unplaced = append(unplaced, p.placementNode(view))
	}

	return unplaced
}

// reportPlacementDrift notes nodes whose site or availability zone no longer
// matches where they were placed.
//
// Nothing is done about it. A zone and a cohort are frozen once stamped: a
// cohort is the node's column in every trio it holds, and a zone is the catalog
// it is a member of, which moves one node at a time. Moving a live node would
// be a decommission and a re-enrolment, which is an operator's decision.
func (p *pass) reportPlacementDrift() {
	for i := range p.nodes {
		view := &p.nodes[i]
		if view.state.ID == 0 || view.state.Zone == 0 {
			continue
		}

		def, ok := p.cursors.ZoneDefs[view.state.Zone]
		if !ok {
			continue
		}

		placement := racerctrl.Placement{Zone: view.state.Zone, Cohort: view.state.Cohort}

		drift := racerctrl.PlacementDrift(p.placementNode(view), placement, def,
			p.cursors.ZoneBuckets[view.state.Zone])
		if drift != "" {
			p.wait("%s", drift)
		}
	}
}

// nodeSite is the unbounded site a node belongs to. The deprecated label is
// still read because per-site components are mid-migration onto the canonical
// one, and placement must not split a site across two zones because half its
// nodes carry the old key.
func nodeSite(node *corev1.Node) string {
	if site := node.Labels[component.SiteLabelKey]; site != "" {
		return site
	}

	return node.Labels[component.DeprecatedSiteLabelKey]
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

		if err := p.reconcileMembershipEpochs(ctx, view); err != nil {
			return err
		}

		for _, zone := range p.zones() {
			if err := p.reconcileZoneMembership(ctx, view, zone, states); err != nil {
				return err
			}
		}
	}

	return nil
}

// reconcileMembershipEpochs repairs the relationship between a universe's epoch
// cursor and the epochs its published memberships carry.
//
// A step publishes the membership first and moves the cursor second, so a crash
// between the two leaves a catalog dated ahead of the class. The catalog is the
// published one and nodes are already running it, so the repair is to move the
// cursor up to it rather than to rewrite the catalog: the cursor's only job is
// to say which epochs have been handed out, and one that lags would hand the
// same epoch to a second zone.
//
// A membership with no epoch at all predates the epoch travelling with it. It is
// stamped with the cursor, which is the epoch the nodes reading it are already
// running, so the stamp changes nothing about the configuration.
func (p *pass) reconcileMembershipEpochs(ctx context.Context, view *universeView) error {
	highest := view.state.Epoch

	for _, epoch := range view.state.MemberEpochs {
		if epoch > highest {
			highest = epoch
		}
	}

	if highest > view.state.Epoch {
		err := p.patchClass(ctx, view, map[string]string{
			racerctrl.EpochAnnotation: formatUint(uint64(highest)),
		})
		if err != nil {
			return err
		}
	}

	if view.state.Epoch == 0 {
		// The universe has not been allocated an epoch yet, so there is nothing
		// to stamp a legacy membership with.
		return nil
	}

	for zone, members := range view.state.Members {
		if view.state.MemberEpochs[zone] != 0 {
			continue
		}

		err := p.writeMembership(ctx, view.state.ID, zone, members, view.state.Draining[zone], view.state.Epoch)
		if err != nil {
			return err
		}

		view.state.MemberEpochs[zone] = view.state.Epoch
	}

	return nil
}

func (p *pass) reconcileZoneMembership(ctx context.Context, view *universeView, zone uint32, states []racerctrl.NodeState) error {
	candidates := p.candidates(zone)
	current := view.state.Members[zone]
	draining := view.state.Draining[zone]

	if len(candidates) == 0 && len(current) == 0 && len(draining) == 0 {
		return nil
	}

	step, gate, err := racerctrl.PlanMembership(racerctrl.MembershipPlan{
		Universe:    view.state.ID,
		Epoch:       view.state.EpochFor(zone),
		CatalogSize: view.state.CatalogSize,
		Current:     current,
		Draining:    draining,
		Candidates:  candidates,
		Nodes:       states,
	})
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

	// The membership itself goes in the zone's own ConfigMap; only the epoch
	// cursor is on the class. Every membership change is a new configuration of
	// the universe, and the epoch is what orders those configurations, so the
	// new epoch is written with the new catalog in a single object update. The
	// cursor moves afterwards to record that this epoch has been handed out; if
	// that write is lost, reconcileMembershipEpochs catches the cursor up on the
	// next pass rather than leaving the catalog stranded at an epoch the
	// universe does not know it has spent.
	//
	// A pass that only retires a drained node takes an epoch too. The draining
	// set is part of the universe's shape: it decides who the survivors link to
	// and admit, so dropping an id from it is as much a new configuration as
	// dropping one from the catalog, and the gate that waits on it needs an
	// epoch to wait for.
	epoch := view.state.Epoch + 1

	if err := p.writeMembership(ctx, view.state.ID, zone, step.Next, step.Draining, epoch); err != nil {
		return err
	}

	view.state.Members[zone] = step.Next
	view.state.Draining[zone] = step.Draining
	view.state.MemberEpochs[zone] = epoch

	err = p.patchClass(ctx, view, map[string]string{
		racerctrl.EpochAnnotation: formatUint(uint64(epoch)),
	})
	if err != nil {
		return err
	}

	p.wait("universe %s zone %d membership stepped to %d nodes, %d draining",
		view.class.Name, zone, len(step.Next), len(step.Draining))

	return p.reconcileGateways(ctx, view, zone, step.Next)
}

// reconcileGateways publishes the nodes other zones may route through.
//
// A zone needs at least one gateway that is also a peer or its neighbours cannot
// reach it at all, and the schema caps the list at 64. How many below that cap
// is the knob that decides how much two zones overlap: every node in every
// other zone holds an NVMe-oF controller per gateway, so a wide list costs
// controllers everywhere, and a narrow one funnels cross-zone traffic through
// fewer machines.
//
// Which members are chosen is racerctrl.SelectGateways: bridge nodes first,
// because a member sitting on another zone's fabric reaches that zone without
// leaving RDMA, then round-robin across cohorts so an availability zone going
// down does not take the whole gateway list with it.
func (p *pass) reconcileGateways(ctx context.Context, view *universeView, zone uint32, members racerctrl.Membership) error {
	ids := racerctrl.SelectGateways(members, p.bridgeNodes(zone), int(view.state.GatewayCount))

	desired := racerctrl.FormatUint32List(ids)
	if view.class.Annotations[racerctrl.GatewaysAnnotation(zone)] == desired {
		return nil
	}

	return p.patchClass(ctx, view, map[string]string{racerctrl.GatewaysAnnotation(zone): desired})
}

// bridgeNodes is the set of node ids in a zone whose fabric is not the zone's
// own. Those are the nodes another zone can reach over RDMA, which is what
// makes them the gateways worth having.
func (p *pass) bridgeNodes(zone uint32) map[uint32]bool {
	def, ok := p.cursors.ZoneDefs[zone]
	if !ok {
		return nil
	}

	bridges := map[uint32]bool{}

	for _, view := range p.nodes {
		if view.state.ID == 0 || view.state.Zone != zone {
			continue
		}

		// A node with no fabric at all bridges nothing: there is no other
		// fabric it is closer to than anyone else is.
		if view.state.FabricID != "" && view.state.FabricID != def.Fabric {
			bridges[view.state.ID] = true
		}
	}

	return bridges
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
