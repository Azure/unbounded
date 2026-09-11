// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Azure/unbounded/internal/provision"
)

func TestTargetCommitGateRejectsUnjoinedAndStaleTargets(t *testing.T) {
	t.Parallel()

	for _, failure := range []string{"expired-token", "api-unreachable", "source-boot", "stale-ready", "not-ready", "target-restarted", "missing-lease", "wrong-lease-owner", "slow-status-report", "success"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()

			cfg := baseConfig()
			cfg.NodeName = "worker"
			s := &repaveState{Phase: "verifying", Target: "kube2", TargetConfig: provision.UnboundedAgentConfig{AgentConfig: *cfg}}

			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: cfg.NodeName, UID: "replacement"}, Status: corev1.NodeStatus{
				NodeInfo: corev1.NodeSystemInfo{BootID: "target-boot"}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue, LastHeartbeatTime: metav1.Now()}},
			}}

			lease := &coordinationv1.Lease{
				ObjectMeta: metav1.ObjectMeta{
					Name: cfg.NodeName, Namespace: corev1.NamespaceNodeLease,
					OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Node", Name: cfg.NodeName, UID: node.UID}},
				},
				Spec: coordinationv1.LeaseSpec{HolderIdentity: ptr.To(cfg.NodeName), LeaseDurationSeconds: ptr.To(int32(40)), RenewTime: ptr.To(metav1.NewMicroTime(time.Now()))},
			}
			if failure == "source-boot" {
				node.Status.NodeInfo.BootID = "source-boot"
			}

			if failure == "stale-ready" {
				node.Status.Conditions[0].LastHeartbeatTime = metav1.NewTime(time.Now().Add(-time.Hour))
				lease.Spec.RenewTime = ptr.To(metav1.NewMicroTime(time.Now().Add(-time.Hour)))
			}

			if failure == "slow-status-report" {
				cfg.Kubelet.Configuration = map[string]any{"nodeStatusReportFrequency": "10m"}
				node.Status.Conditions[0].LastHeartbeatTime = metav1.NewTime(time.Now().Add(-5 * time.Minute))
			}

			if failure == "wrong-lease-owner" {
				lease.OwnerReferences[0].UID = "retired-source"
			}

			if failure == "not-ready" {
				node.Status.Conditions[0].Status = corev1.ConditionFalse
			}

			data, err := json.Marshal(node)
			require.NoError(t, err)

			boots := 0

			objects := []client.Object{node}
			if failure != "missing-lease" {
				objects = append(objects, lease)
			}

			err = verifyRepaveTargetWithRunner(t.Context(), fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(objects...).Build(), s, func(_ context.Context, machine string, args ...string) (string, error) {
				require.Equal(t, "kube2", machine)

				if args[0] == "cat" {
					boots++
					if boots > 1 && failure == "target-restarted" {
						return "new-boot", nil
					}

					return "target-boot", nil
				}

				require.Contains(t, args[1], "kubeconfig")

				if failure == "expired-token" || failure == "api-unreachable" {
					return "", errors.New(failure)
				}

				return string(data), nil
			})
			if failure == "success" || failure == "slow-status-report" {
				require.NoError(t, err)
				require.Equal(t, "committed", s.Phase)
				require.Equal(t, "replacement", s.TargetNodeUID)
			} else {
				require.Error(t, err)
				require.Equal(t, "verifying", s.Phase)
				require.Nil(t, s.VerifiedAt)
			}
		})
	}
}

func TestTargetLeaseFreshness(t *testing.T) {
	t.Parallel()

	now := time.Now()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "target"}}

	for _, tc := range []struct {
		name   string
		mutate func(*coordinationv1.Lease)
	}{
		{"expired", func(l *coordinationv1.Lease) {
			l.Spec.RenewTime = ptr.To(metav1.NewMicroTime(now.Add(-40 * time.Second)))
		}},
		{"future", func(l *coordinationv1.Lease) { l.Spec.RenewTime = ptr.To(metav1.NewMicroTime(now.Add(time.Minute))) }},
		{"no-renewal", func(l *coordinationv1.Lease) { l.Spec.RenewTime = nil }},
		{"no-duration", func(l *coordinationv1.Lease) { l.Spec.LeaseDurationSeconds = nil }},
		{"zero-duration", func(l *coordinationv1.Lease) { l.Spec.LeaseDurationSeconds = ptr.To(int32(0)) }},
		{"wrong-holder", func(l *coordinationv1.Lease) { l.Spec.HolderIdentity = ptr.To("source") }},
		{"no-owner", func(l *coordinationv1.Lease) { l.OwnerReferences = nil }},
		{"old-node", func(l *coordinationv1.Lease) { l.OwnerReferences[0].UID = "source" }},
		{"wrong-namespace", func(l *coordinationv1.Lease) { l.Namespace = "default" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			lease := &coordinationv1.Lease{
				ObjectMeta: metav1.ObjectMeta{
					Name: node.Name, Namespace: corev1.NamespaceNodeLease,
					OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Node", Name: node.Name, UID: node.UID}},
				},
				Spec: coordinationv1.LeaseSpec{HolderIdentity: ptr.To(node.Name), RenewTime: ptr.To(metav1.NewMicroTime(now)), LeaseDurationSeconds: ptr.To(int32(40))},
			}
			require.NoError(t, targetLeaseFresh(lease, node, now))
			tc.mutate(lease)
			require.Error(t, targetLeaseFresh(lease, node, now))
		})
	}
}
