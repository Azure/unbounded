// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package members derives deterministic membership candidates from observed
// Kubernetes values. Discovery uses caller-provided readers; this package owns
// no clients, publication state, workload builders, or annotation writes.
package members

import (
	"cmp"
	"context"
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
	"sigs.k8s.io/controller-runtime/pkg/client"

	machinav1 "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/wire"
)

// ReadWorkloadIdentities snapshots current DaemonSet ownership for one discovery
// or authorization pass. Missing and terminating workloads grant no ownership.
func ReadWorkloadIdentities(ctx context.Context, reader client.Reader, namespace, daemonSetName string) (WorkloadIdentities, error) {
	ids := WorkloadIdentities{Namespace: namespace, Name: daemonSetName}

	var ds appsv1.DaemonSet
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: daemonSetName}, &ds); err != nil {
		if apierrors.IsNotFound(err) {
			return ids, nil
		}

		return WorkloadIdentities{}, err
	}

	if ds.DeletionTimestamp == nil {
		ids.UID = ds.UID
	}

	return ids, nil
}

// ControllerPod checks the live namespace and service-account identity shared by
// replication discovery and authorization. Readiness is not an identity signal.
func ControllerPod(pod *corev1.Pod, namespace, serviceAccount string) bool {
	return pod.Namespace == namespace && pod.UID != "" && pod.DeletionTimestamp == nil && pod.Spec.ServiceAccountName == serviceAccount && pod.Status.Phase != corev1.PodFailed && pod.Status.Phase != corev1.PodSucceeded
}

// Observed workload ownership and endpoint selection.

// WorkloadIdentities is the configured live DaemonSet identity, not a label selector.
// Owns performs no I/O; callers refresh the snapshot with ReadWorkloadIdentities
// on every topology or authorization pass so ownership never relies on stale UIDs.
type WorkloadIdentities struct {
	Namespace string
	Name      string
	UID       types.UID
}

// Owns checks ownership only. Callers retain their Pod, Node, service-account,
// token and readiness-independent membership checks.
func (ids WorkloadIdentities) Owns(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}

	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.APIVersion != "apps/v1" || owner.Kind != "DaemonSet" || owner.UID == "" {
		return false
	}

	if pod.Namespace != ids.Namespace {
		return false
	}

	return owner.Name == ids.Name && owner.UID == ids.UID
}

