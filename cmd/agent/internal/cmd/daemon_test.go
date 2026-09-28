// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"errors"
	"fmt"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
)

// TestDaemonCommandKeepsDeferredOutOfTheJournalAsAnError covers what an
// operator actually sees when the daemon stands down for an unfinished
// installation.
//
// The daemon already reports it as a warning. Cobra would then print "Error:"
// over the top of that, and without SilenceUsage the whole flag listing too, so
// an ordinary state reads as both a fault and a misuse of the command. The
// journal is the one place this behavior is observed, so narrating it wrongly
// there undoes the fix where it counts.
func TestDaemonCommandKeepsDeferredOutOfTheJournalAsAnError(t *testing.T) {
	t.Parallel()

	cmd := newCmdDaemon(&CommandContext{LogFormat: "text"})
	require.True(t, cmd.SilenceUsage, "systemd passes fixed arguments; a runtime failure is never a usage problem")

	// Run wraps the sentinel with context before it reaches here, which is how
	// it appears in the journal, so recognition has to survive wrapping.
	wrapped := fmt.Errorf("find active machine: %w", daemon.ErrDeferred)

	require.ErrorIs(t, quietWhenDeferred(cmd, wrapped), daemon.ErrDeferred)
	require.True(t, cmd.SilenceErrors, "a deferred daemon must not be narrated as an error")
}

// TestDaemonCommandStillReportsRealFailures keeps the suppression narrow. A
// daemon that cannot reach the API server has genuinely failed, and silencing
// that would hide the fault this path exists to distinguish from.
func TestDaemonCommandStillReportsRealFailures(t *testing.T) {
	t.Parallel()

	cmd := &cobra.Command{}

	real := errors.New("kube client unreachable")
	require.ErrorIs(t, quietWhenDeferred(cmd, real), real)
	require.False(t, cmd.SilenceErrors, "genuine failures must still be reported")

	require.NoError(t, quietWhenDeferred(cmd, nil))
	require.False(t, cmd.SilenceErrors)
}
