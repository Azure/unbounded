// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racerobject_test

import (
	"go/version"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

func TestImageToolchainMeetsModuleMinimum(t *testing.T) {
	module, err := os.ReadFile("../../go.mod")
	require.NoError(t, err)
	image, err := os.ReadFile("../../images/racer-object/Dockerfile")
	require.NoError(t, err)

	var minimum string

	for line := range strings.SplitSeq(string(module), "\n") {
		if value, ok := strings.CutPrefix(line, "go "); ok {
			minimum = "go" + strings.TrimSpace(value)
			break
		}
	}

	_, tag, ok := strings.Cut(string(image), "docker.io/library/golang:")
	require.True(t, ok)
	toolchain, _, ok := strings.Cut(tag, "-trixie")
	require.True(t, ok)
	require.True(t, version.IsValid(minimum))
	require.True(t, version.IsValid("go"+toolchain))
	require.GreaterOrEqual(t, version.Compare("go"+toolchain, minimum), 0)
}

func TestImageResolver(t *testing.T) {
	for _, tool := range []string{"bash", "timeout"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("image workflow test requires %s: %v", tool, err)
		}
	}

	data, err := os.ReadFile("../../.github/workflows/images.yaml")
	require.NoError(t, err)

	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				ID  string `json:"id"`
				Run string `json:"run"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	require.NoError(t, yaml.Unmarshal(data, &workflow))

	var script string

	for _, step := range workflow.Jobs["resolve"].Steps {
		if step.ID == "resolve" {
			script = step.Run
		}
	}

	require.NotEmpty(t, script)

	for _, tc := range []struct {
		name      string
		files     []string
		event     string
		image     string
		platforms string
		expected  string
		status    int
	}{
		{"containerfile", []string{"Containerfile"}, "workflow_dispatch", "example", "", "Containerfile", 0},
		{"dockerfile", []string{"Dockerfile"}, "workflow_dispatch", "racer-object", "", "Dockerfile", 0},
		{"containerfile-first", []string{"Containerfile", "Dockerfile"}, "push", "racer-object", "", "Containerfile", 0},
		{"missing", nil, "workflow_dispatch", "missing", "", "", 1},
		{"playpen-skipped", nil, "push", "playpen-test", "", "", 0},
		{"explicit-platform", []string{"Dockerfile"}, "push", "racer-object", "linux/arm64", "Dockerfile", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()

			imageDir := filepath.Join(root, "images", tc.image)
			require.NoError(t, os.MkdirAll(imageDir, 0o755))

			for _, file := range tc.files {
				require.NoError(t, os.WriteFile(filepath.Join(imageDir, file), nil, 0o600))
			}

			output := filepath.Join(root, "output")
			require.NoError(t, os.WriteFile(output, nil, 0o600))

			rendered := strings.NewReplacer(
				"${{ github.event_name }}", tc.event,
				"${{ github.ref_name }}", "images/"+tc.image+"/v1",
				"${{ inputs.image }}", tc.image,
				"${{ github.sha }}", "test-sha",
			).Replace(script)
			cmd := exec.CommandContext(t.Context(), "timeout", "--signal=TERM", "--kill-after=10s", "60s", "bash", "-e", "-c", rendered)
			cmd.Dir = root

			cmd.Env = append(os.Environ(), "GITHUB_OUTPUT="+output, "INPUT_PLATFORMS="+tc.platforms)
			result, err := cmd.CombinedOutput()

			if tc.status == 0 {
				require.NoError(t, err, "%s", result)
			} else {
				var exitErr *exec.ExitError
				require.ErrorAs(t, err, &exitErr)
				require.Equal(t, tc.status, exitErr.ExitCode(), "%s", result)
			}

			data, err := os.ReadFile(output)
			require.NoError(t, err)

			values := make(map[string]string)

			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				if key, value, ok := strings.Cut(line, "="); ok {
					values[key] = value
				}
			}

			switch {
			case tc.expected != "":
				platforms := tc.platforms
				if platforms == "" {
					platforms = "linux/amd64,linux/arm64"
				}

				tag := "test-sha"
				if tc.event == "push" {
					tag = "v1"
				}

				require.Equal(t, map[string]string{
					"name": tc.image, "tag": tag,
					"file":         "images/" + tc.image + "/" + tc.expected,
					"should_build": "true", "platforms": platforms,
				}, values)
			case tc.status != 0:
				require.Contains(t, string(result), "No Containerfile or Dockerfile")
				require.Empty(t, values)
			default:
				require.Equal(t, map[string]string{"should_build": "false"}, values)
			}
		})
	}
}
