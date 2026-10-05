// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/authority"
	"github.com/Azure/unbounded/internal/racer/members"
	"github.com/Azure/unbounded/internal/racer/server"
	"github.com/Azure/unbounded/internal/racer/wire"
)

type TopologyReconciler struct {
	authority *authority.Authority
	settings  frozenConfig
	client.Client
	APIReader client.Reader
	Config    Config
}

func (r *TopologyReconciler) runtimeConfig() Config { return r.settings.get(&r.Config) }

// Reconcile builds from the synchronized cache, reads the version ConfigMap
// authoritatively, commits counters/hashes with CAS, then installs the result.
// Conflicts requeue from fresh inputs; missing established counters fail closed.
func (r *TopologyReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	if err := ctx.Err(); err != nil {
		return ctrl.Result{}, reconcile.TerminalError(err)
	}

	if err := r.runtimeConfig().Validate(); err != nil {
		return ctrl.Result{}, reconcile.TerminalError(err)
	}

	err := r.reconcile(ctx)
	// Never schedule retries from a canceled leadership operation, even if a
	// transport returned Conflict concurrently with cancellation.
	if ctx.Err() != nil {
		return ctrl.Result{}, reconcile.TerminalError(ctx.Err())
	}

	if apierrors.IsConflict(err) {
		return ctrl.Result{RequeueAfter: retryConflictDelay}, nil
	}

	return ctrl.Result{}, err
}

func (r *TopologyReconciler) reconcile(ctx context.Context) error {
	update, err := r.publish(ctx)
	if err != nil {
		return err
	}

	return r.annotate(ctx, update)
}

// publish protects authoritative reads, CAS, and local installation. Annotation
// writes are recovery hints, not authority, and must not block trust observation.
func (r *TopologyReconciler) publish(ctx context.Context) (authority.TopologyHints, error) {
	return r.authority.PublishTopology(ctx, r.observeTopology)
}

// TopologyObservation contains discovery inputs, not accepted history or proofs.
type TopologyObservation = authority.TopologyObservation

func (r *TopologyReconciler) observeTopology(ctx context.Context) (TopologyObservation, error) {
	cfg := r.runtimeConfig()

	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return TopologyObservation{}, err
	}

	var caches racerv1.ClusterCacheList
	if err := r.APIReader.List(ctx, &caches); err != nil {
		return TopologyObservation{}, err
	}

	catalog, err := members.BuildCatalog(caches.Items)
	if err != nil {
		return TopologyObservation{}, err
	}

	ownership, err := readManagedWorkloadIdentities(ctx, r.APIReader, cfg)
	if err != nil {
		return TopologyObservation{}, err
	}
	// Indexed namespace-scoped queries avoid scanning unrelated Pods for each
	// Node. Ownership is still verified against the current DaemonSet UID.
	podsByNode := make(map[string][]corev1.Pod, len(nodes.Items))

	for _, node := range nodes.Items {
		if err := ctx.Err(); err != nil {
			return TopologyObservation{}, err
		}

		var list corev1.PodList
		if err := r.List(ctx, &list, client.InNamespace(cfg.Namespace), client.MatchingFields{podNodeIndex: node.Name}); err != nil {
			return TopologyObservation{}, err
		}

		podsByNode[node.Name] = list.Items
	}

	return TopologyObservation{Nodes: nodes, Catalog: catalog, Input: members.Input{
		Nodes: nodes.Items, PodsByNode: podsByNode, Ownership: ownership.observed(), PeerPort: cfg.PeerPort,
	}}, nil
}

func (r *TopologyReconciler) annotate(ctx context.Context, update authority.TopologyHints) error {
	for i := range update.Nodes.Items {
		node := &update.Nodes.Items[i]

		member, ok := update.Members[wire.NodeID(node.UID)]
		if !ok {
			if _, excluded := node.Labels[wire.ExclusionLabel]; excluded && node.Annotations[admittedMemberAnnotation] != "" {
				before := node.DeepCopy()
				delete(node.Annotations, admittedMemberAnnotation)

				if err := r.Patch(ctx, node, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
					return err
				}
			}

			continue
		}

		encoded, err := json.Marshal(member)
		if err != nil {
			return err
		}

		if len(encoded) > 64*1024 {
			return wire.TooLarge
		}

		if node.Annotations[admittedMemberAnnotation] == string(encoded) {
			continue
		}

		before := node.DeepCopy()
		if node.Annotations == nil {
			node.Annotations = map[string]string{}
		}

		node.Annotations[admittedMemberAnnotation] = string(encoded)
		if err := r.Patch(ctx, node, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}

	return nil
}

func (r *TopologyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	cfg := r.runtimeConfig()

	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, podNodeIndex, podNodeKeys); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("racer-topology").
		WatchesRawSource(initialEnqueue()).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(nodeChanges())).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(managedPodChanges(cfg))).
		Watches(&appsv1.DaemonSet{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(namedChanges(cfg.Namespace, managedWorkloadNames(cfg)...))).
		Watches(&racerv1.ClusterCache{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(cacheChanges())).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(namedChanges(cfg.Namespace, cfg.CredentialsSecretName))).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(versionChanges(cfg))).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

