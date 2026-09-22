// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	eventhandler "sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/Azure/unbounded/internal/racer"
)

// Kubernetes watches and universe reconciliation.

const (
	universeIndex = "racer.universe"
	originIndex   = "racer.origin"
)

type reconciler struct {
	client client.Client
	store  stateStore
	server *Server
	// Controller serializes reconciles; these are last committed immutable views.
	loaded      map[string]*generation
	pointers    map[string]*corev1.ConfigMap
	minInterval time.Duration
	lastAttempt map[string]time.Time
	reserved    reservedPorts
}

func setupController(ctx context.Context, manager ctrl.Manager, server *Server, namespace string, reserved reservedPorts) error {
	for _, object := range []client.Object{&corev1.Node{}, &corev1.Service{}} {
		if err := manager.GetFieldIndexer().IndexField(ctx, object, universeIndex, objectUniverses); err != nil {
			return err
		}
	}

	if err := manager.GetFieldIndexer().IndexField(ctx, &corev1.Service{}, originIndex, originDependency); err != nil {
		return err
	}

	r := &reconciler{client: manager.GetClient(), store: stateStore{client: manager.GetClient(), namespace: namespace}, server: server, loaded: map[string]*generation{}, pointers: map[string]*corev1.ConfigMap{}}
	r.minInterval = 2 * time.Second
	r.reserved = reserved
	r.lastAttempt = map[string]time.Time{}
	// State persistence must use a non-cached controller-runtime client: the
	// pointer update is a CAS and chunks must be readable immediately after create.
	direct, err := client.New(manager.GetConfig(), client.Options{Scheme: manager.GetScheme()})
	if err != nil {
		return err
	}

	r.store.client = direct
	server.controlStore = r.store
	mapUniverse := eventhandler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
		return universeRequests(objectUniverses(o)...)
	})
	mapState := eventhandler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
		cm, ok := o.(*corev1.ConfigMap)
		if !ok || cm.Namespace != namespace || cm.Labels[stateLabel] != "commit" {
			return nil
		}

		var m manifest
		if json.Unmarshal([]byte(cm.Data["manifest"]), &m) != nil {
			return nil
		}

		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: m.Universe}}}
	})
	nodeFilter := predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		return nodeChanged(e.ObjectOld, e.ObjectNew)
	}}
	serviceFilter := predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		a, aOK := e.ObjectOld.(*corev1.Service)

		b, bOK := e.ObjectNew.(*corev1.Service)
		if !aOK || !bOK {
			return false
		}

		return !reflect.DeepEqual(a.Spec, b.Spec) || !reflect.DeepEqual(desiredAnnotations(a.Annotations), desiredAnnotations(b.Annotations)) || !reflect.DeepEqual(a.DeletionTimestamp, b.DeletionTimestamp)
	}}
	podFilter := predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		a, aOK := e.ObjectOld.(*corev1.Pod)

		b, bOK := e.ObjectNew.(*corev1.Pod)
		if !aOK || !bOK {
			return false
		}

		return a.Spec.NodeName != b.Spec.NodeName || a.Status.PodIP != b.Status.PodIP || podAvailable(a) != podAvailable(b) || podReady(a) != podReady(b) || !reflect.DeepEqual(a.Labels, b.Labels) || !reflect.DeepEqual(a.OwnerReferences, b.OwnerReferences)
	}}

	return ctrl.NewControllerManagedBy(manager).Named("topology").
		Watches(&corev1.Node{}, mapUniverse, builder.WithPredicates(nodeFilter)).
		Watches(&corev1.Service{}, eventhandler.EnqueueRequestsFromMapFunc(r.serviceRequests), builder.WithPredicates(serviceFilter)).
		Watches(&corev1.Pod{}, eventhandler.EnqueueRequestsFromMapFunc(r.podRequests), builder.WithPredicates(podFilter)).
		Watches(&corev1.ConfigMap{}, mapState).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).Complete(r)
}

// EnqueueRequestsFromMapFunc maps both old and new objects on updates.
// Keep excluded Nodes indexed so their former participants can drain.
func objectUniverses(o client.Object) []string {
	name := universe(o.GetAnnotations())
	if node, ok := o.(*corev1.Node); ok {
		name = racer.NodeUniverse(node)
	}

	if name == "" {
		return nil
	}

	return []string{name}
}

func universeRequests(names ...string) []reconcile.Request {
	unique := map[string]bool{}

	for _, name := range names {
		if name != "" {
			unique[name] = true
		}
	}

	requests := make([]reconcile.Request, 0, len(unique))
	for name := range unique {
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
	}

	sort.Slice(requests, func(i, j int) bool { return requests[i].Name < requests[j].Name })

	return requests
}

