// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

func TestImageWorkflowProvenance(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "images.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Name, ID, Uses, Run string
				With                map[string]string
			}
		}
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}

	checkout, provenance, build := -1, -1, -1

	var script string

	for i, step := range workflow.Jobs["build"].Steps {
		switch {
		case strings.HasPrefix(step.Uses, "actions/checkout@"):
			checkout = i
		case step.ID == "provenance":
			provenance, script = i, step.Run
		case step.Name == "Build and push":
			build = i

			if step.With["context"] != "." {
				t.Fatal("image must use the checkout whose provenance was recorded")
			}

			for _, arg := range []string{
				"VERSION=${{ needs.resolve.outputs.tag }}",
				"CONTAINER_REGISTRY=${{ env.CONTAINER_REGISTRY }}",
				"GIT_COMMIT=${{ steps.provenance.outputs.git_commit }}",
				"BUILD_TIME=${{ steps.provenance.outputs.build_time }}",
			} {
				if !strings.Contains(step.With["build-args"], arg+"\n") {
					t.Errorf("missing build argument %s", arg)
				}
			}
		}
	}

	if checkout < 0 || provenance <= checkout || build <= provenance || script == "" {
		t.Fatal("expected checkout, provenance script, then image build")
	}

	for _, tc := range []struct {
		name, setup string
		wantError   bool
	}{
		{name: "checked out revision"},
		{name: "git failure", setup: "git() { return 17; }; ", wantError: true},
		{name: "clock failure", setup: "date() { return 18; }; ", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outputPath := filepath.Join(t.TempDir(), "outputs")
			cmd := exec.Command("timeout", "--signal=TERM", "--kill-after=10s", "30s", "bash", "-e", "-c", tc.setup+script)

			cmd.Env = append(os.Environ(), "GITHUB_OUTPUT="+outputPath)

			output, err := cmd.CombinedOutput()
			if (err != nil) != tc.wantError {
				t.Fatalf("provenance error = %v, want error %t: %s", err, tc.wantError, output)
			}

			if tc.wantError {
				if data, err := os.ReadFile(outputPath); err == nil && len(data) != 0 {
					t.Fatalf("published incomplete provenance: %s", data)
				}

				return
			}

			data, err := os.ReadFile(outputPath)
			if err != nil {
				t.Fatal(err)
			}

			head, err := exec.Command("timeout", "--signal=TERM", "--kill-after=10s", "30s", "git", "rev-parse", "HEAD").Output()
			if err != nil {
				t.Fatal(err)
			}

			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) != 2 || lines[0] != "git_commit="+strings.TrimSpace(string(head)) {
				t.Fatalf("wrong checkout provenance: %s", data)
			}

			stamp := strings.TrimPrefix(lines[1], "build_time=")
			if _, err := time.Parse(time.RFC3339, stamp); err != nil || !strings.HasSuffix(stamp, "Z") {
				t.Fatalf("invalid UTC build time: %q", lines[1])
			}
		})
	}
}
