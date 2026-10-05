// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package membership derives deterministic membership candidates from observed
// Kubernetes values. It owns no clients, publication state, or annotation writes.
package membership

import (
	"cmp"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	machinav1 "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/wire"
)

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

// nodeSite uses only the canonical Machine Site label.
// Read current labels independently of retained annotations: removal must revoke
// the old RDMA boundary even when hardware annotations are malformed.
func nodeSite(node *corev1.Node) string {
	return node.Labels[machinav1.MachineSiteLabelKey]
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

	result := make(History)
	diagnostics := []Diagnostic{}
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

		id := wire.NodeID(node.UID)

		previous, known := accepted[id]
		if !known {
			if saved, err := wire.DecodeAdmittedMember(strings.NewReader(node.Annotations[AdmittedMemberAnnotation])); err == nil && saved.Node == id {
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

		endpoint, endpointErr := SelectEndpoint(input.PodsByNode[node.Name], input.Ownership, node.Name, input.PeerPort)
		if endpointErr != nil {
			if !errors.Is(endpointErr, wire.Unavailable) {
				return Result{}, endpointErr
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
		return Result{}, wire.TooLarge
	}

	return Result{Members: result, Diagnostics: diagnostics}, nil
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