func nodeChanged(old, next client.Object) bool {
	a, aOK := old.(*corev1.Node)

	b, bOK := next.(*corev1.Node)
	if !aOK || !bOK {
		return false
	}

	return racer.NodeUniverse(a) != racer.NodeUniverse(b) || racer.NodeEligible(a) != racer.NodeEligible(b) || a.Annotations[racer.FabricAnnotationKey] != b.Annotations[racer.FabricAnnotationKey] || nodeReady(a) != nodeReady(b) || a.UID != b.UID
}

func (r *reconciler) podRequests(ctx context.Context, o client.Object) []reconcile.Request {
	p, ok := o.(*corev1.Pod)
	if !ok {
		return nil
	}

	// The Pod label still names its old bootstrap universe after a Site move,
	// including when its Node has already disappeared.
	names := []string{p.Labels[universeAnnotation]}
	if p.Spec.NodeName != "" {
		var node corev1.Node
		if err := r.client.Get(ctx, types.NamespacedName{Name: p.Spec.NodeName}, &node); err == nil {
			names = append(names, racer.NodeUniverse(&node))
		}
	}

	return universeRequests(names...)
}

func originDependency(o client.Object) []string {
	s, ok := o.(*corev1.Service)
	if !ok || !isVolume(s) {
		return nil
	}

	ns := s.Annotations[originNamespaceAnnotation]
	if ns == "" {
		ns = s.Namespace
	}

	return []string{ns + "/" + s.Annotations[originServiceAnnotation]}
}

func (r *reconciler) serviceRequests(ctx context.Context, o client.Object) []reconcile.Request {
	if s, ok := o.(*corev1.Service); ok && isVolume(s) && universe(s.Annotations) == "" {
		r.report(ctx, []corev1.Service{*s.DeepCopy()}, "An explicit racer.unbounded-cloud.io/universe annotation containing the mapped Site universe is required")
	}

	names := map[string]bool{universe(o.GetAnnotations()): true}

	var dependents corev1.ServiceList
	if err := r.client.List(ctx, &dependents, client.MatchingFields{originIndex: o.GetNamespace() + "/" + o.GetName()}); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "list origin dependents")
	} else {
		for _, s := range dependents.Items {
			names[universe(s.Annotations)] = true
		}
	}

	requests := make([]reconcile.Request, 0, len(names))
	for name := range names {
		if name == "" {
			continue
		}

		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
	}

	return requests
}

func desiredAnnotations(a map[string]string) map[string]string {
	out := map[string]string{}

	for _, key := range []string{"origin-service", "origin-namespace", "origin-port", "universe", "slot-count", "listener-port", "cache-generation", "routing-algorithm", "max-candidate-attempts", "legacy-peer-wire"} {
		if value, ok := a[annotationPrefix+key]; ok {
			out[key] = value
		}
	}

	return out
}

