// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOperatorVMWatcherRetriesOnlyBusyRunCommand(t *testing.T) {
	for _, test := range []struct {
		mode      string
		wantCalls string
		wantError bool
	}{
		{mode: "busy-then-ready", wantCalls: "2"},
		{mode: "authentication-error", wantCalls: "1", wantError: true},
		{mode: "always-busy", wantCalls: "12", wantError: true},
	} {
		t.Run(test.mode, func(t *testing.T) {
			repository, err := findRepoRoot()
			if err != nil {
				t.Fatal(err)
			}

			root := t.TempDir()

			bin := filepath.Join(root, "bin")
			if err := os.Mkdir(bin, 0o755); err != nil {
				t.Fatal(err)
			}

			write := func(path, content string) {
				t.Helper()

				if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			calls := filepath.Join(root, "calls")
			write(calls, "0\n")
			write(filepath.Join(bin, "az"), `#!/bin/sh
count=$(cat "$TEST_CALLS")
count=$((count+1))
printf '%s\n' "$count" > "$TEST_CALLS"
if [ "$TEST_MODE" = authentication-error ]; then
  echo "Azure authentication failed" >&2
  exit 7
fi
if [ "$TEST_MODE" = always-busy ] || [ "$count" = 1 ]; then
  echo "ERROR: (Conflict) Run command extension execution is in progress." >&2
  exit 1
fi
printf '[stdout]\nservice: active\nstage: prepare\nbatch ID: current\n[stderr]\n'
`)
			write(filepath.Join(bin, "sleep"), "#!/bin/sh\nexit 0\n")
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("AZURE_RESOURCE_GROUP", "test-resource-group")
			t.Setenv("OPERATOR_SSH_CONFIG", "")
			t.Setenv("OPERATOR_SSH_HOST", "")
			t.Setenv("OPERATOR_RUN_COMMAND_LOCK", filepath.Join(root, "run-command.lock"))
			t.Setenv("TEST_CALLS", calls)
			t.Setenv("TEST_MODE", test.mode)
			command := exec.CommandContext(t.Context(), "bash", filepath.Join(repository, "hack", "gantry-benchmark", "operator-vm-watch.sh"))

			output, err := command.CombinedOutput()
			if (err != nil) != test.wantError {
				t.Fatalf("watch error = %v, want error=%t\n%s", err, test.wantError, output)
			}

			count, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}

			if strings.TrimSpace(string(count)) != test.wantCalls {
				t.Fatalf("Azure calls = %q, want %s", count, test.wantCalls)
			}

			if !test.wantError && !strings.Contains(string(output), "batch ID: current") {
				t.Fatalf("successful retry did not display the active batch:\n%s", output)
			}
		})
	}
}
