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

// handoffMargin is how long before ledger.HandoffCooldown ends a delete
// authorized by a ledger read must be finished. It covers the time a delete
// takes to commit after its context ends and clock skew between rebound
// replicas. Release write latency needs no margin: the cooldown counts from a
// confirmation time read after the removal committed. Each delete runs within
// HandoffCooldown minus this margin of the read that authorized it.
const handoffMargin = 4 * time.Second

// evictor deletes pods that a grant does not allow on its nodes:
//
//   - While a grant is active, pods from other namespaces.
//   - After a grant is removed, pods from its namespace that were created
//     or scheduled onto the node while it was active. A namespace recreated
//     with the same name does not count as the grant's namespace.
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
		//
		// The read also starts the delete's deadline. A removal that the
		// read did not see commits after checkedAt, and its release is
		// confirmed by a write that read it back, so ConfirmedAt is after
		// checkedAt however slow the removal write was. The API server
		// rejects new grants on the node while the release is unconfirmed
		// and for ledger.HandoffCooldown after ConfirmedAt, so a delete
		// that ends by the deadline cannot remove a pod that a new grant
		// admitted. A grant that expires is confirmed at ExpiresAt, which
		// is after checkedAt because of the expiry check below.
		// Clocks of rebound replicas are assumed to agree within the
		// margin; the controller and API server run in the same binary
		// but can be on different replicas.
		//
		// Deletes for release records get the same deadline, though they
		// do not need it: they only match pods created before ReleasedAt,
		// which no later grant admitted.
		checkedAt := r.clock.Now()

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

		timeout := checkedAt.Add(ledger.HandoffCooldown - handoffMargin).Sub(r.clock.Now())
		if timeout <= 0 {
			superseded = true
			break
		}

		ctrl.LoggerFrom(ctx).Info("deleting pod not allowed by node grant", "node", node, "pod", client.ObjectKeyFromObject(pod))

		// The resourceVersion precondition makes the delete fail with a
		// conflict if the pod changed since it was listed, for example by
		// finishing. Conflicts are ignored and the next sweep looks again.
		deleteCtx, cancel := context.WithTimeout(ctx, timeout)
		err = r.client.Delete(deleteCtx, pod, client.GracePeriodSeconds(0),
			client.Preconditions{UID: &pod.UID, ResourceVersion: &pod.ResourceVersion})

		cancel()

		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			superseded = true
			break
		}

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

	// A release covers pods placed on the node while the grant was active.
	// The lower bound uses placedAt, not creationTimestamp: a pod created and
	// left Pending before the grant can be bound to the node during it. The
	// upper bound uses creationTimestamp, since pods created after release
	// were admitted under whatever holds the node next. It does not use
	// placedAt: kubelet can set PodScheduled after release for a pod created
	// with spec.nodeName during the grant, and that pod must not outlive it.
	// The cost is that a pod created before release but bound after it is
	// deleted once; its replacement is created after release and is kept.
	//
	// Both timestamps have one-second precision, so truncate both grant
	// boundaries. Keep pods created in the release second to avoid deleting
	// pods admitted after release. This also keeps pods created just before
	// release in that same second.
	created := pod.CreationTimestamp.Time
	placed := placedAt(pod)

	for _, rel := range releases {
		if pod.Namespace != rel.Namespace || placed.Before(rel.CreatedAt.Truncate(time.Second)) || !created.Before(rel.ReleasedAt.Truncate(time.Second)) {
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

// placedAt returns when pod was placed on its node: the later of its
// creationTimestamp and the lastTransitionTime of its PodScheduled=True
// condition. The API server sets that condition when it binds a pod, so a
// pod that waited in Pending is placed when it is bound. For a pod created
// with spec.nodeName set, kubelet sets the condition after creation, and
// placement happens no earlier than creation.
func placedAt(pod *corev1.Pod) time.Time {
	placed := pod.CreationTimestamp.Time

	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionTrue && c.LastTransitionTime.After(placed) {
			placed = c.LastTransitionTime.Time
		}
	}

	return placed
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
