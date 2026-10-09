// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func releaseWorkflowStep(t *testing.T, job, name string) string {
	t.Helper()

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH")
	}

	data, err := os.ReadFile("../../.github/workflows/release.yaml")
	if err != nil {
		t.Fatal(err)
	}

	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string
				Run  string
			}
		}
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}

	var script string

	for _, step := range workflow.Jobs[job].Steps {
		if step.Name == name {
			script = step.Run
		}
	}

	if script == "" {
		t.Fatalf("step %q missing in job %q", name, job)
	}

	return script
}

func TestGantryChartPushAndSign(t *testing.T) {
	script := releaseWorkflowStep(t, "gantry-chart", "Push and sign Gantry chart")

	digest := "sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		name   string
		output string
		fail   bool
	}{
		{name: "stderr digest", output: "printf 'Pushed: chart\\nDigest: " + digest + "\\n' >&2"},
		{name: "stdout digest", output: "printf 'Digest: " + digest + "\\n'"},
		{name: "missing digest", output: "echo 'Pushed: chart' >&2", fail: true},
		{name: "invalid digest", output: "echo 'Digest: sha256:invalid' >&2", fail: true},
		{name: "failed push with digest", output: "echo 'Digest: " + digest + "' >&2; echo 'registry push denied' >&2; exit 7", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()

			bin := filepath.Join(dir, "bin")
			if err := os.Mkdir(bin, 0o755); err != nil {
				t.Fatal(err)
			}

			for name, contents := range map[string]string{
				"helm":   "#!/usr/bin/env bash\n" + tc.output + "\n",
				"cosign": "#!/usr/bin/env bash\nprintf '%s\\n' \"$*\" >> \"$SIGN_LOG\"\n",
			} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte(contents), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			log := filepath.Join(dir, "sign.log")
			cmd := exec.Command("bash", "-c", script)
			cmd.Dir = dir

			cmd.Env = append(os.Environ(),
				"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"TAG=v0.9.0", "REGISTRY=ghcr.io/azure", "SIGN_LOG="+log)

			output, err := cmd.CombinedOutput()
			if (err != nil) != tc.fail {
				t.Fatalf("push and sign error = %v, output:\n%s", err, output)
			}

			if tc.name == "failed push with digest" {
				if !strings.Contains(string(output), "registry push denied") {
					t.Fatalf("push failure diagnostic was lost: %s", output)
				}

				exit, ok := err.(*exec.ExitError)
				if !ok || exit.ExitCode() != 7 {
					t.Fatalf("push failure exit code was not preserved: %v", err)
				}
			}

			calls, err := os.ReadFile(log)
			if tc.fail {
				if !os.IsNotExist(err) {
					t.Fatalf("failed push or digest validation invoked signing: %s, %v", calls, err)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			want := "sign --yes ghcr.io/azure/charts/gantry@" + digest + "\n" +
				"sign-blob --yes --bundle=build/charts/gantry-0.9.0.tgz.bundle.json build/charts/gantry-0.9.0.tgz\n"
			if string(calls) != want {
				t.Fatalf("sign calls:\n%s\nwant:\n%s", calls, want)
			}

			if !strings.Contains(string(output), digest) {
				t.Fatalf("push digest not retained in log: %s", output)
			}
		})
	}
}
