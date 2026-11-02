// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func installFakeSystemctl(t *testing.T, script string) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "systemctl")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRebootHost(t *testing.T) {
	tests := []struct {
		name          string
		script        string
		cancelContext bool
		wantErr       string
	}{
		{
			name:   "success",
			script: "exit 0\n",
		},
		{
			name:          "success after caller cancellation",
			script:        "exit 0\n",
			cancelContext: true,
		},
		{
			name:   "systemd stopping",
			script: "if [ \"$1\" = reboot ]; then exit 1; fi\necho stopping\n",
		},
		{
			name:    "reboot failed",
			script:  "if [ \"$1\" = reboot ]; then exit 1; fi\necho running\n",
			wantErr: "systemctl reboot",
		},
		{
			name:    "state unavailable",
			script:  "exit 1\n",
			wantErr: "systemctl reboot",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installFakeSystemctl(t, tt.script)

			log := slog.New(slog.NewTextHandler(os.Stderr, nil))

			ctx := context.Background()
			if tt.cancelContext {
				canceledCtx, cancel := context.WithCancel(ctx)
				cancel()

				ctx = canceledCtx
			}

			err := RebootHost(ctx, log)
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.wantErr)
			}
		})
	}
}
