// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signalmanifest

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestGPUIdentityExamplePreservesOriginalBytes(t *testing.T) {
	t.Parallel()

	script := certificationScript(t, "gpu-identity-example.sh")
	assert.Equal(t, "e8387f7e6b31f2007d8284c606dd37ebc21de886f334c1a59b765bb264bcdb5b",
		fmt.Sprintf("%x", sha256.Sum256([]byte(script))))
}

func TestGPUIdentityExampleReturnsTypedResults(t *testing.T) {
	t.Parallel()

	bash, err := exec.LookPath("bash")
	require.NoError(t, err)
	grep, err := exec.LookPath("grep")
	require.NoError(t, err)
	script := certificationScript(t, "gpu-identity-example.sh")

	for _, tc := range []struct {
		name, productRegex, count, products, verdict, reason string
		nvidiaMissing                                        bool
		nvidiaExit                                           int
	}{
		{name: "approved identity", productRegex: "^NVIDIA B200$", count: "1", products: "NVIDIA B200\n", verdict: "Pass", reason: "ExampleCheckPassed"},
		{name: "missing product guard", count: "1", verdict: "Fail", reason: "ProductGuardNotConfigured"},
		{name: "placeholder product guard", productRegex: "^REPLACE_WITH_VERIFIED_PRODUCT$", count: "1", verdict: "Fail", reason: "ProductGuardNotConfigured"},
		{name: "missing count guard", productRegex: "^NVIDIA B200$", verdict: "Fail", reason: "CountGuardNotConfigured"},
		{name: "zero count guard", productRegex: "^NVIDIA B200$", count: "0", verdict: "Fail", reason: "CountGuardNotConfigured"},
		{name: "negative count guard", productRegex: "^NVIDIA B200$", count: "-1", verdict: "Fail", reason: "CountGuardNotConfigured"},
		{name: "noninteger count guard", productRegex: "^NVIDIA B200$", count: "1.0", verdict: "Fail", reason: "CountGuardNotConfigured"},
		{name: "leading zero count guard", productRegex: "^NVIDIA B200$", count: "01", verdict: "Fail", reason: "CountGuardNotConfigured"},
		{name: "missing nvidia-smi", productRegex: "^NVIDIA B200$", count: "1", nvidiaMissing: true, verdict: "Fail", reason: "NvidiaSMIMissing"},
		{name: "empty GPU inventory", productRegex: "^NVIDIA B200$", count: "1", verdict: "Fail", reason: "GPUCountMismatch"},
		{name: "failed GPU query", productRegex: "^NVIDIA B200$", count: "1", nvidiaExit: 17, verdict: "Fail", reason: "GPUCountMismatch"},
		{name: "unexpected count", productRegex: "^NVIDIA B200$", count: "1", products: "NVIDIA B200\nNVIDIA B200\n", verdict: "Fail", reason: "GPUCountMismatch"},
		{name: "unexpected product", productRegex: "^NVIDIA B200$", count: "1", products: "NVIDIA H100\n", verdict: "Fail", reason: "GPUProductMismatch"},
		{name: "mixed products", productRegex: "^NVIDIA B200$", count: "2", products: "NVIDIA B200\nNVIDIA H100\n", verdict: "Fail", reason: "GPUProductMismatch"},
		{name: "invalid product regex", productRegex: "[", count: "1", products: "NVIDIA B200\n", verdict: "Fail", reason: "GPUProductMismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			directory := t.TempDir()
			results := filepath.Join(directory, "results")
			bin := filepath.Join(directory, "bin")
			terminationLog := filepath.Join(directory, "termination-log")

			require.NoError(t, os.Mkdir(results, 0o755))
			require.NoError(t, os.Mkdir(bin, 0o755))
			require.NoError(t, os.Symlink(grep, filepath.Join(bin, "grep")))

			if !tc.nvidiaMissing {
				require.NoError(t, os.WriteFile(filepath.Join(bin, "nvidia-smi"),
					[]byte("#!/bin/sh\nprintf '%s' \"$SIGNAL_TEST_PRODUCTS\"\nexit \"$SIGNAL_TEST_NVIDIA_EXIT\"\n"), 0o755))
			}

			body := strings.ReplaceAll(script, "/results/", shellQuote(results)+"/")
			body = strings.ReplaceAll(body, "/dev/termination-log", shellQuote(terminationLog))
			command := exec.Command(bash, "-c", body)

			command.Env = append(
				filteredIdentityEnvironment(),
				"PATH="+bin,
				"SIGNAL_TEST_PRODUCTS="+tc.products,
				fmt.Sprintf("SIGNAL_TEST_NVIDIA_EXIT=%d", tc.nvidiaExit),
			)

			if tc.productRegex != "" {
				command.Env = append(command.Env, "EXPECTED_GPU_PRODUCT_REGEX="+tc.productRegex)
			}

			if tc.count != "" {
				command.Env = append(command.Env, "EXPECTED_GPU_COUNT="+tc.count)
			}

			output, err := command.CombinedOutput()
			require.NoError(t, err, "a hardware failure must remain a typed result: %s", output)
			assertFileContents(t, filepath.Join(results, "verdict"), tc.verdict)
			assertFileContents(t, filepath.Join(results, "reason"), tc.reason)
			assertFileContents(t, filepath.Join(results, "measurements.txt"), strings.TrimSuffix(tc.products, "\n")+"\n")
			assertFileContents(t, terminationLog, fmt.Sprintf("verdict=%s reason=%s\n", tc.verdict, tc.reason))
			assert.Contains(t, string(output), fmt.Sprintf("PROJECT_SIGNAL_EXAMPLE verdict=%s reason=%s", tc.verdict, tc.reason))
		})
	}
}

func certificationScript(t *testing.T, name string) string {
	t.Helper()

	content, err := os.ReadFile("03-scripts.yaml.tmpl")
	require.NoError(t, err)

	var manifest struct {
		Data map[string]string `yaml:"data"`
	}

	rendered := strings.ReplaceAll(string(content), `{{ default "project-signal" .Namespace }}`, "project-signal")
	require.NoError(t, yaml.Unmarshal([]byte(rendered), &manifest))
	require.Contains(t, manifest.Data, name)

	return manifest.Data[name]
}

func assertFileContents(t *testing.T, path, expected string) {
	t.Helper()

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, expected, string(content), path)
}

func filteredIdentityEnvironment() []string {
	var environment []string

	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "EXPECTED_GPU_PRODUCT_REGEX=") && !strings.HasPrefix(entry, "EXPECTED_GPU_COUNT=") {
			environment = append(environment, entry)
		}
	}

	return environment
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
