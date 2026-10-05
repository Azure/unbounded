// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	machinav1 "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/wire"
)

type TopologyReconciler struct {
	settings frozenConfig
	client.Client
	APIReader    client.Reader
	Config       Config
	Publications *Publications
	Accepted     AcceptedMembers
	CatalogGate  *CatalogGate
	Trust        *Trust
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

type topologyUpdate struct {
	nodes   corev1.NodeList
	members AcceptedMembers
}

// publish protects authoritative reads, CAS, and local installation. Annotation
// writes are recovery hints, not authority, and must not block trust observation.
func (r *TopologyReconciler) publish(ctx context.Context) (topologyUpdate, error) {
	cfg := r.runtimeConfig()
	if r.CatalogGate != nil {
		if err := r.CatalogGate.Acquire(ctx); err != nil {
			return topologyUpdate{}, err
		}
		defer r.CatalogGate.Release()
	}

	if err := ctx.Err(); err != nil {
		return topologyUpdate{}, err
	}

	cm, previous, err := readVersion(ctx, r.APIReader, cfg)
	if err != nil {
		r.suspendInvalidAuthority(err)
		return topologyUpdate{}, err
	}

	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return topologyUpdate{}, err
	}

	var caches racerv1.ClusterCacheList
	if err := r.APIReader.List(ctx, &caches); err != nil {
		return topologyUpdate{}, err
	}

	catalog, err := BuildCatalog(caches.Items)
	if err != nil {
		return topologyUpdate{}, err
	}

	// The committed keyring is the admission authority. A cache event can arrive
	// before its keys exist; only the subsequent Secret event may publish it.
	// Read authoritatively so a stale informer cannot admit rejected growth.
	if claim := cm.Annotations[credentialClaim]; claim != "" {
		credentials, err := readBoundCredentials(ctx, r.APIReader, cfg, claim, cm)
		if err != nil {
			r.suspendInvalidAuthority(err)
			return topologyUpdate{}, err
		}

		keyed := keyedCaches(credentials.bundle)

		accepted := catalog[:0]
		for _, cache := range catalog {
			if keyed[cache.ID] {
				accepted = append(accepted, cache)
			}
		}

		catalog = accepted
	} else {
		catalog = nil
	}

	ownership, err := readManagedWorkloadIdentities(ctx, r.APIReader, cfg)
	if err != nil {
		return topologyUpdate{}, err
	}
	// Indexed namespace-scoped queries avoid scanning unrelated Pods for each
	// Node. Ownership is still verified against the current DaemonSet UID.
	podsByNode := make(map[string][]corev1.Pod, len(nodes.Items))

	for _, node := range nodes.Items {
		if err := ctx.Err(); err != nil {
			return topologyUpdate{}, err
		}

		var list corev1.PodList
		if err := r.List(ctx, &list, client.InNamespace(cfg.Namespace), client.MatchingFields{podNodeIndex: node.Name}); err != nil {
			return topologyUpdate{}, err
		}

		podsByNode[node.Name] = list.Items
	}

	candidate, diagnostics, err := reconcileMembers(nodes.Items, podsByNode, ownership, r.Accepted, cfg.PeerPort)
	if err != nil {
		return topologyUpdate{}, err
	}

	for _, d := range diagnostics {
		ctrl.LoggerFrom(ctx).Info("membership input rejected", "object", d.Object, "field", d.Field, "reason", d.Reason)
	}

	prepared, err := r.Publications.Prepare(previous, cm.ResourceVersion, candidate, catalog)
	if err != nil {
		return topologyUpdate{}, err
	}

	committed, err := r.CommitVersion(ctx, prepared)
	if err != nil {
		return topologyUpdate{}, err
	}

	if err := ctx.Err(); err != nil {
		return topologyUpdate{}, err
	}

	if err := r.Publications.Install(committed); err != nil {
		return topologyUpdate{}, err
	}

	r.Accepted = candidate

	return topologyUpdate{nodes: nodes, members: candidate}, nil
}

func (r *TopologyReconciler) annotate(ctx context.Context, update topologyUpdate) error {
	for i := range update.nodes.Items {
		node := &update.nodes.Items[i]

		member, ok := update.members[wire.NodeID(node.UID)]
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

func (r *TopologyReconciler) suspendInvalidAuthority(err error) {
	if shouldInvalidateTrust(err) {
		r.Publications.Suspend()
		r.Trust.invalidate()
	}
}

// singleton coalesces input changes without introducing a singleton CR.
func singleton(_ context.Context, _ client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "racer"}}}
}

const (
	DataplaneDaemonSetName  = "racer-dataplane"
	PodNetworkDaemonSetName = "racer-dataplane-podnet"
)

