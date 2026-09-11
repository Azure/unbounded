// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
	"github.com/Azure/unbounded/pkg/agent/installstate"
)

func TestBootstrapReporterInitializesAfterInputs(t *testing.T) {
	t.Parallel()

	resolved := false
	calls := 0
	r := &bootstrapReporter{initialize: func(context.Context) *daemon.BootstrapStatusReporter {
		require.True(t, resolved, "reporter must observe credentials resolved by attestation")

		calls++

		return &daemon.BootstrapStatusReporter{}
	}}

	require.Zero(t, calls)

	resolved = true

	r.StageStarted(context.Background(), installstate.CheckpointPreparingRootFS)
	r.StageStarted(context.Background(), installstate.CheckpointStartingNode)
	require.Equal(t, 1, calls)
}

func TestDaemonRepairDoesNotUseBootstrapReporter(t *testing.T) {
	t.Parallel()

	r := &bootstrapReporter{initialize: func(context.Context) *daemon.BootstrapStatusReporter {
		t.Fatal("repair must not initialize bootstrap credentials")
		return nil
	}}
	r.StageStarted(context.Background(), installstate.CheckpointRepairingDaemon)
	r.succeeded(context.Background())
}
