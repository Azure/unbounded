// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/rebound/ledger"
)

// sweepInterval bounds the delay between checks of active and released nodes.
// Pods can arrive after a sweep because placement uses a cached ledger or
// a pod creation that started during a grant finishes after its release.
const sweepInterval = 15 * time.Second

// maxListAttempts bounds how often a reconcile lists pods while the ledger
// keeps changing before it gives up and retries with backoff.
const maxListAttempts = 3

// evictor deletes pods that a grant does not allow on its nodes:
//
//   - While a grant is active, pods from other namespaces.
//   - After a grant is removed, pods from its namespace that were created
//     while it was active. A namespace recreated with the same name does not
//     count as the grant's namespace.
//
// Pods are deleted with a zero grace period and PodDisruptionBudgets do not
// apply: the grant's TTL is the grace period.
//
// The ledger and pods are both read live. A cached ledger can lag behind the
// live pod list, for example still showing a node's previous grant while the
// pod list already holds pods admitted under its new one.
type evictor struct {
	client    client.Client
	apiReader client.Reader
	namespace string
	exempt    map[string]bool
	clock     clock.Clock

	ledgerMu       sync.Mutex
	ledgerVersion  string
	cachedLedger   *ledger.Ledger
	releasesByNode map[string][]*ledger.Release
}

func (r *evictor) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	node := req.Name

	version, active, releases, err := r.ledgerRecords(ctx, node)
	if err != nil {
		return ctrl.Result{}, err
	}

	var pods corev1.PodList

	// The ledger can change while pods are listed. For example, the node can
	// move to another namespace whose new pods are in the list. Only judge
	// pods against a ledger version that did not change during the list.
	for attempt := 1; ; attempt++ {
		if active == nil && len(releases) == 0 {
			return ctrl.Result{}, nil
		}

		if err := r.apiReader.List(ctx, &pods, client.MatchingFields{"spec.nodeName": node}); err != nil {
			return ctrl.Result{}, err
		}

		after, afterActive, afterReleases, err := r.ledgerRecords(ctx, node)
		if err != nil {
			return ctrl.Result{}, err
		}

		// Use the records from the second read so expiry is checked again.
		active, releases = afterActive, afterReleases

		if after == version {
			break
		}

		if attempt == maxListAttempts {
			return ctrl.Result{}, fmt.Errorf("ledger changed while listing pods on node %s", node)
		}

		version = after
	}

	var errs []error

	// Namespace UIDs read live during this reconcile. Empty means not found.
	uids := make(map[string]types.UID)
	namespaceUID := func(name string) (types.UID, error) {
		if uid, ok := uids[name]; ok {
			return uid, nil
		}

		var ns corev1.Namespace

		err := r.apiReader.Get(ctx, types.NamespacedName{Name: name}, &ns)

		switch {
		case apierrors.IsNotFound(err):
			uids[name] = ""
		case err != nil:
			return "", fmt.Errorf("get namespace %s: %w", name, err)
		default:
			uids[name] = ns.UID
		}

		return uids[name], nil
	}

	superseded := false

	for i := range pods.Items {
		pod := &pods.Items[i]

		evict, err := r.shouldEvict(pod, active, releases, namespaceUID)
		if err != nil {
			errs = append(errs, err)
			continue
		}

		if !evict {
			continue
		}

		// An earlier delete can block while the grant expires or the node
		// moves to another namespace. The UID precondition does not catch
		// that, so recheck the ledger and leave the rest to a new sweep.
		// Within one version, the grant can only change by expiring, so a
		// metadata read is enough.
		current, err := r.liveLedgerVersion(ctx)
		if err != nil {
			errs = append(errs, err)
			break
		}

		if current != version || (active != nil && active.Expired(r.clock.Now())) {
			superseded = true
			break
		}

		ctrl.LoggerFrom(ctx).Info("deleting pod not allowed by node grant", "node", node, "pod", client.ObjectKeyFromObject(pod))

		err = r.client.Delete(ctx, pod, client.GracePeriodSeconds(0), client.Preconditions{UID: &pod.UID})
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			errs = append(errs, err)
		}
	}

	if err := errors.Join(errs...); err != nil {
		return ctrl.Result{}, err
	}

	if superseded {
		return ctrl.Result{RequeueAfter: time.Millisecond}, nil
	}

	var requeueAfter time.Duration
	if active != nil {
		requeueAfter = sweepInterval
	}

	now := r.clock.Now()
	for _, rel := range releases {
		if remaining := rel.DropAt.Sub(now); remaining > 0 {
			delay := max(time.Millisecond, min(sweepInterval, remaining))
			if requeueAfter == 0 || delay < requeueAfter {
				requeueAfter = delay
			}
		}
	}

	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// ledgerRecords reads the ledger live and returns its resourceVersion, the
