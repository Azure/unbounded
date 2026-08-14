// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package racer

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/racerctrl"
)

// allocationsName is the ConfigMap holding the cluster-wide id cursors.
const allocationsName = racerctrl.AllocationsConfigMapName

// defaultClassName is the StorageClass created on first reconcile so a cluster
// that just enabled racer has one universe to put volumes in.
const defaultClassName = "racer"

// nodeView pairs a Node object with the racer state parsed out of it.
type nodeView struct {
	node *corev1.Node

	// eligible is whether racer belongs on this node: it sits in a site that
	// enables racer and has not been opted out by hand. A node that has an
	// identity but is no longer eligible is being decommissioned.
	eligible bool

	// announced is whether the node's agent has said racer is running there.
	// Eligibility decides where the pod goes; this decides when the node is
	// given an identity, so a node whose store never came up is never placed
	// into a catalog.
	announced bool

	// active is whether the node carries the workload label, which is what the
	// DaemonSet selects on and what this component writes.
	active bool

	state racerctrl.NodeState
}

// placeable reports whether a node is waiting to be given an identity: racer
// belongs on it, its agent has said racer is running there, and nothing has been
// allocated to it yet.
func (v *nodeView) placeable() bool {
	return v.eligible && v.announced && v.state.ID == 0
}

// enrollment decides which nodes racer belongs on.
//
// Membership is not something a user asks for per node. A node in a site that
// enables racer runs racer, and running racer is what makes it part of the
// cluster; the only per-node input is the opt-out label, for the node an
// administrator wants kept out of it.
type enrollment struct {
	// sites are the names of the Sites that enable racer, matched against the
	// node's site-membership label.
	sites map[string]bool

	// frozen says no site enables racer any more but an installation is being
	// retained. Every node stays eligible in that state. Draining the whole
	// cluster because the last Site stopped asking would be a decommission with
	// nowhere to hand the data to, which is a gate that can never clear, so
	// turning racer off everywhere freezes the cluster where it stands and
	// leaves the teardown to whoever meant it.
	frozen bool
}

// admits reports whether racer belongs on a node.
func (e enrollment) admits(node *corev1.Node) bool {
	if node.Labels[EnrollmentLabel] == "false" {
		return false
	}

	if e.frozen {
		return true
	}

	return e.sites[nodeSite(node)]
}

// volumeView pairs a PersistentVolume with the racer state parsed out of it.
type volumeView struct {
	pv    *corev1.PersistentVolume
	state racerctrl.VolumeState
}

// universeView pairs a StorageClass with its universe state and its volumes.
type universeView struct {
	class   *storagev1.StorageClass
	state   racerctrl.UniverseState
	volumes []volumeView
}

// pass is one reconcile's view of the world plus the writes it has made. It is
// rebuilt from the API server every reconcile: there is no cached progress,
// because every decision here is a function of published state and re-deriving
// it is what makes a restart indistinguishable from a requeue.
type pass struct {
	env *component.Env

	// enroll is the rule that decides which nodes racer belongs on, computed
	// once from the Sites this reconcile was handed.
	enroll enrollment

	allocations *corev1.ConfigMap
	cursors     racerctrl.Cursors

	nodes     []nodeView
	universes []universeView
	orphans   []volumeView

	// memberships is every per-zone membership ConfigMap in the namespace,
	// keyed by the universe and zone it belongs to.
	memberships map[membershipKey]*corev1.ConfigMap

	// waiting collects the reasons a sequence has not finished, which become
	// the not-ready condition's message.
	waiting []string
}

// wait records a blocked gate.
func (p *pass) wait(format string, args ...any) {
	p.waiting = append(p.waiting, fmt.Sprintf(format, args...))
}

// nodeStates returns just the parsed node states, which is what the pure gates
// in internal/racerctrl take.
func (p *pass) nodeStates() []racerctrl.NodeState {
	states := make([]racerctrl.NodeState, 0, len(p.nodes))
	for _, node := range p.nodes {
		states = append(states, node.state)
	}

	return states
}

