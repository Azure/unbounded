// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOperatorVMStatusSelectsCurrentLifecycle(t *testing.T) {
	type batch struct {
		name     string
		state    string
		modified int64
		invalid  bool
	}

	for _, test := range []struct {
		name            string
		primary         string
		primaryModified int64
		batches         []batch
		want            string
		wantError       bool
		largeLog        bool
		latestAlias     bool
	}{
		{
			name: "active batch beats newer stale lifecycle", primary: "failed", primaryModified: 300,
			batches: []batch{{name: "current", state: "active", modified: 200}}, want: "batch ID: current",
		},
		{
			name: "completed batch remains visible", primary: "inactive", primaryModified: 100,
			batches: []batch{{name: "finished", state: "inactive", modified: 200}}, want: "stage: completed",
		},
		{
			name: "active standard lifecycle is unchanged", primary: "active", primaryModified: 100,
			batches: []batch{{name: "current", state: "active", modified: 200}}, want: "=== Gantry benchmark operator ===",
		},
		{
			name: "newer standard lifecycle is retained", primary: "inactive", primaryModified: 300,
			batches: []batch{{name: "finished", state: "inactive", modified: 200}}, want: "=== Gantry benchmark operator ===",
		},
		{
			name: "multiple active batches fail explicitly", primary: "failed", primaryModified: 100,
			batches: []batch{{name: "first", state: "active", modified: 200}, {name: "second", state: "active", modified: 300}},
			want:    "multiple active benchmark batches", wantError: true,
		},
		{
			name: "invalid batch progress fails explicitly", primary: "failed", primaryModified: 100,
			batches: []batch{{name: "invalid", state: "active", modified: 200, invalid: true}},
			want:    "invalid benchmark batch progress", wantError: true,
		},
		{
			name: "batch summary survives Azure output limit", primary: "failed", primaryModified: 100,
			batches: []batch{{name: "current", state: "active", modified: 200}},
			want:    "batch ID: current", largeLog: true,
		},
		{
			name: "latest symlink does not duplicate an active batch", primary: "failed", primaryModified: 100,
			batches: []batch{{name: "current", state: "active", modified: 200}},
			want:    "batch ID: current", latestAlias: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, err := findRepoRoot()
			if err != nil {
				t.Fatal(err)
			}

			root := t.TempDir()
			artifacts := filepath.Join(root, "artifacts")

			bin := filepath.Join(root, "bin")
			for _, directory := range []string{artifacts, bin} {
				if err := os.MkdirAll(directory, 0o755); err != nil {
					t.Fatal(err)
				}
			}

			write := func(path, content string, mode os.FileMode) {
				t.Helper()

				if err := os.WriteFile(path, []byte(content), mode); err != nil {
					t.Fatal(err)
				}
			}
			timestamp := func(path string, seconds int64) {
				t.Helper()

				at := time.Unix(seconds, 0)
				if err := os.Chtimes(path, at, at); err != nil {
					t.Fatal(err)
				}
			}
			progress := filepath.Join(artifacts, "progress.json")
			write(progress, `{"stage":"preflight","message":"standard lifecycle"}`, 0o600)
			timestamp(progress, test.primaryModified)

			states := map[string]string{"gantry-benchmark-operator.service": test.primary}
			for _, item := range test.batches {
				directory := filepath.Join(artifacts, item.name)
				if err := os.MkdirAll(directory, 0o755); err != nil {
					t.Fatal(err)
				}

				stage := "prepare"
				if item.state == "inactive" {
					stage = "completed"
				}

				body := fmt.Sprintf(`{"batch_id":%q,"stage":%q,"run_index":2,"completed_runs":1,"planned_runs":5}`, item.name, stage)
				if item.invalid {
					body = fmt.Sprintf(`{"batch_id":%q,"stage":false}`, item.name)
				}

				path := filepath.Join(directory, "progress.json")
				write(path, body, 0o600)
				timestamp(path, item.modified)

				log := "actual batch log " + item.name + "\n"
				if test.largeLog {
					log = strings.Repeat(strings.Repeat("x", 300)+"\n", 100) + log
				}

				write(filepath.Join(directory, "operator.log"), log, 0o600)

				states["gantry-"+item.name+"-worker.service"] = item.state
			}

			if test.latestAlias {
				if err := os.Symlink(filepath.Join(artifacts, "current"), filepath.Join(artifacts, "latest")); err != nil {
					t.Fatal(err)
				}
			}

			encodedStates, err := json.Marshal(states)
			if err != nil {
				t.Fatal(err)
			}

			write(filepath.Join(bin, "systemctl"), `#!/bin/sh
case "$1" in
  is-active)
    state=$(printf '%s' "$TEST_SERVICE_STATES" | jq -r --arg unit "$2" '.[$unit] // "inactive"')
    printf '%s\n' "$state"
    [ "$state" = active ] || [ "$state" = activating ]
    ;;
  show) printf 'test timestamp\n' ;;
  *) exit 1 ;;
esac
`, 0o755)
			write(filepath.Join(bin, "pgrep"), "#!/bin/sh\nexit 1\n", 0o755)
			write(filepath.Join(bin, "podman"), "#!/bin/sh\nexit 0\n", 0o755)
			config := filepath.Join(root, "env")
			write(config, fmt.Sprintf("BENCHMARK_OPERATOR_HOME=%q\nBENCHMARK_ARTIFACT_ROOT=%q\nBENCHMARK_REPO_ROOT=%q\nKUBECONFIG=%q\n",
				root, artifacts, root, filepath.Join(root, "missing-kubeconfig")), 0o600)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("GANTRY_BENCHMARK_CONFIG", config)
			t.Setenv("TEST_SERVICE_STATES", string(encodedStates))
			command := exec.CommandContext(t.Context(), "bash", filepath.Join(repository, "hack", "gantry-benchmark", "operator-vm-status.sh"))

			output, err := command.CombinedOutput()
			if (err != nil) != test.wantError {
				t.Fatalf("status error = %v, want error=%t\n%s", err, test.wantError, output)
			}

			if !strings.Contains(string(output), test.want) {
				t.Fatalf("status missing %q:\n%s", test.want, output)
			}

			if strings.HasPrefix(test.want, "batch ID:") && !strings.Contains(string(output), "actual batch log current") {
				t.Fatalf("batch view did not include its real operator log:\n%s", output)
			}

			if test.largeLog && len(output) > 4096 {
				t.Fatalf("batch status is %d bytes, exceeds Azure Run Command's 4096-byte output limit", len(output))
			}
		})
	}
}
