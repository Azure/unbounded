// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nspawn

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func quietLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

// scriptedRunner is a Runner whose behavior is set per test.
type scriptedRunner struct {
	running     bool
	runningErr  error
	startErrs   []error
	exists      bool
	starts      int
	terminates  int
	resetFailed int
}

func (r *scriptedRunner) Start(context.Context, string) error {
	err := r.startErrs[r.starts]
	r.starts++

	return err
}

func (r *scriptedRunner) Enable(context.Context, string) error { return nil }

func (r *scriptedRunner) Terminate(context.Context, string) error {
	r.terminates++
	r.exists = false

	return nil
}

func (r *scriptedRunner) Exists(context.Context, string) bool { return r.exists }

func (r *scriptedRunner) ResetFailed(context.Context, string) error {
	r.resetFailed++
	return nil
}

func (r *scriptedRunner) Running(context.Context, string) (bool, error) {
	return r.running, r.runningErr
}

func TestStartWithRecovery(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")

	tests := []struct {
		name         string
		runner       *scriptedRunner
		wantRunning  bool
		wantErr      string
		wantStarts   int
		wantTerminat int
	}{
		{name: "already running is not restarted", runner: &scriptedRunner{running: true}, wantRunning: true},
		{name: "clean start", runner: &scriptedRunner{startErrs: []error{nil}}, wantStarts: 1},
		{name: "inspection failure", runner: &scriptedRunner{runningErr: boom}, wantErr: "inspect nspawn service"},
		{
			name:       "unrelated failure is not retried",
			runner:     &scriptedRunner{startErrs: []error{boom}},
			wantErr:    "machinectl start kube1: boom",
			wantStarts: 1,
		},
		{
			name:         "stale registration is terminated and retried",
			runner:       &scriptedRunner{startErrs: []error{boom, nil}, exists: true},
			wantStarts:   2,
			wantTerminat: 1,
		},
		{
			name:         "file exists text is recoverable",
			runner:       &scriptedRunner{startErrs: []error{errors.New("File exists"), nil}},
			wantStarts:   2,
			wantTerminat: 1,
		},
		{
			name:         "retry failure is reported",
			runner:       &scriptedRunner{startErrs: []error{boom, boom}, exists: true},
			wantErr:      "after recovery",
			wantStarts:   2,
			wantTerminat: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			running, err := StartWithRecovery(t.Context(), quietLog(), tc.runner, "kube1")
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}

			require.Equal(t, tc.wantRunning, running)
			require.Equal(t, tc.wantStarts, tc.runner.starts)
			require.Equal(t, tc.wantTerminat, tc.runner.terminates)
		})
	}
}

func TestIsAlreadyExists(t *testing.T) {
	t.Parallel()

	require.False(t, IsAlreadyExists(nil))
	require.True(t, IsAlreadyExists(errors.New("Machine 'kube1' already exists")))
	require.True(t, IsAlreadyExists(errors.New("File exists")))
	require.False(t, IsAlreadyExists(errors.New("permission denied")))
}

func TestServiceName(t *testing.T) {
	t.Parallel()

	require.Equal(t, "systemd-nspawn@kube2.service", ServiceName("kube2"))
}

// onlyTool makes name the only executable on PATH.
func onlyTool(t *testing.T, name, script string) {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script+"\n"), 0o755))
	t.Setenv("PATH", dir)
}

func TestRegisteredAndFirstRegistered(t *testing.T) {
	onlyTool(t, "machinectl", "printf 'kube2 container systemd-nspawn - - -\\nkube10 container systemd-nspawn - - -\\n'")

	ok, err := Registered(t.Context(), quietLog(), "kube2")
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = Registered(t.Context(), quietLog(), "kube1")
	require.NoError(t, err)
	require.False(t, ok)

	first, err := FirstRegistered(t.Context(), quietLog(), "kube1", "kube2")
	require.NoError(t, err)
	require.Equal(t, "kube2", first)

	none, err := FirstRegistered(t.Context(), quietLog(), "kube1")
	require.NoError(t, err)
	require.Empty(t, none)
}

func TestRegisteredFailsClosedButCleanupToleratesMissingTool(t *testing.T) {
	onlyTool(t, "unrelated", "exit 0") // PATH without machinectl

	_, err := Registered(t.Context(), quietLog(), "kube1")
	require.Error(t, err)
	require.True(t, MissingTool(err))

	ok, err := RegisteredForCleanup(t.Context(), quietLog(), "kube1")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestRegisteredForCleanupPropagatesRealFailures(t *testing.T) {
	onlyTool(t, "machinectl", "exit 1")

	_, err := RegisteredForCleanup(t.Context(), quietLog(), "kube1")
	require.Error(t, err)
}

func TestConfirmNotEnabled(t *testing.T) {
	for state, ok := range map[string]bool{"disabled": true, "masked": true, "enabled": false, "linked": false} {
		t.Run(state, func(t *testing.T) {
			onlyTool(t, "systemctl", "printf '"+state+"\\n'")

			err := ConfirmNotEnabled(t.Context(), quietLog(), ServiceName("kube1"))
			if ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}

	onlyTool(t, "systemctl", "exit 1")
	require.Error(t, ConfirmNotEnabled(t.Context(), quietLog(), ServiceName("kube1")))
}

func TestRemoveConfig(t *testing.T) {
	dir := t.TempDir()
	p := ConfigPaths{
		NSpawnFile:             filepath.Join(dir, "kube1.nspawn"),
		OverrideDir:            filepath.Join(dir, "override.d"),
		ConfigRegenerationUnit: filepath.Join(dir, "regen.service"),
	}

	require.NoError(t, os.WriteFile(p.NSpawnFile, nil, 0o644))
	require.NoError(t, os.MkdirAll(p.OverrideDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(p.OverrideDir, "x.conf"), nil, 0o644))
	require.NoError(t, os.WriteFile(p.ConfigRegenerationUnit, nil, 0o644))

	require.NoError(t, RemoveConfig(quietLog(), p))

	for _, f := range []string{p.NSpawnFile, p.OverrideDir, p.ConfigRegenerationUnit} {
		_, err := os.Stat(f)
		require.ErrorIs(t, err, os.ErrNotExist)
	}

	// Removing again is a no-op.
	require.NoError(t, RemoveConfig(quietLog(), p))
}

func TestRemoveBPFFSMountAbsentPathIsNoop(t *testing.T) {
	require.NoError(t, RemoveBPFFSMount(t.Context(), quietLog(), filepath.Join(t.TempDir(), "missing")))
}

func TestStopMissingMachinectlIsNoop(t *testing.T) {
	// A host where bootstrap died before systemd-container was installed.
	onlyTool(t, "systemctl", "case \"$1\" in show) echo \"\" ;; esac\nexit 0")

	require.NoError(t, Stop(t.Context(), quietLog(), "kube1"))
}