// loadState reads everything this component reasons about.
//
// Objects that cannot be parsed are a hard error rather than a skip. On the node
// side a malformed annotation is skipped, because a node must keep serving what
// it already has; here it means the single writer would be about to overwrite
// something it does not understand, and stopping is the safer answer.
func loadState(ctx context.Context, env *component.Env, enroll enrollment) (*pass, error) {
	p := &pass{env: env, enroll: enroll}

	allocations, err := ensureAllocations(ctx, env)
	if err != nil {
		return nil, err
	}

	p.allocations = allocations

	cursors, err := racerctrl.ParseCursors(allocations.Data)
	if err != nil {
		return nil, fmt.Errorf("parse %s/%s: %w", allocations.Namespace, allocations.Name, err)
	}

	p.cursors = cursors

	if err := p.loadNodes(ctx); err != nil {
		return nil, err
	}

	if err := p.loadMemberships(ctx); err != nil {
		return nil, err
	}

	if err := p.loadUniverses(ctx); err != nil {
		return nil, err
	}

	return p, nil
}

// membershipKey identifies one zone's membership within one universe.
type membershipKey struct {
	universe uint32
	zone     uint32
}

// loadMemberships reads every per-zone membership ConfigMap in the operator
// namespace.
//
// They are listed by label rather than fetched by name because the operator has
// no other record of which zones a universe has published: the zone cursor says
// which ids exist, not which of them ever got a catalog.
func (p *pass) loadMemberships(ctx context.Context) error {
	maps := &corev1.ConfigMapList{}
	if err := p.env.Client.List(ctx, maps, client.InNamespace(p.env.Namespace),
		client.HasLabels{racerctrl.MembershipUniverseLabel}); err != nil {
		return fmt.Errorf("list membership config maps: %w", err)
	}

	p.memberships = make(map[membershipKey]*corev1.ConfigMap, len(maps.Items))

	for i := range maps.Items {
		item := &maps.Items[i]

		universe, zone, ok := racerctrl.ParseMembershipLabels(item.Labels)
		if !ok {
			continue
		}

		p.memberships[membershipKey{universe: universe, zone: zone}] = item
	}

	return nil
}

// writeMembership publishes a zone's membership together with the topology
// epoch that names it, creating the ConfigMap the first time the zone gets a
// catalog.
//
// The three go in one object because they have to change together and
// Kubernetes has no transaction spanning two. A catalog published by itself,
// with the epoch left to a second write that may never happen, is a
// configuration the cluster cannot tell apart from the one before it; and a
// catalog that drops an id without naming it as draining is one the dropped
// node never learns about.
func (p *pass) writeMembership(
	ctx context.Context,
	universe, zone uint32,
	members, draining racerctrl.Membership,
	catalog racerctrl.Catalog,
	epoch uint32,
) error {
	key := membershipKey{universe: universe, zone: zone}

	desired := map[string]string{
		racerctrl.MembershipDataKey:     racerctrl.FormatMembership(members),
		racerctrl.MembershipDrainingKey: racerctrl.FormatMembership(draining),
		racerctrl.MembershipCatalogKey:  racerctrl.FormatCatalog(catalog),
		racerctrl.MembershipEpochKey:    formatUint(uint64(epoch)),
	}

	if item, ok := p.memberships[key]; ok {
		if item.Data[racerctrl.MembershipDataKey] == desired[racerctrl.MembershipDataKey] &&
			item.Data[racerctrl.MembershipDrainingKey] == desired[racerctrl.MembershipDrainingKey] &&
			item.Data[racerctrl.MembershipCatalogKey] == desired[racerctrl.MembershipCatalogKey] &&
			item.Data[racerctrl.MembershipEpochKey] == desired[racerctrl.MembershipEpochKey] {
			return nil
		}

		updated := item.DeepCopy()
		if updated.Data == nil {
			updated.Data = map[string]string{}
		}

		for name, value := range desired {
			updated.Data[name] = value
		}

		if err := p.env.Client.Update(ctx, updated); err != nil {
			return fmt.Errorf("update %s/%s: %w", updated.Namespace, updated.Name, err)
		}

		p.memberships[key] = updated

		return nil
	}

	item := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: p.env.Namespace,
			Name:      racerctrl.MembershipConfigMapName(universe, zone),
			Labels:    racerctrl.MembershipLabels(universe, zone),
		},
		Data: desired,
	}

	if err := p.env.Client.Create(ctx, item); err != nil {
		return fmt.Errorf("create %s/%s: %w", item.Namespace, item.Name, err)
	}

	p.memberships[key] = item

	return nil
}