func managedWorkloadNames(cfg Config) []string {
	if cfg.DaemonSetName == DataplaneDaemonSetName {
		return []string{DataplaneDaemonSetName, PodNetworkDaemonSetName}
	}

	return []string{cfg.DaemonSetName}
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

// Owns checks ownership only. Callers retain their Pod, Node, service-account,
// token and readiness-independent membership checks.
func (ids DataplaneWorkloadIdentities) Owns(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}

	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.APIVersion != "apps/v1" || owner.Kind != "DaemonSet" || owner.UID == "" {
		return false
	}

	if pod.Namespace != ids.namespace {
		return false
	}

	for _, workload := range ids.workloads {
		if owner.Name == workload.name && owner.UID == workload.uid {
			return true
		}
	}

	return false
}

const (
	enrolledSharesAnnotation   = "racer.unbounded-cloud.io/enrolled-shares"
	enrolledRDMANICsAnnotation = "racer.unbounded-cloud.io/enrolled-rdma-nics"
	admittedMemberAnnotation   = "racer.unbounded-cloud.io/last-admitted-member"
)

// nodeSite uses only the canonical Machine Site label.
// Read current labels independently of retained annotations: removal must revoke
// the old RDMA boundary even when hardware annotations are malformed.
func nodeSite(node *corev1.Node) string {
	return node.Labels[machinav1.MachineSiteLabelKey]
}

// AcceptedMembers is backed by per-Node UID-bound last-admitted annotations.
type AcceptedMembers map[wire.NodeID]wire.Member

type MemberAttributes struct {
	Shares   uint32
	RDMANICs []wire.RDMANIC
}

type Diagnostic struct {
	Object string
	Field  string
	Reason string
}

// ParseAnnotations distinguishes absent defaults from malformed proposed updates.
func ParseAnnotations(node *corev1.Node) (MemberAttributes, error) {
	if node == nil {
		return MemberAttributes{}, wire.InvalidRequest
	}

	attributes := MemberAttributes{Shares: wire.DefaultShares, RDMANICs: []wire.RDMANIC{}}

	if _, explicit := node.Annotations[wire.SharesAnnotation]; !explicit {
		if value := node.Annotations[enrolledSharesAnnotation]; value != "" {
			shares, err := strconv.ParseUint(value, 10, 32)
			if err != nil || shares == 0 {
				return MemberAttributes{}, wire.InvalidRequest
			}

			attributes.Shares = uint32(shares)
		}
	}

	if value, present := node.Annotations[wire.SharesAnnotation]; present {
		shares, err := strconv.ParseUint(value, 10, 32)
		if err != nil || shares == 0 || strings.HasPrefix(value, "+") {
			return MemberAttributes{}, fmt.Errorf("%s: %w", wire.SharesAnnotation, wire.InvalidRequest)
		}

		attributes.Shares = uint32(shares)
	}

	field := wire.RDMANICsAnnotation

	value, present := node.Annotations[field]
	if !present {
		field = enrolledRDMANICsAnnotation
		value, present = node.Annotations[field]
	}

	if present {
		nics, err := wire.DecodeRDMANICs(strings.NewReader(value))
		if err != nil {
			return MemberAttributes{}, fmt.Errorf("%s: %w", field, err)
		}

		attributes.RDMANICs = nics
	}

	return attributes, nil
}

// selectEndpoint verifies workload ownership, ignores terminal/terminating/IP-less Pods,
// and chooses the newest creation time, breaking ties by UID. Readiness is ignored.
func selectEndpoint(pods []corev1.Pod, ownership DataplaneWorkloadIdentities, nodeName string, port uint16) (string, error) {
	if nodeName == "" || port == 0 {
		return "", wire.InvalidRequest
	}

	var (
		selected *corev1.Pod
		address  netip.Addr
	)

	for i := range pods {
		pod := &pods[i]
		if pod.Spec.NodeName != nodeName || pod.DeletionTimestamp != nil || pod.UID == "" {
			continue
		}

		if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			continue
		}

		if !ownership.Owns(pod) {
			continue
		}

		ip, err := netip.ParseAddr(pod.Status.PodIP)
		if err != nil || ip.Zone() != "" {
			continue
		}

		if selected == nil || pod.CreationTimestamp.After(selected.CreationTimestamp.Time) ||
			pod.CreationTimestamp.Equal(&selected.CreationTimestamp) && pod.UID > selected.UID {
			selected, address = pod, ip
		}
	}

	if selected == nil {
		return "", wire.Unavailable
	}

	return netip.AddrPortFrom(address, port).String(), nil
}

