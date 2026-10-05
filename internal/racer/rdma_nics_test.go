// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestLegacyRDMADiagnosticsAndMalformedUnitRetention(t *testing.T) {
	node := memberNode()
	node.Annotations = map[string]string{wire.RailsAnnotation: "malformed", wire.AlignmentAnnotation: "false"}
	pods := map[string][]corev1.Pod{node.Name: {memberPod("a", 1, "192.0.2.1")}}
	accepted, diagnostics, err := reconcileMembers([]corev1.Node{node}, pods, memberOwnership(t, testDaemonSetUID), nil, 7443)
	require.NoError(t, err)
	require.Len(t, diagnostics, 2)
	require.Empty(t, accepted[testNodeUID].RDMANICs)

	for _, diagnostic := range diagnostics {
		require.Contains(t, diagnostic.Reason, "legacy annotation ignored")
	}

	node.Annotations = map[string]string{wire.SharesAnnotation: "8", enrolledRDMANICsAnnotation: `[{"device":"a","port":1,"rail":0}]`}
	accepted, _, err = reconcileMembers([]corev1.Node{node}, pods, memberOwnership(t, testDaemonSetUID), accepted, 7443)
	require.NoError(t, err)

	node.Annotations[wire.SharesAnnotation] = "9"
	node.Annotations[wire.RDMANICsAnnotation] = "null"
	retained, diagnostics, err := reconcileMembers([]corev1.Node{node}, pods, memberOwnership(t, testDaemonSetUID), accepted, 7443)
	require.NoError(t, err)
	require.Len(t, diagnostics, 1)
	require.Equal(t, accepted, retained)

	node.Annotations[wire.RDMANICsAnnotation] = "[]"
	attributes, err := ParseAnnotations(&node)
	require.NoError(t, err)
	require.Equal(t, uint32(9), attributes.Shares)
	require.Empty(t, attributes.RDMANICs)
}

func TestRDMANICAnnotationWatches(t *testing.T) {
	for _, field := range []string{wire.RDMANICsAnnotation, enrolledRDMANICsAnnotation, wire.RailsAnnotation, wire.AlignmentAnnotation} {
		t.Run(field, func(t *testing.T) {
			before := memberNode()
			after := before.DeepCopy()
			after.Annotations = map[string]string{field: "[]"}
			require.True(t, nodeChanges().Update(event.UpdateEvent{ObjectOld: &before, ObjectNew: after}))
			require.True(t, nodeChanges().Update(event.UpdateEvent{ObjectOld: after, ObjectNew: &before}))
			require.False(t, nodeChanges().Update(event.UpdateEvent{ObjectOld: after, ObjectNew: after.DeepCopy()}))
		})
	}
}
