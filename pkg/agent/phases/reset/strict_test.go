// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package reset

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMachineInspectionFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name, script   string
		exists, failed bool
	}{
		{"registered", "printf 'kube1 container systemd-nspawn - - -\\n'", true, false},
		{"different", "printf 'kube10 container systemd-nspawn - - -\\n'", false, false},
		{"absent", "exit 0", false, false},
		{"denied", "exit 1", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "machinectl"), []byte("#!/bin/sh\n"+tc.script+"\n"), 0o755))
			t.Setenv("PATH", dir)
			exists, err := machineExists(t.Context(), slog.New(slog.DiscardHandler), "kube1")
			require.Equal(t, tc.exists, exists)

			if tc.failed {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestCleanupRoutesPropagatesInspectionAndMutationFailures(t *testing.T) {
	t.Parallel()

	for _, failure := range []string{"-4 -N -j rule show", "-4 rule del table 51820", "-4 -N -j route show table all", "-4 route flush table 51820", "-6 -N -j rule show", "-6 rule del table 51820", "-6 -N -j route show table all", "-6 route flush table 51820"} {
		t.Run(failure, func(t *testing.T) {
			injected := errors.New("injected network failure")
			task := &cleanupRoutes{log: slog.New(slog.DiscardHandler), output: func(_ context.Context, args ...string) (string, error) {
				if strings.Join(args, " ") == failure {
					return "", injected
				}

				return `[{"table":51820}]`, nil
			}}
			require.ErrorIs(t, task.Do(t.Context()), injected)
		})
	}
}

func TestCleanupRoutesPreservesUnrelatedTables(t *testing.T) {
	t.Parallel()

	var mutations []string

	task := &cleanupRoutes{log: slog.New(slog.DiscardHandler), output: func(_ context.Context, args ...string) (string, error) {
		if args[1] == "-N" {
			return `[{"table":"main"},{"table":51819},{"table":51820},{"table":"51820"},{"table":51899},{"table":51900}]`, nil
		}

		mutations = append(mutations, strings.Join(args, " "))

		return "", nil
	}}
	require.NoError(t, task.Do(t.Context()))
	require.Equal(t, []string{
		"-4 rule del table 51820", "-4 rule del table 51820", "-4 rule del table 51899",
		"-4 route flush table 51820", "-4 route flush table 51899",
		"-6 rule del table 51820", "-6 rule del table 51820", "-6 rule del table 51899",
		"-6 route flush table 51820", "-6 route flush table 51899",
	}, mutations)
}

func TestCleanupRoutesAbsenceAndMalformedInventory(t *testing.T) {
	t.Parallel()

	for _, output := range []string{"[]", "invalid", `[{"table":"unexpected-name"}]`, `[{"table":{}}]`} {
		t.Run(output, func(t *testing.T) {
			task := &cleanupRoutes{log: slog.New(slog.DiscardHandler), output: func(_ context.Context, args ...string) (string, error) {
				require.Contains(t, args, "show", "invalid inventory must not authorize mutation")
				return output, nil
			}}

			err := task.Do(t.Context())
			if output == "[]" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestFileCleanupPropagatesSubstantiveFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "child"), []byte("data"), 0o600))

	log := slog.New(slog.DiscardHandler)
	require.Error(t, removeFileIfExists(log, dir))
	require.NoError(t, removeAllIfExists(log, dir))
	require.NoError(t, removeFileIfExists(log, dir))
}