// reconcileMembers preserves admitted values across gaps using UID-bound Node
// annotations on restart. Never-admitted nodes with unavailable or malformed
// required inputs are omitted. Deletion and exclusion remove membership.
// Annotations are accepted as one unit, independently of the endpoint. Site is
// always derived from current labels, never from admitted history, so a
// malformed annotation cannot retain a removed or changed RDMA boundary.
// The caller installs returned history only after the candidate publication commits.
// Inputs and nested accepted state are never mutated or aliased by the result.
// Accepted must contain only previously committed results from this function.
// The caller supplies installation-namespace Pods grouped by assigned node name.
// Each group is still checked for node assignment and DaemonSet ownership.
func reconcileMembers(nodes []corev1.Node, podsByNode map[string][]corev1.Pod, ownership DataplaneWorkloadIdentities, accepted AcceptedMembers, port uint16) (AcceptedMembers, []Diagnostic, error) {
	if port == 0 {
		return nil, nil, wire.InvalidRequest
	}

	nodes = slices.Clone(nodes)
	slices.SortFunc(nodes, func(a, b corev1.Node) int { return cmp.Compare(a.UID, b.UID) })

	result := make(AcceptedMembers)
	diagnostics := []Diagnostic{}
	ids, names := map[types.UID]bool{}, map[string]bool{}

	for i := range nodes {
		node := &nodes[i]
		if !wire.ValidUUID(string(node.UID)) || node.Name == "" || ids[node.UID] || names[node.Name] {
			return nil, nil, fmt.Errorf("node identity: %w", wire.InvalidRequest)
		}

		ids[node.UID], names[node.Name] = true, true
		if _, excluded := node.Labels[wire.ExclusionLabel]; excluded {
			continue
		}

		id := wire.NodeID(node.UID)

		previous, known := accepted[id]
		if !known {
			if saved, err := wire.DecodeAdmittedMember(strings.NewReader(node.Annotations[admittedMemberAnnotation])); err == nil && saved.Node == id {
				previous, known = saved, true
			}
		}

		for _, field := range []string{wire.RailsAnnotation, wire.AlignmentAnnotation} {
			if _, present := node.Annotations[field]; present {
				diagnostics = append(diagnostics, Diagnostic{Object: node.Name, Field: field, Reason: "legacy annotation ignored; use " + wire.RDMANICsAnnotation})
			}
		}

		attributes, annotationErr := ParseAnnotations(node)
		if annotationErr != nil {
			diagnostics = append(diagnostics, Diagnostic{Object: node.Name, Field: "annotations", Reason: annotationErr.Error()})

			if known {
				attributes = MemberAttributes{Shares: previous.Shares, RDMANICs: previous.RDMANICs}
			}
		}

		endpoint, endpointErr := selectEndpoint(podsByNode[node.Name], ownership, node.Name, port)
		if endpointErr != nil {
			if !errors.Is(endpointErr, wire.Unavailable) {
				return nil, nil, endpointErr
			}

			diagnostics = append(diagnostics, Diagnostic{Object: node.Name, Field: "peer_endpoint", Reason: "no eligible managed Pod endpoint"})

			if known {
				endpoint = previous.PeerEndpoint
			}
		}

		if !known && (annotationErr != nil || endpointErr != nil) {
			continue
		}

		member := wire.Member{Node: id, Shares: attributes.Shares, RDMANICs: wire.CanonicalRDMANICs(attributes.RDMANICs), PeerEndpoint: endpoint, Site: nodeSite(node)}

		result[id] = member
	}

	if len(result) > wire.MaxMembers {
		return nil, nil, wire.TooLarge
	}

	return result, diagnostics, nil
}

// BuildCatalog derives identities from UIDs and paths from names, sorted by UID.
// An invalid catalog never partially replaces the currently served publication.
func BuildCatalog(caches []racerv1.ClusterCache) ([]wire.CacheDefinition, error) {
	catalog := make([]wire.CacheDefinition, 0, len(caches))
	ids := make(map[wire.CacheID]bool, len(caches))

	names := make(map[string]bool, len(caches))
	for _, cache := range caches {
		id := wire.CacheID(cache.UID)
		if !wire.ValidUUID(string(id)) || ids[id] || names[cache.Name] {
			return nil, fmt.Errorf("cache identity: %w", wire.InvalidRequest)
		}

		client, origin, err := wire.CanonicalSocketPaths(cache.Name)
		if err != nil {
			return nil, fmt.Errorf("cache socket paths: %w", err)
		}

		ids[id], names[cache.Name] = true, true
		catalog = append(catalog, wire.CacheDefinition{
			ID: id, Name: cache.Name, ClientSocket: client, OriginSocket: origin,
		})
	}

	slices.SortFunc(catalog, func(a, b wire.CacheDefinition) int { return cmp.Compare(a.ID, b.ID) })

	return catalog, nil
}
