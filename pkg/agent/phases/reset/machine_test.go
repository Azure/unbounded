// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package reset

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMachineInspectionFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name       string
		script     string
		wantExists bool
		wantError  bool
	}{
		{name: "registered", script: "printf 'kube1 container systemd-nspawn - - -\n'", wantExists: true},
		{name: "different machine", script: "printf 'kube10 container systemd-nspawn - - -\n'"},
		{name: "absent", script: "exit 0"},
		{name: "inspection denied", script: "exit 1", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "machinectl"), []byte("#!/bin/sh\n"+tc.script+"\n"), 0o755))
			t.Setenv("PATH", dir)
			exists, err := machineExists(t.Context(), slog.New(slog.DiscardHandler), "kube1")
			require.Equal(t, tc.wantExists, exists)

			if tc.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
