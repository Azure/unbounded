// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/provision"
	agentdaemon "github.com/Azure/unbounded/pkg/agent/daemon"
	"github.com/Azure/unbounded/pkg/agent/installstate"
)

func TestRecoveryActionsBindIntentAndPhase(t *testing.T) {
	originalDir := installstate.Dir
	installstate.Dir = t.TempDir()
	t.Cleanup(func() { installstate.Dir = originalDir })

	original := installstate.LockPathForTest
	installstate.LockPathForTest = filepath.Join(t.TempDir(), "install.lock")
	t.Cleanup(func() { installstate.LockPathForTest = original })

	for _, tc := range []struct{ action, phase, id, want string }{
		{"retry", "verifying", "transition", "verifying"},
		{"cancel", "preparing", "transition", "canceling"},
		{"reselect", "preparing", "transition", "canceling"},
		{"cancel", "starting", "transition", ""},
		{"reselect", "switching", "transition", ""},
		{"retry", "preparing", "stale", ""},
	} {
		t.Run(tc.action+"/"+tc.phase+"/"+tc.id, func(t *testing.T) {
			cfg := baseConfig()
			state := &repaveState{Version: 2, TransitionID: "transition", Phase: tc.phase, Source: "kube1", Target: "kube2", SourceConfig: *cfg, TargetConfig: provision.UnboundedAgentConfig{AgentConfig: *cfg}}
			store := repaveStore{dir: t.TempDir()}
			require.NoError(t, store.Save(state))

			machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: cfg.MachineName}, Spec: v1alpha3.MachineSpec{ConfigurationRef: &v1alpha3.MachineConfigurationRef{Name: "new", Version: ptr.To(int32(2))}}}
			mcv := machineConfigurationVersion("new", 2, v1alpha3.MachineConfigurationTemplate{Kubernetes: &v1alpha3.MachineConfigurationKubernetes{Version: "v1.34.1"}})
			op := &v1alpha3.MachineOperation{ObjectMeta: metav1.ObjectMeta{Name: "recover"}, Spec: v1alpha3.MachineOperationSpec{MachineRef: cfg.MachineName, OperationKind: v1alpha3.OperationRepaveRecovery, Parameters: map[string]string{"action": tc.action, "transitionID": tc.id}}}
			c := fakeStatusClient(machine, mcv, op)
			worker := &repaveWorker{store: store, client: c, reader: c, log: discardLogger(), wake: make(chan struct{}, 1)}
			target := &machineOperationTarget{Client: c, machineName: cfg.MachineName, log: discardLogger(), worker: worker}
			r, err := agentdaemon.NewMachinaMachineOperationReconciler(c, cfg.MachineName, "node", agentdaemon.MachineOperationHandlers{v1alpha3.OperationRepaveRecovery: target.reconcileRepaveRecovery})
			require.NoError(t, err)
			_, err = r.ReconcileMachineOperation(t.Context(), op.Name)
			require.NoError(t, err)
			loaded, err := store.Load()
			require.NoError(t, err)
			require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(op), op))

			if tc.want == "" {
				require.Equal(t, v1alpha3.OperationPhaseFailed, op.Status.Phase)
				require.Equal(t, tc.phase, loaded.Phase)
			} else {
				require.Equal(t, v1alpha3.OperationPhaseInProgress, op.Status.Phase)
				require.Equal(t, tc.want, loaded.Phase)

				if tc.action == "reselect" {
					require.Equal(t, "new-v2", loaded.ReselectedRef.VersionName)
					require.Equal(t, "1.34.1", loaded.ReselectedConfig.Cluster.Version)
				}
				// A restarted reconciler replays the operation without changing intent.
				_, err = r.ReconcileMachineOperation(t.Context(), op.Name)
				require.NoError(t, err)
				reloaded, err := store.Load()
				require.NoError(t, err)
				require.Equal(t, loaded, reloaded)
			}
		})
	}
}

func TestRecoveryDoesNotOverwriteConcurrentCompletion(t *testing.T) {
	cfg := baseConfig()
	op := &v1alpha3.MachineOperation{ObjectMeta: metav1.ObjectMeta{Name: "finished"}, Status: v1alpha3.MachineOperationStatus{Phase: v1alpha3.OperationPhaseComplete}}
	c := fakeStatusClient(op)
	target := &machineOperationTarget{Client: c, worker: &repaveWorker{client: c, reader: c, store: repaveStore{dir: t.TempDir()}, wake: make(chan struct{}, 1)}}
	store, err := agentdaemon.NewMachinaMachineOperationReconciler(c, cfg.MachineName, "node", agentdaemon.MachineOperationHandlers{})
	require.NoError(t, err)
	_, err = target.reconcileRepaveRecovery(t.Context(), store, agentdaemon.MachineOperation{Name: op.Name, Parameters: map[string]string{"action": "retry", "transitionID": "removed"}})
	require.NoError(t, err)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(op), op))
	require.Equal(t, v1alpha3.OperationPhaseComplete, op.Status.Phase)
}
