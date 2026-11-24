// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package orcadev

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSpawnPortForwardCapturesStderrBeforeReturning(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
		want   string
	}{
		{
			name: "large-stderr",
			script: `#!/bin/sh
printf 'discarded-prefix\n' >&2
i=0
while [ "$i" -lt 2048 ]; do
    printf 'abcdefghijklmnopqrstuvwxyz0123456789\n' >&2
    i=$((i + 1))
done
printf 'final startup diagnostic\n' >&2
exit 1
`,
			want: "final startup diagnostic",
		},
		{
			name:   "empty-stderr",
			script: "#!/bin/sh\nexit 1\n",
			want:   "stderr: )",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(tc.script), 0o755); err != nil {
				t.Fatal(err)
			}

			t.Setenv("PATH", dir)

			g := defaultGlobalFlags()
			g.kubeContext = "stderr-regression-test"
			g.namespace = "stderr-regression-test"

			cleanup, err := spawnPortForward(t.Context(), g, portForwardSpec{
				service: "test", localPort: 12345, remotePort: 12345,
			})
			if cleanup != nil {
				cleanup()
				t.Fatal("unexpected successful port-forward")
			}

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("startup error = %v, want diagnostic %q", err, tc.want)
			}

			if strings.Contains(err.Error(), "discarded-prefix") {
				t.Fatal("stderr diagnostics exceeded the ring buffer capacity")
			}
		})
	}
}