func (r *reconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	name := request.Name
	if name == "" {
		return ctrl.Result{}, nil
	}

	if r.minInterval > 0 {
		if remaining := r.minInterval - time.Since(r.lastAttempt[name]); remaining > 0 {
			return ctrl.Result{RequeueAfter: remaining}, nil
		}

		r.lastAttempt[name] = time.Now()
	}

	previous, exists := r.loaded[name]
	if !exists {
		var err error

		previous, r.pointers[name], err = r.store.load(ctx, name)
		if err != nil {
			return ctrl.Result{}, err
		}

		if previous != nil {
			t, err := indexGeneration(previous)
			if err != nil {
				return ctrl.Result{}, err
			}

			if err := r.server.install(t); err != nil {
				return ctrl.Result{}, err
			}
		}

		r.loaded[name] = previous
	}

	var nodes corev1.NodeList
	if err := r.client.List(ctx, &nodes, client.MatchingFields{universeIndex: name}); err != nil {
		return ctrl.Result{}, err
	}

	var services corev1.ServiceList
	if err := r.client.List(ctx, &services, client.MatchingFields{universeIndex: name}); err != nil {
		return ctrl.Result{}, err
	}

	var pods []corev1.Pod

	knownNodes := map[string]bool{}
	for _, node := range nodes.Items {
		knownNodes[node.Name] = true
	}

	seenPods := map[types.NamespacedName]bool{}
	hasVolumes := false

	for _, service := range services.Items {
		if !isVolume(&service) || service.DeletionTimestamp != nil {
			continue
		}

		hasVolumes = true

		var selected corev1.PodList
		if err := r.client.List(ctx, &selected, client.InNamespace(service.Namespace), client.MatchingLabels(service.Spec.Selector)); err != nil {
			return ctrl.Result{}, err
		}

		for _, pod := range selected.Items {
			key := types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
			if seenPods[key] {
				continue
			}

			seenPods[key] = true

			if !knownNodes[pod.Spec.NodeName] && pod.Spec.NodeName != "" {
				var node corev1.Node
				if err := r.client.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, &node); err != nil {
					if apierrors.IsNotFound(err) {
						continue
					} // Node deletion precedes Pod garbage collection.

					return ctrl.Result{}, err
				}

				nodes.Items = append(nodes.Items, node)
				knownNodes[node.Name] = true
			}

			pods = append(pods, pod)
		}
	}

	if !hasVolumes {
		// Managed Pods live beside the controller's durable state. Discover them
		// independently of volume Services so initial idle subscriptions can
		// authenticate, including replacement Pods during DaemonSet upgrades.
		var selected corev1.PodList
		if err := r.client.List(ctx, &selected, client.InNamespace(r.store.namespace), client.MatchingLabels{dataplaneLabel: "true", universeAnnotation: name}); err != nil {
			return ctrl.Result{}, err
		}

		pods = selected.Items
	}

	inventory := append([]corev1.Service(nil), services.Items...)
	for _, service := range services.Items {
		if !isVolume(&service) || service.DeletionTimestamp != nil {
			continue
		}

		ref, err := originReference(&service)
		if err != nil {
			r.report(ctx, services.Items, err.Error())
			return ctrl.Result{}, err
		}

		var origin corev1.Service
		if err := r.client.Get(ctx, ref, &origin); err != nil {
			r.report(ctx, services.Items, err.Error())
			return ctrl.Result{}, err
		}
		// Keep volumes in the universe inventory unique; origins can be shared.
		found := false

		for i := range inventory {
			if inventory[i].Namespace == ref.Namespace && inventory[i].Name == ref.Name {
				found = true
				break
			}
		}

		if !found {
			inventory = append(inventory, origin)
		}
	}

	next, _, err := buildGenerationReserved(name, previous, nodes.Items, pods, inventory, r.reserved)
	if err != nil {
		r.report(ctx, services.Items, err.Error())
		return ctrl.Result{}, err
	}

	if err := r.releaseDeletedPods(ctx, next); err != nil {
		return ctrl.Result{}, err
	}

	t, err := indexGeneration(next)
	if err != nil {
		r.report(ctx, services.Items, err.Error())
		return ctrl.Result{}, err
	}

	if err := t.admit(); err != nil {
		r.report(ctx, services.Items, err.Error())
		return ctrl.Result{}, err
	}

	retryAborted := false

	if previous != nil {
		old, err := indexGeneration(previous)
		if err != nil {
			return ctrl.Result{}, err
		}

		busy, err := r.server.rolloutBusy(ctx, old)
		if err != nil {
			return ctrl.Result{}, err
		}

		if busy && reflect.DeepEqual(previous, next) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}

		r.server.mu.Lock()
		preparing := r.server.rollouts[name].phase == 1
		r.server.mu.Unlock()

		if busy && preparing {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
	}

	r.server.mu.Lock()
	rollout := r.server.rollouts[name]
	retryAborted = rollout != nil && rollout.phase == 5
	r.server.mu.Unlock()

	if previous == nil || retryAborted || !reflect.DeepEqual(previous, next) {
		if next.Revision == ^uint64(0) {
			return ctrl.Result{}, fmt.Errorf("revision exhausted")
		}

		next.Revision++

		if err := r.commitCandidate(ctx, t); err != nil {
			// Re-read the commit point after ambiguous writes or CAS conflicts.
			delete(r.loaded, name)
			delete(r.pointers, name)

			return ctrl.Result{}, err
		}
		// Refresh resourceVersion through the same uncached client. Publication
		// remains recoverable if this read fails after a successful commit.
		pointer := &corev1.ConfigMap{}
		if err := r.store.client.Get(ctx, types.NamespacedName{Namespace: r.store.namespace, Name: stateName(name)}, pointer); err != nil {
			delete(r.loaded, name)
			return ctrl.Result{}, err
		}

		r.pointers[name] = pointer

		r.loaded[name] = next
		if err := r.server.install(t); err != nil {
			return ctrl.Result{}, err
		}
	}

	for _, volume := range next.volumes() {
		var service *corev1.Service

		for i := range services.Items {
			s := &services.Items[i]
			if s.Namespace+"/"+s.Name == volume.Volume.ID {
				service = s
				break
			}
		}

		if service == nil {
			continue
		}

		base := service.DeepCopy()
		local := corev1.ServiceInternalTrafficPolicyLocal

		service.Spec.InternalTrafficPolicy = &local
		if service.Spec.Type == corev1.ServiceTypeNodePort || service.Spec.Type == corev1.ServiceTypeLoadBalancer {
			service.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal
		}

		service.Spec.Ports[0].TargetPort = intstr.FromInt32(volume.Volume.Port)
		if service.Annotations == nil {
			service.Annotations = map[string]string{}
		}

		service.Annotations[annotationPrefix+"allocated-port"] = fmt.Sprint(volume.Volume.Port)
		service.Annotations[annotationPrefix+"universe-id"] = identity("universe", name)

		service.Annotations[annotationPrefix+"status"] = "Published; readiness follows dataplane activation"
		if len(volume.Owners) == 0 {
			service.Annotations[annotationPrefix+"status"] = "Waiting for available dataplane Pods"
		}

		if !reflect.DeepEqual(base, service) {
			if err := r.client.Patch(ctx, service, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	// Resync covers a missed cross-resource mapping (e.g. Pod deletion after
	// its Node disappeared); it does not poll the Kubernetes API inventory.
	return ctrl.Result{RequeueAfter: time.Second}, nil
}

// Candidate admission and commit share the subscription writer lock.

// Serialize capacity planning, durable topology commit and installation against
// subscriptions. A concurrent new boot must not consume the planned capacity
// between admission and publication. No speculative rollout record is written.
func (r *reconciler) commitCandidate(ctx context.Context, t *topologyIndex) error {
	s := r.server
	s.mu.Lock()
	defer s.mu.Unlock()
	{
		var raw string

		u := identityBytes("universe", t.g.Universe)
		if s.source != nil && s.source.topologies[[32]byte(u)] != nil {
			old, err := s.rolloutFor(ctx, s.source.topologies[[32]byte(u)])
			if err != nil {
				return err
			}

			if old.phase >= 2 && old.phase <= 4 {
				if err := s.planForwardLocked(ctx, s.source.topologies[[32]byte(u)], t); err != nil {
					return err
				}
				// Preserve B13 terminal catch-up through corrective advancement.
				terminal, err := s.advanceRemovals(t.g.Universe, old, 4, old.pointer.Data["removals"])
				if err != nil {
					return err
				}

				if terminal != old.pointer.Data["removals"] {
					ds, err := removalHistory(terminal, t.g.Universe, old.revision)
					if err != nil {
						return err
					}

					if err = s.saveRemovals(ctx, old, ds); err != nil {
						return err
					}
				}
			}

			raw = old.pointer.Data["removals"]
		}

		entries, err := removalHistory(raw, t.g.Universe, t.g.Revision)
		if err != nil {
			return err
		}

		if _, err := planRemovals(t, &rollout{revision: t.g.Revision, phase: 1}, 1, entries); err != nil {
			return fmt.Errorf("candidate catch-up admission: %w", err)
		}
	}

	if err := r.store.commit(ctx, t.g, r.pointers[t.g.Universe]); err != nil {
		// The candidate may already be durable. Until Reconcile reloads it,
		// old-topology boot admissions must not consume its reserved capacity.
		if s.source != nil {
			u := identityBytes("universe", t.g.Universe)
			delete(s.source.topologies, [32]byte(u))
		}

		return err
	}

	return s.installLocked(t)
}

// Historical Pod authority and Service diagnostics.

// PodUID is subscription authority, not historical snapshot identity. Only an
// uncached, unfiltered inventory can prove an excluded Pod is gone: absence from
// Service selectors, Node readiness, and Pod availability cannot prove deletion.
// Change the proposal only; history GC and authorization use the committed view.
func (r *reconciler) releaseDeletedPods(ctx context.Context, g *generation) error {
	needed := false

	for _, m := range g.Nodes {
		if m.IP == "" && m.PodUID != "" {
			needed = true
			break
		}
	}

	if !needed {
		return nil
	}

	var pods corev1.PodList
	if err := r.store.client.List(ctx, &pods); err != nil {
		return err
	}

	present := make(map[string]bool, len(pods.Items))
	for _, p := range pods.Items {
		present[string(p.UID)] = true
	}

	for name, m := range g.Nodes {
		if m.IP == "" && m.PodUID != "" && !present[m.PodUID] {
			m.PodUID = ""
			g.Nodes[name] = m
		}
	}

	return nil
}

func (r *reconciler) report(ctx context.Context, services []corev1.Service, message string) {
	if len(message) > 1024 {
		message = message[:1024]
	}

	for i := range services {
		s := &services[i]
		if !isVolume(s) {
			continue
		}

		if s.Annotations[annotationPrefix+"status"] == message {
			continue
		}

		base := s.DeepCopy()

		s.Annotations[annotationPrefix+"status"] = message
		if err := r.client.Patch(ctx, s, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil && !apierrors.IsNotFound(err) {
			ctrl.LoggerFrom(ctx).Error(err, "recording volume diagnostic")
		}
	}
}