func (p *pass) loadNodes(ctx context.Context) error {
	nodes := &corev1.NodeList{}
	if err := p.env.Client.List(ctx, nodes); err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}

	for i := range nodes.Items {
		node := &nodes.Items[i]

		state, err := racerctrl.ParseNodeState(node.Name, node.Annotations)
		if err != nil {
			return fmt.Errorf("parse racer annotations on node %s: %w", node.Name, err)
		}

		eligible := p.enroll.admits(node)
		active := node.Labels[WorkloadLabel] == "true"

		if !eligible && !active && state.ID == 0 {
			// Racer does not belong here, has never run here, and never
			// allocated anything here: not our business at all.
			continue
		}

		p.nodes = append(p.nodes, nodeView{
			node:      node,
			eligible:  eligible,
			announced: state.Agent != "",
			active:    active,
			state:     state,
		})
	}

	sort.Slice(p.nodes, func(i, j int) bool { return p.nodes[i].node.Name < p.nodes[j].node.Name })

	return nil
}

func (p *pass) loadUniverses(ctx context.Context) error {
	classes := &storagev1.StorageClassList{}
	if err := p.env.Client.List(ctx, classes); err != nil {
		return fmt.Errorf("list storage classes: %w", err)
	}

	byName := map[string]*universeView{}

	for i := range classes.Items {
		class := &classes.Items[i]
		if class.Provisioner != racerctrl.DriverName {
			continue
		}

		state, err := racerctrl.ParseUniverseState(class.Name, class.Annotations)
		if err != nil {
			return fmt.Errorf("parse racer annotations on storage class %s: %w", class.Name, err)
		}

		state.Deleting = class.DeletionTimestamp != nil
		p.universes = append(p.universes, universeView{class: class, state: state})
	}

	// Indexed after every class is appended, not during: taking a pointer into
	// a slice that is still growing hands out addresses into a backing array
	// the next append may replace.
	for i := range p.universes {
		view := &p.universes[i]
		byName[view.class.Name] = view

		if view.state.ID == 0 {
			continue
		}

		// Membership is not in the class's annotations. It lives in one
		// ConfigMap per zone, so it is joined in here.
		for key, item := range p.memberships {
			if key.universe != view.state.ID {
				continue
			}

			members, err := racerctrl.ParseMembership(item.Data[racerctrl.MembershipDataKey])
			if err != nil {
				return fmt.Errorf("parse %s/%s: %w", item.Namespace, item.Name, err)
			}

			draining, err := racerctrl.ParseMembership(item.Data[racerctrl.MembershipDrainingKey])
			if err != nil {
				return fmt.Errorf("parse %s/%s: %w", item.Namespace, item.Name, err)
			}

			catalog, err := racerctrl.ParseCatalog(item.Data[racerctrl.MembershipCatalogKey])
			if err != nil {
				return fmt.Errorf("parse %s/%s: %w", item.Namespace, item.Name, err)
			}

			epoch, err := racerctrl.ParseMembershipEpoch(item.Data)
			if err != nil {
				return fmt.Errorf("parse %s/%s: %w", item.Namespace, item.Name, err)
			}

			view.state.Members[key.zone] = members
			view.state.Draining[key.zone] = draining
			view.state.Catalogs[key.zone] = catalog
			view.state.MemberEpochs[key.zone] = epoch
		}
	}

	volumes := &corev1.PersistentVolumeList{}
	if err := p.env.Client.List(ctx, volumes); err != nil {
		return fmt.Errorf("list persistent volumes: %w", err)
	}

	for i := range volumes.Items {
		pv := &volumes.Items[i]
		if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != racerctrl.DriverName {
			continue
		}

		state, err := racerctrl.ParseVolumeState(pv.Name, pv.Annotations)
		if err != nil {
			return fmt.Errorf("parse racer annotations on persistent volume %s: %w", pv.Name, err)
		}

		universe, ok := byName[pv.Spec.StorageClassName]
		if !ok {
			// Without the class there is no universe state from which the
			// composition can be sequenced. Keep the volume visible so sequence
			// can report it and release a finalizer that can no longer protect
			// anything.
			p.orphans = append(p.orphans, volumeView{pv: pv, state: state})

			continue
		}

		universe.volumes = append(universe.volumes, volumeView{pv: pv, state: state})
	}

	sort.Slice(p.universes, func(i, j int) bool { return p.universes[i].class.Name < p.universes[j].class.Name })

	for i := range p.universes {
		volumes := p.universes[i].volumes
		sort.Slice(volumes, func(a, b int) bool { return volumes[a].pv.Name < volumes[b].pv.Name })
	}

	sort.Slice(p.orphans, func(i, j int) bool { return p.orphans[i].pv.Name < p.orphans[j].pv.Name })

	return nil
}

