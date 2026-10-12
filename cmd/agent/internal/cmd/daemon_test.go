// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

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

// TestDaemonRestartsInPlace: a daemon that stopped to be restarted replaces
// itself with the binary its unit runs, with its own arguments and environment,
// so systemd sees no restart and there is no RestartSec delay.
func TestDaemonRestartsInPlace(t *testing.T) {
	t.Parallel()

	r, execs := fakeInPlaceRestart("/opt/unbounded/agent/bin/unbounded-agent-current", nil, nil)

	for _, err := range []error{daemon.ErrRestartRequested, fmt.Errorf("stopping: %w", daemon.ErrRestartRequested)} {
		*execs = nil

		require.NoError(t, r.after(slog.New(slog.DiscardHandler), err))
		require.Equal(t, []fakeExec{{
			argv0: "/opt/unbounded/agent/bin/unbounded-agent-current",
			argv:  []string{"/opt/unbounded/agent/bin/unbounded-agent-current", "daemon", "--log-format", "json"},
			envv:  []string{"INVOCATION_ID=abc", "PATH=/usr/bin"},
		}}, *execs, "%v: the unit's binary, as the unit runs it, in place", err)
	}
}

// TestDaemonFallsBackToExitingToBeRestarted: when the daemon cannot replace
// itself, it exits 0 and says why, and the unit's Restart=always starts it
// again after RestartSec. Exiting non-zero would read as a crash.
func TestDaemonFallsBackToExitingToBeRestarted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		resolve   error
		exec      error
		wantExecs int
	}{
		{name: "the binary cannot be resolved", resolve: errors.New("no host root"), wantExecs: 0},
		{name: "exec fails", exec: errors.New("permission denied"), wantExecs: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var logged bytes.Buffer

			r, execs := fakeInPlaceRestart("/opt/unbounded/agent/bin/unbounded-agent-current", tt.resolve, tt.exec)

			require.NoError(t, r.after(slog.New(slog.NewTextHandler(&logged, nil)), daemon.ErrRestartRequested))
			require.Len(t, *execs, tt.wantExecs)
			require.Contains(t, logged.String(), "exiting for systemd to restart")
		})
	}
}

// TestDaemonResultsOtherThanARestartPassThrough: only a requested restart
// re-executes the daemon. A deferred daemon keeps its own exit code, and a
// failure stays a failure.
func TestDaemonResultsOtherThanARestartPassThrough(t *testing.T) {
	t.Parallel()

	r, execs := fakeInPlaceRestart("/opt/unbounded/agent/bin/unbounded-agent-current", nil, nil)
	log := slog.New(slog.DiscardHandler)

	real := errors.New("kube client unreachable")
	require.ErrorIs(t, r.after(log, real), real)
	require.ErrorIs(t, r.after(log, daemon.ErrDeferred), daemon.ErrDeferred)
	require.NoError(t, r.after(log, nil))
	require.Empty(t, *execs)
}

type fakeExec struct {
	argv0      string
	argv, envv []string
}

// fakeInPlaceRestart returns an inPlaceRestart whose current path is path, or
// resolveErr, and whose exec records each call and returns execErr. A real exec
// does not return when it succeeds; this one returns nil.
func fakeInPlaceRestart(path string, resolveErr, execErr error) (inPlaceRestart, *[]fakeExec) {
	var execs []fakeExec

	return inPlaceRestart{
		currentPath: func() (string, error) { return path, resolveErr },
		exec: func(argv0 string, argv, envv []string) error {
			execs = append(execs, fakeExec{argv0: argv0, argv: argv, envv: envv})

			return execErr
		},
		args: []string{"daemon", "--log-format", "json"},
		env:  []string{"INVOCATION_ID=abc", "PATH=/usr/bin"},
	}, &execs
}

// TestDaemonRestartsInPlaceForReal runs the real unix.Exec from a Go process
// with its runtime threads running, as the daemon does: a helper copy of this
// test binary prints its PID and re-executes a shell through inPlaceRestart,
// and the shell must report the same PID.
func TestDaemonRestartsInPlaceForReal(t *testing.T) {
	if os.Getenv("UNBOUNDED_TEST_REEXEC_HELPER") == "1" {
		fmt.Printf("before %d\n", os.Getpid())

		r := inPlaceRestart{
			currentPath: func() (string, error) { return "/bin/sh", nil },
			exec:        unix.Exec,
			args:        []string{"-c", `echo "after $$"`},
			env:         os.Environ(),
		}

		// Reached only if exec failed.
		_ = r.after(slog.New(slog.DiscardHandler), daemon.ErrRestartRequested)

		os.Exit(3)
	}

	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh to re-execute")
	}

	helper := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestDaemonRestartsInPlaceForReal$")

	helper.Env = append(os.Environ(), "UNBOUNDED_TEST_REEXEC_HELPER=1")

	out, err := helper.Output()
	require.NoError(t, err, "the helper exited with the fallback code: %s", out)

	var before, after int

	_, err = fmt.Sscanf(string(out), "before %d\nafter %d\n", &before, &after)
	require.NoError(t, err, "helper output: %q", out)
	require.Equal(t, before, after, "the shell ran as the same process")
}
