// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	corev1 "k8s.io/api/core/v1"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/membership"
	"github.com/Azure/unbounded/internal/racer/wire"
	"github.com/Azure/unbounded/internal/racer/workload"
)

// Test-local vocabulary keeps the original membership scenarios readable without
// re-exporting the extracted packages through the production controller API.
type (
	AcceptedMembers  = membership.History
	MemberAttributes = membership.MemberAttributes
	Diagnostic       = membership.Diagnostic
)

const (
	DataplaneDaemonSetName  = workload.DataplaneDaemonSetName
	PodNetworkDaemonSetName = workload.PodNetworkDaemonSetName
)

func (ids DataplaneWorkloadIdentities) Owns(pod *corev1.Pod) bool {
	return ids.observed().Owns(pod)
}

func ParseAnnotations(node *corev1.Node) (MemberAttributes, error) {
	return membership.ParseAnnotations(node)
}

func selectEndpoint(pods []corev1.Pod, ownership DataplaneWorkloadIdentities, nodeName string, port uint16) (string, error) {
	return membership.SelectEndpoint(pods, ownership.observed(), nodeName, port)
}

func reconcileMembers(nodes []corev1.Node, podsByNode map[string][]corev1.Pod, ownership DataplaneWorkloadIdentities, accepted AcceptedMembers, port uint16) (AcceptedMembers, []Diagnostic, error) {
	result, err := membership.Reconcile(membership.Input{
		Nodes: nodes, PodsByNode: podsByNode, Ownership: ownership.observed(), PeerPort: port,
	}, accepted)

	return result.Members, result.Diagnostics, err
}

func BuildCatalog(caches []racerv1.ClusterCache) ([]wire.CacheDefinition, error) {
	return membership.BuildCatalog(caches)
}
