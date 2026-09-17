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
			exists, err := RegisteredMachine(t.Context(), slog.New(slog.DiscardHandler), "kube1")
			require.Equal(t, tc.exists, exists)

			if tc.failed {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestConfirmNotEnabledOnlyAcceptsUnstartableStates pins which reported unit
// file states let reset continue after machinectl disable fails. Anything that
// could still start the unit at boot, or an inspection that did not answer, has
// to stop reset rather than leave an enablement symlink behind for a machine
// whose config and rootfs are about to be deleted.
func TestConfirmNotEnabledOnlyAcceptsUnstartableStates(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		confirmed    bool
	}{
		{"never-enabled", "printf 'disabled\\n'", true},
		{"no-unit-file", "printf '\\n'", true},
		{"not-found", "printf 'not-found\\n'", true},
		{"masked", "printf 'masked\\n'", true},
		{"static", "printf 'static\\n'", true},
		{"indirect", "printf 'indirect\\n'", true},
		{"enabled", "printf 'enabled\\n'", false},
		{"enabled-runtime", "printf 'enabled-runtime\\n'", false},
		{"linked", "printf 'linked\\n'", false},
		{"generated", "printf 'generated\\n'", false},
		{"inspection-failed", "exit 1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "systemctl"), []byte("#!/bin/sh\n"+tc.script+"\n"), 0o755))
			t.Setenv("PATH", dir)

			err := confirmNotEnabled(t.Context(), slog.New(slog.DiscardHandler), "systemd-nspawn@kube1.service")
			if tc.confirmed {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

// TestStopMachineFailsWhenDisableLeavesUnitEnabled covers the whole task: a
// failed disable is tolerated only once systemd confirms the unit cannot start
// at boot.
func TestStopMachineFailsWhenDisableLeavesUnitEnabled(t *testing.T) {
	for _, tc := range []struct {
		name, disable, unitFileState string
		wantErr                      bool
	}{
		{"disable-succeeds", "exit 0", "enabled", false},
		{"disable-fails-but-not-enabled", "exit 1", "disabled", false},
		{"disable-fails-and-still-enabled", "exit 1", "enabled", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// list reports no registered machines, so a tolerated failure falls
			// through to "nothing to stop" and the task succeeds.
			machinectl := "#!/bin/sh\ncase \"$1\" in\ndisable) " + tc.disable + " ;;\nlist) exit 0 ;;\nesac\nexit 0\n"
			require.NoError(t, os.WriteFile(filepath.Join(dir, "machinectl"), []byte(machinectl), 0o755))

			systemctl := "#!/bin/sh\nif [ \"$1\" = show ]; then printf '" + tc.unitFileState + "\\n'; fi\nexit 0\n"
			require.NoError(t, os.WriteFile(filepath.Join(dir, "systemctl"), []byte(systemctl), 0o755))
			t.Setenv("PATH", dir)

			err := StopMachine(slog.New(slog.DiscardHandler), "kube1").Do(t.Context())
			if tc.wantErr {
				require.ErrorContains(t, err, "systemd-nspawn@kube1.service is enabled")
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
			if output == "[]" || output == `[{"table":"unexpected-name"}]` {
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
