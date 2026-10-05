// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	corev1 "k8s.io/api/core/v1"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/members"
	"github.com/Azure/unbounded/internal/racer/wire"
)

// Test-local vocabulary keeps the original membership scenarios readable without
// re-exporting the extracted packages through the production controller API.
type (
	AcceptedMembers  = members.History
	MemberAttributes = members.MemberAttributes
	Diagnostic       = members.Diagnostic
)

const (
	DataplaneDaemonSetName  = members.DataplaneDaemonSetName
	PodNetworkDaemonSetName = members.PodNetworkDaemonSetName
)

func (ids DataplaneWorkloadIdentities) Owns(pod *corev1.Pod) bool {
	return ids.observed().Owns(pod)
}

func ParseAnnotations(node *corev1.Node) (MemberAttributes, error) {
	return members.ParseAnnotations(node)
}

func selectEndpoint(pods []corev1.Pod, ownership DataplaneWorkloadIdentities, nodeName string, port uint16) (string, error) {
	return members.SelectEndpoint(pods, ownership.observed(), nodeName, port)
}

func reconcileMembers(nodes []corev1.Node, podsByNode map[string][]corev1.Pod, ownership DataplaneWorkloadIdentities, accepted AcceptedMembers, port uint16) (AcceptedMembers, []Diagnostic, error) {
	result, err := members.Reconcile(members.Input{
		Nodes: nodes, PodsByNode: podsByNode, Ownership: ownership.observed(), PeerPort: port,
	}, accepted)

	return result.Members, result.Diagnostics, err
}

func BuildCatalog(caches []racerv1.ClusterCache) ([]wire.CacheDefinition, error) {
	return members.BuildCatalog(caches)
}
