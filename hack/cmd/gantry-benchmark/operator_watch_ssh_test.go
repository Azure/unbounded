// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestOperatorVMWatcherSSHSendsCurrentStatusScript(t *testing.T) {
	for _, mode := range []string{"config", "host"} {
		t.Run(mode, func(t *testing.T) {
			repository, err := findRepoRoot()
			if err != nil {
				t.Fatal(err)
			}

			root := t.TempDir()

			bin := filepath.Join(root, "bin")
			if err := os.Mkdir(bin, 0o755); err != nil {
				t.Fatal(err)
			}

			input := filepath.Join(root, "status-script")

			ssh := `#!/bin/sh
cat > "$TEST_SSH_INPUT"
printf 'service: active\nstage: prepare\n'
`
			if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(ssh), 0o755); err != nil {
				t.Fatal(err)
			}

			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("TEST_SSH_INPUT", input)
			t.Setenv("OPERATOR_SSH_CONFIG", "")
			t.Setenv("OPERATOR_SSH_HOST", "")

			switch mode {
			case "config":
				config := filepath.Join(root, "ssh-config")
				if err := os.WriteFile(config, []byte("Host operator\n"), 0o600); err != nil {
					t.Fatal(err)
				}

				t.Setenv("OPERATOR_SSH_CONFIG", config)
			case "host":
				t.Setenv("OPERATOR_SSH_HOST", "operator.example.invalid")
				t.Setenv("OPERATOR_SSH_KEY", filepath.Join(root, "key"))
			}

			command := exec.CommandContext(t.Context(), "bash", filepath.Join(repository, "hack", "gantry-benchmark", "operator-vm-watch.sh"))

			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("SSH status failed: %v\n%s", err, output)
			}

			got, err := os.ReadFile(input)
			if err != nil {
				t.Fatal(err)
			}

			want, err := os.ReadFile(filepath.Join(repository, "hack", "gantry-benchmark", "operator-vm-status.sh"))
			if err != nil {
				t.Fatal(err)
			}

			if !bytes.Equal(got, want) {
				t.Fatal("SSH transport did not send the current local status script")
			}
		})
	}
}
