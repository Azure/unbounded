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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Azure/unbounded/internal/provision"
)

func TestTargetCommitGateRejectsUnjoinedAndStaleTargets(t *testing.T) {
	t.Parallel()

	for _, failure := range []string{"expired-token", "api-unreachable", "source-boot", "stale-ready", "not-ready", "target-restarted", "success"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()

			cfg := baseConfig()
			cfg.NodeName = "worker"
			s := &repaveState{Phase: "verifying", Target: "kube2", TargetConfig: provision.UnboundedAgentConfig{AgentConfig: *cfg}}

			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: cfg.NodeName, UID: "replacement"}, Status: corev1.NodeStatus{
				NodeInfo: corev1.NodeSystemInfo{BootID: "target-boot"}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue, LastHeartbeatTime: metav1.Now()}},
			}}
			if failure == "source-boot" {
				node.Status.NodeInfo.BootID = "source-boot"
			}

			if failure == "stale-ready" {
				node.Status.Conditions[0].LastHeartbeatTime = metav1.NewTime(time.Now().Add(-time.Hour))
			}

			if failure == "not-ready" {
				node.Status.Conditions[0].Status = corev1.ConditionFalse
			}

			data, err := json.Marshal(node)
			require.NoError(t, err)

			boots := 0

			err = verifyRepaveTargetWithRunner(t.Context(), fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(node).Build(), s, func(_ context.Context, machine string, args ...string) (string, error) {
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
			if failure == "success" {
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
