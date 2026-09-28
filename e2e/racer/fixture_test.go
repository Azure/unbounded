//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestPeerOutageUsesDeployedPort(t *testing.T) {
	for _, port := range []int32{8082, 7443, 19090, 65535} {
		pod := corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "sidecar", Ports: []corev1.ContainerPort{{Name: "peer", ContainerPort: 1111, Protocol: corev1.ProtocolTCP}}},
			{Name: "dataplane", Ports: []corev1.ContainerPort{
				{Name: "diagnostics", ContainerPort: 9090, Protocol: corev1.ProtocolTCP},
				{Name: "peer", ContainerPort: port, Protocol: corev1.ProtocolTCP},
			}},
		}}}
		actual, err := peerPort(pod)
		require.NoError(t, err)
		require.Equal(t, port, actual)
		rule := peerOutageRule(peerNode{ip: "10.244.2.4", port: actual})
		require.Equal(t, "OUTPUT", rule[0])
		require.Equal(t, "10.244.2.4", rule[2])

		var target int32

		_, err = fmt.Sscan(rule[6], &target)
		require.NoError(t, err)
		require.Equal(t, port, target)
	}
}

func TestPeerPortRejectsMissingOrInvalidEndpoint(t *testing.T) {
	for _, port := range []corev1.ContainerPort{
		{Name: "diagnostics", ContainerPort: 9090, Protocol: corev1.ProtocolTCP},
		{Name: "peer", ContainerPort: 8082, Protocol: corev1.ProtocolUDP},
		{Name: "peer", ContainerPort: 0, Protocol: corev1.ProtocolTCP},
		{Name: "peer", ContainerPort: 65536, Protocol: corev1.ProtocolTCP},
	} {
		_, err := peerPort(corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dataplane", Ports: []corev1.ContainerPort{port}}}}})
		require.Error(t, err)
	}

	_, err := peerPort(corev1.Pod{})
	require.Error(t, err)
}

func TestFixturePhaseCleanupAfterGoexit(t *testing.T) {
	h := &harness{t: t, sequence: 8}
	cleaned := false

	h.runPhase("early exit", func(child *harness) {
		child.sequence++
		child.t.Cleanup(func() {
			cleaned = true
			child.sequence++
		})
		// SkipNow, like FailNow, exits via runtime.Goexit rather than returning.
		child.t.SkipNow()
	})
	require.True(t, cleaned)
	require.Equal(t, 10, h.sequence)
	h.runPhase("next phase", func(child *harness) {
		require.True(child.t, cleaned)
		require.Equal(child.t, 10, child.sequence)
		child.sequence++
	})
	require.Equal(t, 11, h.sequence)
}
