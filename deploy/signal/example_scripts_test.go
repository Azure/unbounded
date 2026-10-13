// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signalmanifest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestExampleScriptsAreConfigMapDataAndParseAsBash(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("03-scripts.yaml.tmpl")
	if err != nil {
		t.Fatal(err)
	}

	rendered := strings.ReplaceAll(
		string(raw),
		`{{ default "project-signal" .Namespace }}`,
		"project-signal",
	)

	var manifest struct {
		Kind string            `yaml:"kind"`
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal([]byte(rendered), &manifest); err != nil {
		t.Fatalf("parse script ConfigMap: %v", err)
	}

	if manifest.Kind != "ConfigMap" || len(manifest.Data) == 0 {
		t.Fatalf("expected a ConfigMap containing example scripts, got %#v", manifest)
	}

	for name, script := range manifest.Data {
		name, script := name, script
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cmd := exec.Command("bash", "-n")
			cmd.Stdin = strings.NewReader(script)

			var stderr bytes.Buffer

			cmd.Stderr = &stderr

			if err := cmd.Run(); err != nil {
				t.Fatalf("bash -n: %v: %s", err, stderr.String())
			}
		})
	}
}

func TestNoStandaloneCertificationScripts(t *testing.T) {
	t.Parallel()

	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !info.IsDir() && strings.HasSuffix(info.Name(), ".sh") {
			t.Errorf("certification scripts must be ConfigMap data, found %s", path)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFullNodePreflightReturnsTypedInventoryMismatch(t *testing.T) {
	t.Parallel()

	script := certificationScript(t, "gpu-full-node.sh")
	tempDir := t.TempDir()

	resultsDir := filepath.Join(tempDir, "results")
	if err := os.Mkdir(resultsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	nvidiaSMI := filepath.Join(tempDir, "nvidia-smi")
	if err := os.WriteFile(nvidiaSMI, []byte("#!/usr/bin/env bash\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", "-c", strings.ReplaceAll(script, "/results/", resultsDir+"/"))

	cmd.Env = append(
		os.Environ(),
		"PATH="+tempDir+":"+os.Getenv("PATH"),
		"PROJECT_SIGNAL_STAGE_NAME=preflight",
		"EXPECTED_GPU_PRODUCT_REGEX=^NVIDIA B200$",
		"EXPECTED_GPU_COUNT=8",
		"OBSERVED_GPU_CAPACITY=7",
		"OBSERVED_GPU_ALLOCATABLE=7",
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("preflight returned infrastructure failure: %v: %s", err, output)
	}

	assertFileContents(t, filepath.Join(resultsDir, "verdict"), "Fail")
	assertFileContents(t, filepath.Join(resultsDir, "reason"), "NodeGPUCapacityMismatch")

	if !strings.Contains(string(output), `"name":"preflight","status":"failed"`) {
		t.Fatalf("missing typed preflight stage evidence: %s", output)
	}
}

func TestFullNodePreflightAcceptsExpectedB200Inventory(t *testing.T) {
	t.Parallel()

	script := certificationScript(t, "gpu-full-node.sh")
	tempDir := t.TempDir()
	resultsDir := filepath.Join(tempDir, "results")

	if err := os.Mkdir(resultsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	nvidiaSMI := filepath.Join(tempDir, "nvidia-smi")

	fake := `#!/usr/bin/env bash
if [[ "$*" == *"--query-gpu=name"* ]]; then
  printf '%s\n' 'NVIDIA B200'
else
  printf '%s\n' '0, NVIDIA B200, 00000000:01:00.0, P0, 31, 570.00'
fi
`
	if err := os.WriteFile(nvidiaSMI, []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", "-c", strings.ReplaceAll(script, "/results/", resultsDir+"/"))

	cmd.Env = append(
		os.Environ(),
		"PATH="+tempDir+":"+os.Getenv("PATH"),
		"PROJECT_SIGNAL_STAGE_NAME=preflight",
		"EXPECTED_GPU_PRODUCT_REGEX=^NVIDIA B200$",
		"EXPECTED_GPU_COUNT=8",
		"OBSERVED_GPU_CAPACITY=8",
		"OBSERVED_GPU_ALLOCATABLE=8",
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("preflight returned infrastructure failure: %v: %s", err, output)
	}

	assertFileContents(t, filepath.Join(resultsDir, "verdict"), "Pass")
	assertFileContents(t, filepath.Join(resultsDir, "reason"), "NodeInventoryHealthy")
}

func certificationScript(t *testing.T, name string) string {
	t.Helper()

	raw, err := os.ReadFile("03-scripts.yaml.tmpl")
	if err != nil {
		t.Fatal(err)
	}

	rendered := strings.ReplaceAll(
		string(raw),
		`{{ default "project-signal" .Namespace }}`,
		"project-signal",
	)

	var manifest struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal([]byte(rendered), &manifest); err != nil {
		t.Fatalf("parse script ConfigMap: %v", err)
	}

	script, ok := manifest.Data[name]
	if !ok {
		t.Fatalf("script %q not found", name)
	}

	return script
}

func assertFileContents(t *testing.T, path string, want string) {
	t.Helper()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}