// node's unexpired grant, and the node's release records. A missing ledger
// has version "" and no records.
func (r *evictor) ledgerRecords(ctx context.Context, node string) (string, *ledger.Grant, []*ledger.Release, error) {
	var cm corev1.ConfigMap
	if err := r.apiReader.Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: ledger.ConfigMapName}, &cm); err != nil {
		return "", nil, nil, client.IgnoreNotFound(err)
	}

	g, releases := r.nodeRecords(&cm, node)
	if g != nil && g.Expired(r.clock.Now()) {
		g = nil
	}

	return cm.ResourceVersion, g, releases, nil
}

// liveLedgerVersion reads only the ledger's metadata live and returns its
// resourceVersion. A missing ledger has version "", as in ledgerRecords.
func (r *evictor) liveLedgerVersion(ctx context.Context) (string, error) {
	meta := &metav1.PartialObjectMetadata{}
	meta.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("ConfigMap"))

	if err := r.apiReader.Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: ledger.ConfigMapName}, meta); err != nil {
		return "", client.IgnoreNotFound(err)
	}

	return meta.ResourceVersion, nil
}

// nodeRecords returns read-only records from an index rebuilt once per ledger version.
func (r *evictor) nodeRecords(cm *corev1.ConfigMap, node string) (*ledger.Grant, []*ledger.Release) {
	r.ledgerMu.Lock()
	defer r.ledgerMu.Unlock()

	if r.cachedLedger == nil || r.ledgerVersion != cm.ResourceVersion {
		r.cachedLedger = ledger.Parse(cm.Data)
		r.releasesByNode = make(map[string][]*ledger.Release)

		for _, rel := range r.cachedLedger.Releases() {
			for _, name := range rel.Nodes {
				r.releasesByNode[name] = append(r.releasesByNode[name], rel)
			}
		}

		r.ledgerVersion = cm.ResourceVersion
	}

	key, _ := r.cachedLedger.NodeGrant(node)
	g, _ := r.cachedLedger.Get(key)

	return g, r.releasesByNode[node]
}

// shouldEvict reports whether pod must be deleted. namespaceUID returns the
// live UID of a namespace, or "" if it does not exist. It is only called for
// pods that match a release recorded with a namespace UID.
func (r *evictor) shouldEvict(pod *corev1.Pod, active *ledger.Grant, releases []*ledger.Release, namespaceUID func(string) (types.UID, error)) (bool, error) {
	if r.exempt[pod.Namespace] || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return false, nil
	}

	if isMirrorPod(pod) {
		return false, nil
	}

	if g := pod.DeletionGracePeriodSeconds; pod.DeletionTimestamp != nil && g != nil && *g == 0 {
		return false, nil
	}

	if active != nil && pod.Namespace != active.Namespace {
		return true, nil
	}

	// creationTimestamp has one-second precision, so truncate both grant boundaries.
	// Keep pods from the release second to avoid deleting pods admitted after release.
	// This also keeps pods created just before release in that same second.
	created := pod.CreationTimestamp.Time
	for _, rel := range releases {
		if pod.Namespace != rel.Namespace || created.Before(rel.CreatedAt.Truncate(time.Second)) || !created.Before(rel.ReleasedAt.Truncate(time.Second)) {
			continue
		}

		// Releases without a namespace UID match by name only.
		if rel.NamespaceUID == "" {
			return true, nil
		}

		// A namespace recreated with the same name is not the grant's namespace.
		// If the namespace is gone, its pods are garbage collected anyway.
		uid, err := namespaceUID(pod.Namespace)
		if err != nil {
			return false, err
		}

		if uid == rel.NamespaceUID {
			return true, nil
		}
	}

	return false, nil
}

// mapLedger enqueues every node named by a grant or a release.
func (r *evictor) mapLedger(_ context.Context, obj client.Object) []ctrl.Request {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok || cm.Namespace != r.namespace || cm.Name != ledger.ConfigMapName {
		return nil
	}

	l := ledger.Parse(cm.Data)
	names := make(map[string]struct{})

	for _, g := range l.Grants() {
		for _, node := range g.Nodes {
			names[node] = struct{}{}
		}
	}

	for _, rel := range l.Releases() {
		for _, node := range rel.Nodes {
			names[node] = struct{}{}
		}
	}

	requests := make([]ctrl.Request, 0, len(names))
	for name := range names {
		requests = append(requests, ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	}

	return requests
}

// isMirrorPod reports whether pod looks like a kubelet mirror pod: it has
// the mirror annotation and its controller is the node it runs on. Users can
// forge both, but kubelet never runs a mirror pod it did not create and
// deletes such orphans, so a forged one holds no resources.
func isMirrorPod(pod *corev1.Pod) bool {
	if _, ok := pod.Annotations[corev1.MirrorPodAnnotationKey]; !ok {
		return false
	}

	owner := metav1.GetControllerOf(pod)

	return owner != nil && owner.Kind == "Node" && owner.APIVersion == "v1" && owner.Name == pod.Spec.NodeName
}