// ensureAllocations gets the cursor ConfigMap, creating an empty one the first
// time. It is created rather than applied from a manifest because every write
// after the first is an increment of what is already there, and a manifest apply
// would reset it.
func ensureAllocations(ctx context.Context, env *component.Env) (*corev1.ConfigMap, error) {
	key := client.ObjectKey{Namespace: env.Namespace, Name: allocationsName}
	existing := &corev1.ConfigMap{}

	err := env.Client.Get(ctx, key, existing)
	if err == nil {
		return existing, nil
	}

	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get %s/%s: %w", key.Namespace, key.Name, err)
	}

	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: key.Namespace,
			Name:      key.Name,
			Labels:    map[string]string{"app.kubernetes.io/part-of": "racer"},
		},
		Data: racerctrl.Cursors{}.Data(),
	}

	if err := env.Client.Create(ctx, desired); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("create %s/%s: %w", key.Namespace, key.Name, err)
		}

		if err := env.Client.Get(ctx, key, existing); err != nil {
			return nil, fmt.Errorf("get raced %s/%s: %w", key.Namespace, key.Name, err)
		}

		return existing, nil
	}

	return desired, nil
}

// commitCursors writes the cursors back before any id they produced is stamped
// on an object.
//
// The order matters and only one way round is safe. Persist first and crash, and
// the ids just handed out are burned - nothing refers to them and nothing ever
// will. Stamp first and crash, and the next pass hands the same id to a
// different object, which is the one failure R2 rules out outright.
func (p *pass) commitCursors(ctx context.Context) error {
	data := p.cursors.Data()

	same := len(data) == len(p.allocations.Data)
	if same {
		for key, value := range data {
			if p.allocations.Data[key] != value {
				same = false
				break
			}
		}
	}

	if same {
		return nil
	}

	updated := p.allocations.DeepCopy()
	updated.Data = data

	if err := p.env.Client.Update(ctx, updated); err != nil {
		return fmt.Errorf("update %s/%s: %w", updated.Namespace, updated.Name, err)
	}

	p.allocations = updated

	return nil
}

// patchNode merge-patches annotations onto a Node and keeps the in-memory view
// in step, so later passes in the same reconcile see what earlier ones wrote.
func (p *pass) patchNode(ctx context.Context, view *nodeView, annotations map[string]string) error {
	return p.patchNodeMeta(ctx, view, annotations, nil)
}

// patchNodeMeta merge-patches annotations and labels onto a Node in one write.
//
// Retirement needs both in the same patch: the identity annotations say the node
// is nobody, and the workload label says the pod should stop. Split across two
// writes, a crash between them leaves either a pod with no identity or an
// identity with no pod, and one of those two is a node that can never finish
// leaving.
func (p *pass) patchNodeMeta(ctx context.Context, view *nodeView, annotations, labels map[string]string) error {
	patch := client.MergeFrom(view.node.DeepCopy())
	updated := view.node.DeepCopy()

	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}

	for key, value := range annotations {
		if value == "" {
			delete(updated.Annotations, key)
		} else {
			updated.Annotations[key] = value
		}
	}

	if len(labels) > 0 && updated.Labels == nil {
		updated.Labels = map[string]string{}
	}

	for key, value := range labels {
		if value == "" {
			delete(updated.Labels, key)
		} else {
			updated.Labels[key] = value
		}
	}

	if err := p.env.Client.Patch(ctx, updated, patch); err != nil {
		return fmt.Errorf("patch node %s: %w", updated.Name, err)
	}

	state, err := racerctrl.ParseNodeState(updated.Name, updated.Annotations)
	if err != nil {
		return fmt.Errorf("reparse node %s after patch: %w", updated.Name, err)
	}

	view.node = updated
	view.state = state
	view.eligible = p.enroll.admits(updated)
	view.announced = state.Agent != ""
	view.active = updated.Labels[WorkloadLabel] == "true"

	return nil
}

