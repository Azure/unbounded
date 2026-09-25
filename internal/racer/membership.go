// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"cmp"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Azure/unbounded/internal/racer/wire"
)

// AcceptedMembers is process-local history, never a persisted checkpoint.
type AcceptedMembers map[wire.NodeID]wire.Member

type MemberAttributes struct {
	Shares           uint32
	Rails            []wire.Rail
	AlignmentEnabled bool
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

	attributes := MemberAttributes{Shares: wire.DefaultShares, Rails: []wire.Rail{}, AlignmentEnabled: true}

	if value, present := node.Annotations[wire.SharesAnnotation]; present {
		shares, err := strconv.ParseUint(value, 10, 32)
		if err != nil || shares == 0 || strings.HasPrefix(value, "+") {
			return MemberAttributes{}, fmt.Errorf("%s: %w", wire.SharesAnnotation, wire.InvalidRequest)
		}

		attributes.Shares = uint32(shares)
	}

	if value, present := node.Annotations[wire.AlignmentAnnotation]; present {
		if value != "true" && value != "false" {
			return MemberAttributes{}, fmt.Errorf("%s: %w", wire.AlignmentAnnotation, wire.InvalidRequest)
		}

		attributes.AlignmentEnabled = value == "true"
	}

	if value, present := node.Annotations[wire.RailsAnnotation]; present {
		rails, err := wire.DecodeRails(strings.NewReader(value))
		if err != nil {
			return MemberAttributes{}, fmt.Errorf("%s: %w", wire.RailsAnnotation, err)
		}

		slices.SortFunc(rails, func(a, b wire.Rail) int { return cmp.Compare(a.Rail, b.Rail) })

		for _, rail := range rails {
			if n := len(attributes.Rails); n > 0 && attributes.Rails[n-1].Rail == rail.Rail {
				previous := attributes.Rails[n-1]
				if previous.Fabric != rail.Fabric || !equalNUMA(previous.NUMANode, rail.NUMANode) {
					return MemberAttributes{}, fmt.Errorf("%s: conflicting rail mappings: %w", wire.RailsAnnotation, wire.InvalidRequest)
				}

				continue
			}

			attributes.Rails = append(attributes.Rails, rail)
		}
	}

	return attributes, nil
}

func equalNUMA(a, b *uint32) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// SelectEndpoint verifies workload ownership, ignores terminating/IP-less Pods,
// and chooses the newest creation time, breaking ties by UID. Readiness is ignored.
// An empty DaemonSet UID represents a missing workload and returns Unavailable.
// The caller supplies Pods from the installation namespace and the current
// managed DaemonSet UID; labels alone never establish ownership.
func SelectEndpoint(pods []corev1.Pod, daemonSetUID types.UID, nodeName string, port uint16) (string, error) {
	if nodeName == "" || port == 0 {
		return "", wire.InvalidRequest
	}

	if daemonSetUID == "" {
		return "", wire.Unavailable
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

		owner := metav1.GetControllerOf(pod)
		if owner == nil || owner.APIVersion != "apps/v1" || owner.Kind != "DaemonSet" || owner.UID != daemonSetUID {
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

// ReconcileMembers preserves accepted values across gaps/invalid updates only
// while this process retains them. Cold-start nodes with unavailable or malformed
// required inputs are omitted. Deletion and exclusion remove accepted history.
// Annotations are accepted as one unit, independently of the endpoint. The caller
// installs the returned history only after the candidate publication commits.
// Inputs and nested accepted state are never mutated or aliased by the result.
// Accepted must contain only previously committed results from this function.
func ReconcileMembers(nodes []corev1.Node, pods []corev1.Pod, daemonSetUID types.UID, accepted AcceptedMembers, port uint16) (AcceptedMembers, []Diagnostic, error) {
	if port == 0 {
		return nil, nil, wire.InvalidRequest
	}

	byNode := make(map[string][]corev1.Pod)
	for _, pod := range pods {
		byNode[pod.Spec.NodeName] = append(byNode[pod.Spec.NodeName], pod)
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

		attributes, annotationErr := ParseAnnotations(node)
		if annotationErr != nil {
			diagnostics = append(diagnostics, Diagnostic{Object: node.Name, Field: "annotations", Reason: annotationErr.Error()})

			if known {
				attributes = MemberAttributes{Shares: previous.Shares, Rails: previous.Rails, AlignmentEnabled: previous.AlignmentEnabled}
			}
		}

		endpoint, endpointErr := SelectEndpoint(byNode[node.Name], daemonSetUID, node.Name, port)
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

		member := wire.Member{Node: id, Shares: attributes.Shares, Rails: attributes.Rails, AlignmentEnabled: attributes.AlignmentEnabled, PeerEndpoint: endpoint}

		member.Rails = append([]wire.Rail{}, member.Rails...)
		for j := range member.Rails {
			if numa := member.Rails[j].NUMANode; numa != nil {
				value := *numa
				member.Rails[j].NUMANode = &value
			}
		}

		result[id] = member
	}

	if len(result) > wire.MaxMembers {
		return nil, nil, wire.TooLarge
	}

	return result, diagnostics, nil
}
