// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
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
	// pods against node records that did not change during the list. All
	// grants share one ledger, so changes for other nodes are ignored.
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

		unchanged := after == version || sameNodeRecords(active, releases, afterActive, afterReleases)

		// Use the records from the second read so expiry is checked again.
		version, active, releases = after, afterActive, afterReleases

		if unchanged {
			break
		}

		if attempt == maxListAttempts {
			return ctrl.Result{}, fmt.Errorf("ledger changed while listing pods on node %s", node)
		}
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
		// metadata read is enough. A new version is read in full and only
		// stops the sweep if it changed this node's records.
		current, err := r.liveLedgerVersion(ctx)
		if err != nil {
			errs = append(errs, err)
			break
		}

		if current != version {
			recorded, currentActive, currentReleases, err := r.recordsAt(ctx, current, node)
			if err != nil {
				errs = append(errs, err)
				break
			}

			if !sameNodeRecords(active, releases, currentActive, currentReleases) {
				superseded = true
				break
			}

			version = recorded
		}

		if active != nil && active.Expired(r.clock.Now()) {
			superseded = true
			break
		}

		ctrl.LoggerFrom(ctx).Info("deleting pod not allowed by node grant", "node", node, "pod", client.ObjectKeyFromObject(pod))

		// The resourceVersion precondition makes the delete fail with a
		// conflict if the pod changed since it was listed, for example by
		// finishing. Conflicts are ignored and the next sweep looks again.
		// Nothing ties this delete to the ledger version checked above, so a
		// handoff that lands between the two calls can still delete a pod
		// the new grant allows. The README lists this as a known limit.
		err = r.client.Delete(ctx, pod, client.GracePeriodSeconds(0),
			client.Preconditions{UID: &pod.UID, ResourceVersion: &pod.ResourceVersion})
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
// has version "" and no records. It reads the ledger's metadata first and
// fetches the full ledger only when its version is not cached.
func (r *evictor) ledgerRecords(ctx context.Context, node string) (string, *ledger.Grant, []*ledger.Release, error) {
	version, err := r.liveLedgerVersion(ctx)
	if err != nil {
		return "", nil, nil, err
	}

	return r.recordsAt(ctx, version, node)
}

// recordsAt is ledgerRecords after the metadata read that returned version.
func (r *evictor) recordsAt(ctx context.Context, version, node string) (string, *ledger.Grant, []*ledger.Release, error) {
	if version == "" {
		return "", nil, nil, nil
	}

	g, releases, ok := r.cachedRecords(version, node)
	if !ok {
		var cm corev1.ConfigMap
		if err := r.apiReader.Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: ledger.ConfigMapName}, &cm); err != nil {
			return "", nil, nil, client.IgnoreNotFound(err)
		}

		// The ledger can change after the metadata read. Return the version
		// of the records.
		version = cm.ResourceVersion
		g, releases = r.nodeRecords(&cm, node)
	}

	if g != nil && g.Expired(r.clock.Now()) {
		g = nil
	}

	return version, g, releases, nil
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

	return r.lookupLocked(node)
}

// cachedRecords returns read-only records from the index if it holds
// version, and false otherwise.
func (r *evictor) cachedRecords(version, node string) (*ledger.Grant, []*ledger.Release, bool) {
	r.ledgerMu.Lock()
	defer r.ledgerMu.Unlock()

	if r.cachedLedger == nil || r.ledgerVersion != version {
		return nil, nil, false
	}

	g, releases := r.lookupLocked(node)

	return g, releases, true
}

// lookupLocked returns the node's records from the index. The caller must
// hold ledgerMu.
func (r *evictor) lookupLocked(node string) (*ledger.Grant, []*ledger.Release) {
	key, _ := r.cachedLedger.NodeGrant(node)
	g, _ := r.cachedLedger.Get(key)

	return g, r.releasesByNode[node]
}

// sameNodeRecords reports whether two reads of a node's records judge pods
// the same way: the same unexpired grant, or none, and the same release
// records in any order. Expired grants are already nil from ledgerRecords,
// so a grant that expired between the reads counts as a change.
func sameNodeRecords(aActive *ledger.Grant, aReleases []*ledger.Release, bActive *ledger.Grant, bReleases []*ledger.Release) bool {
	if (aActive == nil) != (bActive == nil) {
		return false
	}

	if aActive != nil && (aActive.Namespace != bActive.Namespace || aActive.Name != bActive.Name ||
		aActive.UID != bActive.UID || aActive.NamespaceUID != bActive.NamespaceUID ||
		!aActive.ExpiresAt.Equal(bActive.ExpiresAt)) {
		return false
	}

	if len(aReleases) != len(bReleases) {
		return false
	}

	byUID := func(x, y *ledger.Release) int { return cmp.Compare(x.UID, y.UID) }
	a := slices.SortedFunc(slices.Values(aReleases), byUID)
	b := slices.SortedFunc(slices.Values(bReleases), byUID)

	return slices.EqualFunc(a, b, func(x, y *ledger.Release) bool {
		return x.UID == y.UID && x.Namespace == y.Namespace && x.NamespaceUID == y.NamespaceUID &&
			x.CreatedAt.Equal(y.CreatedAt) && x.ReleasedAt.Equal(y.ReleasedAt) && x.DropAt.Equal(y.DropAt)
	})
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
