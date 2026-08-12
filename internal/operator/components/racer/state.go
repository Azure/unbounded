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

	// enrolled is whether the node still carries the enrollment label. A node
	// that has identity but is no longer enrolled is being decommissioned.
	enrolled bool

	state racerctrl.NodeState
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

	allocations *corev1.ConfigMap
	cursors     racerctrl.Cursors

	nodes     []nodeView
	universes []universeView

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
func loadState(ctx context.Context, env *component.Env) (*pass, error) {
	p := &pass{env: env}

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

	if err := p.loadUniverses(ctx); err != nil {
		return nil, err
	}

	return p, nil
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

		enrolled := node.Labels[EnrollmentLabel] == "true"
		if !enrolled && state.ID == 0 {
			// Never enrolled and never allocated: not our business at all.
			continue
		}

		p.nodes = append(p.nodes, nodeView{node: node, enrolled: enrolled, state: state})
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

		p.universes = append(p.universes, universeView{class: class, state: state})
		byName[class.Name] = &p.universes[len(p.universes)-1]
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

		universe, ok := byName[pv.Spec.StorageClassName]
		if !ok {
			// A racer volume whose class has been deleted or never carried our
			// provisioner. There is no universe to place it in, so leave it
			// alone rather than guess.
			continue
		}

		state, err := racerctrl.ParseVolumeState(pv.Name, pv.Annotations)
		if err != nil {
			return fmt.Errorf("parse racer annotations on persistent volume %s: %w", pv.Name, err)
		}

		universe.volumes = append(universe.volumes, volumeView{pv: pv, state: state})
	}

	sort.Slice(p.universes, func(i, j int) bool { return p.universes[i].class.Name < p.universes[j].class.Name })

	for i := range p.universes {
		volumes := p.universes[i].volumes
		sort.Slice(volumes, func(a, b int) bool { return volumes[a].pv.Name < volumes[b].pv.Name })
	}

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

	if err := p.env.Client.Patch(ctx, updated, patch); err != nil {
		return fmt.Errorf("patch node %s: %w", updated.Name, err)
	}

	state, err := racerctrl.ParseNodeState(updated.Name, updated.Annotations)
	if err != nil {
		return fmt.Errorf("reparse node %s after patch: %w", updated.Name, err)
	}

	view.node = updated
	view.state = state

	return nil
}

// patchClass merge-patches annotations onto a StorageClass.
func (p *pass) patchClass(ctx context.Context, view *universeView, annotations map[string]string) error {
	patch := client.MergeFrom(view.class.DeepCopy())
	updated := view.class.DeepCopy()

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
	view.state.Members = state.Members
	view.state.Gateways = state.Gateways

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