// SelectEndpoint verifies workload ownership, ignores terminal/terminating/IP-less
// Pods, and chooses the newest creation time, breaking ties by UID. Readiness is ignored.
func SelectEndpoint(pods []corev1.Pod, ownership WorkloadIdentities, nodeName string, port uint16) (string, error) {
	if nodeName == "" || port == 0 {
		return "", wire.InvalidRequest
	}

	var (
		selected *corev1.Pod
		address  netip.Addr
	)

	for i := range pods {
		pod := &pods[i]

		ip, eligible := endpointAddress(pod, ownership, nodeName)
		if !eligible {
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

func endpointAddress(pod *corev1.Pod, ownership WorkloadIdentities, nodeName string) (netip.Addr, bool) {
	if pod.Spec.NodeName != nodeName || pod.DeletionTimestamp != nil || pod.UID == "" {
		return netip.Addr{}, false
	}

	if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded || !ownership.Owns(pod) {
		return netip.Addr{}, false
	}

	ip, err := netip.ParseAddr(pod.Status.PodIP)

	return ip, err == nil && ip.Zone() == ""
}

// Membership candidates and recovery.

const (
	EnrolledSharesAnnotation   = "racer.unbounded-cloud.io/enrolled-shares"
	EnrolledRDMANICsAnnotation = "racer.unbounded-cloud.io/enrolled-rdma-nics"
	AdmittedMemberAnnotation   = "racer.unbounded-cloud.io/last-admitted-member"
)

// History contains only previously published members. Node annotations provide
// UID-bound recovery hints when an entry is absent. Callers must not advance this
// history until publication succeeds.
type History map[wire.NodeID]wire.Member

// Input is one observed topology snapshot. Pods are grouped by assigned Node
// name; each group is still checked for assignment and workload ownership.
// Reconcile borrows these values without mutating or retaining them.
type Input struct {
	Nodes      []corev1.Node
	PodsByNode map[string][]corev1.Pod
	Ownership  WorkloadIdentities
	PeerPort   uint16
}

// Result owns its members (including nested NICs) and diagnostics. Members is a
// candidate, not accepted state, until the caller successfully publishes it.
type Result struct {
	Members     History
	Diagnostics []Diagnostic
}

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
		if value := node.Annotations[EnrolledSharesAnnotation]; value != "" {
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
		field = EnrolledRDMANICsAnnotation
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

// Reconcile preserves admitted values across gaps using UID-bound Node
// annotations on restart. Never-admitted nodes with unavailable or malformed
// required inputs are omitted. Deletion and exclusion remove membership.
// Annotations are accepted as one unit, independently of the endpoint. Site is
// always derived from current labels, never from admitted history, so a
// malformed annotation cannot retain a removed or changed RDMA boundary.
// The caller installs returned history only after the candidate publication commits.
// Inputs and nested accepted state are never mutated or aliased by the result.
func Reconcile(input Input, accepted History) (Result, error) {
	if input.PeerPort == 0 {
		return Result{}, wire.InvalidRequest
	}

	nodes := slices.Clone(input.Nodes)
	slices.SortFunc(nodes, func(a, b corev1.Node) int { return cmp.Compare(a.UID, b.UID) })

	result := Result{Members: make(History), Diagnostics: []Diagnostic{}}
	ids, names := map[types.UID]bool{}, map[string]bool{}

	for i := range nodes {
		node := &nodes[i]
		if !wire.ValidUUID(string(node.UID)) || node.Name == "" || ids[node.UID] || names[node.Name] {
			return Result{}, fmt.Errorf("node identity: %w", wire.InvalidRequest)
		}

		ids[node.UID], names[node.Name] = true, true
		if _, excluded := node.Labels[wire.ExclusionLabel]; excluded {
			continue
		}

		result.reconcileNode(node, input, accepted)
	}

	if len(result.Members) > wire.MaxMembers {
		return Result{}, wire.TooLarge
	}

	return result, nil
}

func (result *Result) reconcileNode(node *corev1.Node, input Input, accepted History) {
	id := wire.NodeID(node.UID)

	previous, known := accepted[id]
	if !known {
		if saved, err := wire.DecodeAdmittedMember(strings.NewReader(node.Annotations[AdmittedMemberAnnotation])); err == nil && saved.Node == id {
			previous, known = saved, true
		}
	}

	attributes, annotationErr := ParseAnnotations(node)
	if annotationErr != nil {
		result.Diagnostics = append(result.Diagnostics, Diagnostic{Object: node.Name, Field: "annotations", Reason: annotationErr.Error()})

		if known {
			attributes = MemberAttributes{Shares: previous.Shares, RDMANICs: previous.RDMANICs}
		}
	}
	// Node identity and peer port were validated by Reconcile, so the only
	// possible endpoint error here is Unavailable.
	endpoint, endpointErr := SelectEndpoint(input.PodsByNode[node.Name], input.Ownership, node.Name, input.PeerPort)
	if endpointErr != nil {
		result.Diagnostics = append(result.Diagnostics, Diagnostic{Object: node.Name, Field: "peer_endpoint", Reason: "no eligible managed Pod endpoint"})

		if known {
			endpoint = previous.PeerEndpoint
		}
	}

	if !known && (annotationErr != nil || endpointErr != nil) {
		return
	}

	result.Members[id] = wire.Member{
		Node: id, Shares: attributes.Shares, RDMANICs: wire.CanonicalRDMANICs(attributes.RDMANICs), PeerEndpoint: endpoint,
		// Current labels revoke a stale RDMA boundary even if annotations are invalid.
		Site: node.Labels[machinav1.MachineSiteLabelKey],
	}
}

// BuildCatalog selects Cache volumes, deriving identities from UIDs and paths from names, sorted by UID.
// An invalid catalog never partially replaces the currently served publication.
func BuildCatalog(volumes []racerv1.ClusterVolume) ([]wire.CacheDefinition, error) {
	catalog := make([]wire.CacheDefinition, 0, len(volumes))
	ids := make(map[wire.CacheID]bool, len(volumes))

	names := make(map[string]bool, len(volumes))
	for _, volume := range volumes {
		if volume.Spec.Type != racerv1.ClusterVolumeTypeCache {
			continue
		}

		id := wire.CacheID(volume.UID)
		if !wire.ValidUUID(string(id)) || ids[id] || names[volume.Name] {
			return nil, fmt.Errorf("cache identity: %w", wire.InvalidRequest)
		}

		client, origin, err := wire.CanonicalSocketPaths(volume.Name)
		if err != nil {
			return nil, fmt.Errorf("cache socket paths: %w", err)
		}

		ids[id], names[volume.Name] = true, true
		catalog = append(catalog, wire.CacheDefinition{ID: id, Name: volume.Name, ClientSocket: client, OriginSocket: origin})
	}

	slices.SortFunc(catalog, func(a, b wire.CacheDefinition) int { return cmp.Compare(a.ID, b.ID) })

	return catalog, nil
}
