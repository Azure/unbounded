// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0
package server

import (
	"context"
	"net/http"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/authority"
	"github.com/Azure/unbounded/internal/racer/members"
	"github.com/Azure/unbounded/internal/racer/wire"
)

type fixtureConfig struct {
	authority.Config
	ServerConfig Config
	PeerPort     uint16
}

func (c fixtureConfig) authorityConfig() authority.Config { return c.Config }

type Application struct {
	authority   *authority.Authority
	Topology    *TopologyReconciler
	Keyring     *KeyringReconciler
	Server      *Server
	Lifecycle   *Lifecycle
	Replication *fixtureLeader
}
type TopologyReconciler struct {
	client.Client
	APIReader client.Reader
	Config    fixtureConfig
	authority *authority.Authority
}

func (r *TopologyReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	_, err := r.authority.PublishTopology(ctx, r.observeTopology)
	return ctrl.Result{}, err
}

type KeyringReconciler struct {
	Config    fixtureConfig
	authority *authority.Authority
}

func (r *KeyringReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	delay, err := r.authority.ReconcileCredentials(ctx)
	return ctrl.Result{RequeueAfter: delay}, err
}

type fixtureLeader struct {
	Config    fixtureConfig
	Client    client.Client
	APIReader client.Reader
	authority *authority.Authority
	mu        sync.Mutex
	leader    context.Context
}

func (r *fixtureLeader) LeaderContext() (context.Context, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.leader, r.leader != nil && r.leader.Err() == nil
}

func (r *fixtureLeader) PollInterval() time.Duration {
	return min(5*time.Second, r.Config.SnapshotMaxAge/3)
}

func (r *fixtureLeader) AuthenticateReplica(ctx context.Context, req *http.Request) (string, time.Time, error) {
	i, e := r.authority.AuthenticateReplica(ctx, req)
	return i.UID(), i.Expires(), e
}
func (r *fixtureLeader) observe(ctx context.Context) { _ = r.authority.Observe(ctx) }
func (r *fixtureLeader) installReplica(ctx, process context.Context, p wire.Publication) error {
	return r.authority.AcceptReplica(ctx, process, p)
}

func Assemble(cfg fixtureConfig, c client.Client, reader client.Reader) *Application {
	a := authority.New(cfg.authorityConfig(), authority.Dependencies{Writer: c, Reader: reader})
	l := NewLifecycle(a)
	r := &fixtureLeader{Config: cfg, Client: c, APIReader: reader, authority: a}

	return &Application{authority: a, Topology: &TopologyReconciler{Client: c, APIReader: reader, Config: cfg, authority: a}, Keyring: &KeyringReconciler{Config: cfg, authority: a}, Server: New(cfg.ServerConfig, c, a, l, r), Lifecycle: l, Replication: r}
}

var fixtureConfigs = map[*authority.Authority]fixtureConfig{}

type (
	NodeIdentity        = authority.NodeIdentity
	TopologyObservation = authority.TopologyObservation
	AcceptedMembers     = members.History
)

const (
	podNodeIndex               = "spec.nodeName"
	enrolledSharesAnnotation   = members.EnrolledSharesAnnotation
	enrolledRDMANICsAnnotation = members.EnrolledRDMANICsAnnotation
	admittedMemberAnnotation   = members.AdmittedMemberAnnotation
	ReplicationAudience        = authority.ReplicationAudience
)

func (r *TopologyReconciler) observeTopology(ctx context.Context) (TopologyObservation, error) {
	cfg := r.Config

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
func readManagedWorkloadIdentities(ctx context.Context, reader client.Reader, cfg fixtureConfig) (DataplaneWorkloadIdentities, error) {
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

func podNodeKeys(obj client.Object) []string {
	pod, ok := obj.(*corev1.Pod)
	if !ok || pod.Spec.NodeName == "" {
		return nil
	}

	return []string{pod.Spec.NodeName}
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

func managedWorkloadNames(cfg fixtureConfig) []string {
	return members.ManagedNames(cfg.DaemonSetName)
}

func catalogCache(name string, uid types.UID) racerv1.ClusterCache {
	return racerv1.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid}}
}

// CanonicalSocketPaths keeps these tests on the production wire validation boundary.
