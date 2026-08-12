// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReconcileNSpawnLifecycleRestartsManagedUnit(t *testing.T) {
	t.Parallel()

	task := ReconcileNSpawnLifecycle(slog.New(slog.DiscardHandler), "kube1").(*reconcileNSpawnLifecycle)

	var gotUnit string

	task.restart = func(_ context.Context, _ *slog.Logger, unit string) error {
		gotUnit = unit

		return nil
	}

	require.NoError(t, task.Do(context.Background()))
	require.Equal(t, "systemd-nspawn@kube1.service", gotUnit)
}