// reconcileWorkloadLabels keeps the label the DaemonSet selects on in step with
// racer's lifecycle rather than with the operator's opinion of it.
//
// A node runs racer while racer belongs on it and, after it stops belonging, for
// as long as it still holds an identity. That trailing window is the whole
// decommission: the node is out of the candidate set, the membership sequencer
// is stepping it out of each catalog one universe at a time, and it has to be
// running to accept those configs, shed what it holds and say so. Retirement is
// what takes the label away again, in the same write that takes the identity.
//
// This is also why the DaemonSet selects on this label rather than on the site
// label directly. Taking racer off a site is a request to leave, and a node that
// leaves has to keep running until it has handed its data over.
func (p *pass) reconcileWorkloadLabels(ctx context.Context) error {
	for i := range p.nodes {
		view := &p.nodes[i]

		wanted := view.eligible || view.state.ID != 0
		if wanted == view.active {
			continue
		}

		value := "true"
		if !wanted {
			// A stray label: the node was retired and re-labelled by hand, or a
			// patch landed and the one that was meant to follow it did not.
			// Either way nothing here should be running.
			value = ""
		}

		if err := p.patchNodeMeta(ctx, view, nil, map[string]string{WorkloadLabel: value}); err != nil {
			return err
		}
	}

	return nil
}

// patchClass merge-patches annotations onto a StorageClass.
func (p *pass) patchClass(ctx context.Context, view *universeView, annotations map[string]string) error {
	return p.patchClassMeta(ctx, view, annotations, nil)
}

// patchClassMeta merge-patches annotations and finalizers onto a StorageClass.
func (p *pass) patchClassMeta(
	ctx context.Context,
	view *universeView,
	annotations map[string]string,
	finalizers []string,
) error {
	patch := client.MergeFrom(view.class.DeepCopy())
	updated := view.class.DeepCopy()

	if annotations != nil && updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}

	for key, value := range annotations {
		if value == "" {
			delete(updated.Annotations, key)
		} else {
			updated.Annotations[key] = value
		}
	}

	if finalizers != nil {
		updated.Finalizers = finalizers
	}

	if err := p.env.Client.Patch(ctx, updated, patch); err != nil {
		return fmt.Errorf("patch storage class %s: %w", updated.Name, err)
	}

	state, err := racerctrl.ParseUniverseState(updated.Name, updated.Annotations)
	if err != nil {
		return fmt.Errorf("reparse storage class %s after patch: %w", updated.Name, err)
	}

	view.class = updated
	view.state.ID = state.ID
	view.state.Epoch = state.Epoch
	view.state.CatalogSize = state.CatalogSize
	view.state.GatewayCount = state.GatewayCount
	view.state.Gateways = state.Gateways

	// Members is deliberately not copied: it was joined in from the per-zone
	// ConfigMaps and the class's annotations no longer carry it. Neither are
	// MemberEpochs, which come from the same maps.

	return nil
}

// patchVolume merge-patches annotations and finalizers onto a PersistentVolume.
func (p *pass) patchVolume(ctx context.Context, view *volumeView, annotations map[string]string, finalizers []string) error {
	patch := client.MergeFrom(view.pv.DeepCopy())
	updated := view.pv.DeepCopy()

	if annotations != nil && updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}

	for key, value := range annotations {
		if value == "" {
			delete(updated.Annotations, key)
		} else {
			updated.Annotations[key] = value
		}
	}

	if finalizers != nil {
		updated.Finalizers = finalizers
	}

	if err := p.env.Client.Patch(ctx, updated, patch); err != nil {
		return fmt.Errorf("patch persistent volume %s: %w", updated.Name, err)
	}

	state, err := racerctrl.ParseVolumeState(updated.Name, updated.Annotations)
	if err != nil {
		return fmt.Errorf("reparse persistent volume %s after patch: %w", updated.Name, err)
	}

	view.pv = updated
	view.state = state

	return nil
}

// formatUint renders an id for an annotation value.
func formatUint(v uint64) string { return strconv.FormatUint(v, 10) }