// singleton coalesces input changes without introducing a singleton CR.
func singleton(_ context.Context, _ client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "racer"}}}
}

// DataplaneWorkloadIdentities is a bounded snapshot of live workload identities.
// Refresh it for each authorization or topology pass; labels are not ownership.
type DataplaneWorkloadIdentities struct {
	namespace string
	workloads [2]workloadIdentity
}

type workloadIdentity struct {
	name string
	uid  types.UID
}

func (ids DataplaneWorkloadIdentities) observed() members.WorkloadIdentities {
	observed := members.WorkloadIdentities{Namespace: ids.namespace}
	for i, workload := range ids.workloads {
		observed.Workloads[i] = members.WorkloadIdentity{Name: workload.name, UID: workload.uid}
	}

	return observed
}

// Custom standalone installations retain their single configured workload.
// Operator installations use both fixed names, never a label-derived allowlist.
func readManagedWorkloadIdentities(ctx context.Context, reader client.Reader, cfg Config) (DataplaneWorkloadIdentities, error) {
	ids := DataplaneWorkloadIdentities{namespace: cfg.Namespace}
	for i, name := range managedWorkloadNames(cfg) {
		ids.workloads[i].name = name

		var ds appsv1.DaemonSet
		if err := reader.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: name}, &ds); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}

			return DataplaneWorkloadIdentities{}, err
		}

		if ds.DeletionTimestamp == nil {
			ids.workloads[i].uid = ds.UID
		}
	}

	return ids, nil
}

const (
	enrolledSharesAnnotation   = members.EnrolledSharesAnnotation
	enrolledRDMANICsAnnotation = members.EnrolledRDMANICsAnnotation
	admittedMemberAnnotation   = members.AdmittedMemberAnnotation
)

type RotationPolicy = authority.RotationPolicy

type KeyringReconciler struct {
	settings  frozenConfig
	Config    Config
	authority *authority.Authority
}

func (r *KeyringReconciler) runtimeConfig() Config { return r.settings.get(&r.Config) }

func (r *KeyringReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	delay, err := r.authority.ReconcileCredentials(ctx)
	if ctx.Err() != nil {
		return ctrl.Result{}, reconcile.TerminalError(ctx.Err())
	}

	if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
		return ctrl.Result{RequeueAfter: retryConflictDelay}, nil
	}

	return ctrl.Result{RequeueAfter: delay}, err
}

func (r *KeyringReconciler) SetupWithManager(mgr ctrl.Manager) error {
	cfg := r.runtimeConfig()

	return ctrl.NewControllerManagedBy(mgr).
		Named("racer-credentials").
		WatchesRawSource(initialEnqueue()).
		Watches(&racerv1.ClusterCache{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(cacheChanges())).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(namedChanges(cfg.Namespace, cfg.CredentialsSecretName))).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(versionChanges(cfg))).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).Complete(r)
}

const (
	ReplicationAudience = authority.ReplicationAudience
)

// Replication observes durable authority on every replica. No request from a
// dataplane performs these reads. Only the elected publisher supplies image bytes.
type Replication struct {
	authority *authority.Authority
	settings  frozenConfig
	Config    Config
	Client    client.Client
	APIReader client.Reader
	mu        sync.Mutex
	leader    context.Context
}

func (r *Replication) runtimeConfig() Config { return r.settings.get(&r.Config) }

type publisherLifetime struct{ replication *Replication }

func (*publisherLifetime) NeedLeaderElection() bool { return true }
func (p *publisherLifetime) Start(ctx context.Context) error {
	p.replication.mu.Lock()
	p.replication.leader = ctx
	p.replication.mu.Unlock()
	<-ctx.Done()

	return nil
}

func (r *Replication) isLeader() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.leader != nil && r.leader.Err() == nil
}

func (*Replication) NeedLeaderElection() bool { return false }

func (r *Replication) interval() time.Duration {
	return min(5*time.Second, r.runtimeConfig().SnapshotMaxAge/3)
}

func (r *Replication) Start(ctx context.Context) error {
	// Credential observations must continue while the follower's single poll is
	// blocked or its leader is unreachable.
	done := make(chan struct{})

	go func() {
		defer close(done)

		for ctx.Err() == nil {
			observation, cancel := context.WithTimeout(ctx, r.interval())
			r.observe(observation)
			cancel()

			if !replicationSleep(ctx, r.interval()) {
				return
			}
		}
	}()

	defer func() { <-done }()

	for ctx.Err() == nil {
		if !r.isLeader() {
			poll, cancel := context.WithTimeout(ctx, 2*r.interval()+r.runtimeConfig().Limits.WriteTimeout)
			if err := r.poll(poll, ctx); err != nil && ctx.Err() == nil {
				ctrl.LoggerFrom(ctx).V(1).Info("snapshot replication retry", "error", err)
			}

			cancel()
		}

		if !replicationSleep(ctx, r.interval()/2+time.Duration(rand.Int64N(int64(r.interval()/2)+1))) {
			break
		}
	}

	return nil
}

