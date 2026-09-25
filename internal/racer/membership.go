// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	corev1 "k8s.io/api/core/v1"
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
func ParseAnnotations(_ *corev1.Node) (MemberAttributes, error) {
	return MemberAttributes{}, pending("membership.parse_annotations")
}

// SelectEndpoint verifies workload ownership, ignores terminating/IP-less Pods,
// and chooses the newest creation time, breaking ties by UID. Readiness is ignored.
func SelectEndpoint(_ []corev1.Pod, _ types.UID, _ string, _ uint16) (string, error) {
	return "", pending("membership.select_endpoint")
}

// ReconcileMembers preserves accepted values across gaps/invalid updates only
// while this process retains them. Cold-start nodes with unavailable or malformed
// required inputs are omitted. Deletion and exclusion remove accepted history.
func ReconcileMembers(_ []corev1.Node, _ []corev1.Pod, _ types.UID, _ AcceptedMembers, _ uint16) (AcceptedMembers, []Diagnostic, error) {
	return nil, nil, pending("membership.reconcile")
}
