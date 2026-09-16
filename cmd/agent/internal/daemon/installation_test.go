// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	ctrl "sigs.k8s.io/controller-runtime"

	shared "github.com/Azure/unbounded/pkg/agent/daemon"
	"github.com/Azure/unbounded/pkg/agent/installstate"
)

func TestInstallationContentionPreventsControllerWork(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := installstate.NewStore(filepath.Join(dir, "state"), filepath.Join(dir, "lock"))
	lock, err := store.AcquireLock()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, lock.Release()) })

	target := &machineOperationTarget{installation: store, log: discardLogger()}
	// Nil clients, operation stores and node operators ensure contention returns
	// before discovery, status publication, binary staging or local mutation.
	for name, handler := range map[string]func(context.Context, shared.MachineOperationStore[int64], shared.MachineOperation) (ctrl.Result, error){
		"reboot":  target.reconcileNodeReboot,
		"upgrade": target.reconcileAgentUpgrade,
		"reset":   target.reconcileAgentReset,
	} {
		t.Run(name, func(t *testing.T) {
			result, err := handler(t.Context(), nil, shared.MachineOperation{})
			require.NoError(t, err)
			require.Positive(t, result.RequeueAfter)
		})
	}

	repave := &repaveReconciler{installation: store, log: discardLogger()}
	result, err := repave.ReconcileRepave(t.Context(), "node-delete")
	require.NoError(t, err)
	require.Positive(t, result.RequeueAfter)
}