func replicationSleep(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (r *Replication) observe(ctx context.Context) {
	if err := r.operationAuthority().Observe(ctx); err != nil && ctx.Err() == nil {
		ctrl.LoggerFrom(ctx).V(1).Info("authority observation retry", "error", err)
	}
}

func (r *Replication) operationAuthority() *authority.Authority { return r.authority }

// Observe refreshes local serving authority without network replication.

// installReplica is the alternate proof to publisher CAS: bounded canonical
// decoding plus exact authoritative durable confirmation, never a trusted hash
// supplied by the remote peer. No blob is persisted.
func (r *Replication) installReplica(ctx, process context.Context, image wire.Publication) error {
	return r.operationAuthority().AcceptReplica(ctx, process, image)
}

// AcceptReplica installs only canonical bytes matching authoritative durable state.

func (r *Replication) leaderAddress(ctx context.Context) (string, error) {
	var lease coordv1.Lease
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: r.runtimeConfig().Namespace, Name: "racer-controller"}, &lease); err != nil {
		return "", err
	}

	if lease.Spec.HolderIdentity == nil || lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil || *lease.Spec.LeaseDurationSeconds <= 0 || time.Since(lease.Spec.RenewTime.Time) >= time.Duration(*lease.Spec.LeaseDurationSeconds)*time.Second {
		return "", wire.Unavailable
	}

	name, uid, ok := strings.Cut(*lease.Spec.HolderIdentity, "/")
	if !ok || name == "" || uid == "" {
		return "", wire.Unavailable
	}

	var pod corev1.Pod
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: r.runtimeConfig().Namespace, Name: name}, &pod); err != nil {
		return "", err
	}

	if string(pod.UID) != uid || !r.controllerPod(&pod) || net.ParseIP(pod.Status.PodIP) == nil {
		return "", wire.Unavailable
	}

	return net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(int(r.runtimeConfig().ReplicationPort))), nil
}

func (r *Replication) controllerPod(pod *corev1.Pod) bool {
	return controllerPod(r.runtimeConfig(), pod)
}

func controllerPod(cfg Config, pod *corev1.Pod) bool {
	return pod.Namespace == cfg.Namespace && pod.UID != "" && pod.DeletionTimestamp == nil && pod.Spec.ServiceAccountName == cfg.ControllerServiceAccount && pod.Status.Phase != corev1.PodFailed && pod.Status.Phase != corev1.PodSucceeded
}

func (r *Replication) poll(ctx, process context.Context) error {
	address, err := r.leaderAddress(ctx)
	if err != nil {
		return err
	}

	pem, err := os.ReadFile(r.runtimeConfig().ReplicationTrustFile)
	if err != nil {
		return err
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return wire.Unavailable
	}

	token, err := os.ReadFile(r.runtimeConfig().ReplicationTokenFile)
	if err != nil {
		return err
	}

	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: r.runtimeConfig().ReplicationServerName}, TLSHandshakeTimeout: r.interval(), DisableKeepAlives: true}
	defer transport.CloseIdleConnections()

	httpClient := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	path := "https://" + address + server.ReplicationPath
	if current, err := r.operationAuthority().Current(); err == nil {
		path += "?after=" + strconv.FormatUint(uint64(current.Sequence()), 10)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}

	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))

	response, err := httpClient.Do(request)
	if err != nil {
		return err
	}

	defer func() {
		if err := response.Body.Close(); err != nil {
			ctrl.LoggerFrom(ctx).V(1).Info("close replication response", "error", err)
		}
	}()

	if response.StatusCode == http.StatusNoContent {
		return nil
	} // Not a freshness confirmation.

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("replication HTTP status %d", response.StatusCode)
	}

	image, err := wire.DecodePublication(response.Body)
	if err != nil {
		return err
	}

	return r.installReplica(ctx, process, image)
}

func (r *Replication) authenticate(ctx context.Context, request *http.Request) (string, time.Time, error) {
	identity, err := r.operationAuthority().AuthenticateReplica(ctx, request)
	return identity.UID(), identity.Expires(), err
}

// LeaderContext snapshots publisher lifetime and leadership under the same lock.
func (r *Replication) LeaderContext() (context.Context, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.leader, r.leader != nil && r.leader.Err() == nil
}

func (r *Replication) PollInterval() time.Duration { return r.interval() }

func (r *Replication) AuthenticateReplica(ctx context.Context, request *http.Request) (string, time.Time, error) {
	return r.authenticate(ctx, request)
}
