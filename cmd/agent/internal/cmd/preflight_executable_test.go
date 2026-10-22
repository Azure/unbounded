// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/preflight"
)

func TestPreflightExecutableJSONFailure(t *testing.T) {
	root, err := filepath.Abs("../../../..")
	require.NoError(t, err)

	dir := t.TempDir()
	pciDir := filepath.Join(dir, "pci")
	deviceDir := filepath.Join(dir, "dev")
	device := filepath.Join(pciDir, "0000:00:00.0")
	require.NoError(t, os.MkdirAll(device, 0o755))
	require.NoError(t, os.MkdirAll(deviceDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(device, "vendor"), []byte("0x10de\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(device, "class"), []byte("0x030200\n"), 0o644))

	// Redirect only hardware discovery paths in the test build. Keep the actual
	// executable entry point, NVIDIA query, report writer, and error handler.
	sourcePath := filepath.Join(root, "pkg/agent/phases/host/preflight_nvidia.go")
	source, err := os.ReadFile(sourcePath)
	require.NoError(t, err)

	sourceText := string(source)
	for original, replacement := range map[string]string{
		`pciDevicesDir: "/sys/bus/pci/devices"`: "pciDevicesDir: " + fmt.Sprintf("%q", pciDir),
		`devDir:        "/dev"`:                 "devDir: " + fmt.Sprintf("%q", deviceDir),
	} {
		require.Contains(t, sourceText, original)
		sourceText = strings.Replace(sourceText, original, replacement, 1)
	}

	overlaySource := filepath.Join(dir, "preflight_nvidia.go")
	require.NoError(t, os.WriteFile(overlaySource, []byte(sourceText), 0o644))
	overlay, err := json.Marshal(struct {
		Replace map[string]string
	}{Replace: map[string]string{sourcePath: overlaySource}})
	require.NoError(t, err)

	overlayPath := filepath.Join(dir, "overlay.json")
	require.NoError(t, os.WriteFile(overlayPath, overlay, 0o644))

	binary := filepath.Join(dir, "agent")
	build := exec.CommandContext(t.Context(), "go", "build", "-overlay", overlayPath, "-o", binary, "./cmd/agent")
	build.Dir = root
	buildOutput, err := build.CombinedOutput()
	require.NoError(t, err, "%s", buildOutput)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "nvidia-smi"),
		[]byte("#!/bin/sh\nprintf 'No devices found.\\n'\n"), 0o755))

	srv := preflightTestServer(t)
	t.Cleanup(srv.Close)

	for _, ignore := range []bool{false, true} {
		t.Run(fmt.Sprintf("ignoreNVIDIA=%t", ignore), func(t *testing.T) {
			cfg := preflightConfig(srv.URL)
			cfg.Kubelet.ApiServer = "https://127.0.0.1:1"
			cfg.MachineName = "preflight-json-" + filepath.Base(dir)
			configPath := writeConfigFile(t, cfg)

			args := []string{"preflight", "--config", configPath, "--output", "json"}
			if ignore {
				args = append(args, "--ignore-preflight-errors=nvidia-driver")
			}

			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()

			command := exec.CommandContext(ctx, binary, args...)

			command.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))

			var stdout, stderr bytes.Buffer

			command.Stdout = &stdout
			command.Stderr = &stderr
			err := command.Run()

			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr, "stderr: %s", stderr.String())
			require.Equal(t, 1, exitErr.ExitCode(), "stdout: %s\nstderr: %s", stdout.String(), stderr.String())

			var report preflight.Report

			require.NoError(t, json.Unmarshal(stdout.Bytes(), &report), "stdout: %s\nstderr: %s", stdout.String(), stderr.String())
			require.Contains(t, stderr.String(), "error: preflight checks failed")
			require.Equal(t, "failed", report.Status)

			var foundGPU, foundOtherFatal bool

			for _, result := range report.Checks {
				if result.Name == "nvidia-driver" && result.Target == "NVIDIA diagnostic tooling" {
					foundGPU = true

					require.Contains(t, result.Message, "NVIDIA GPUs are not ready")
					require.Equal(t, ignore, result.Ignored)

					if ignore {
						require.Equal(t, preflight.SeverityWarning, result.Severity)
					} else {
						require.Equal(t, preflight.SeverityError, result.Severity)
					}
				}

				if result.Name == "api-server-reachable" && result.Severity == preflight.SeverityError {
					foundOtherFatal = true
				}
			}

			require.True(t, foundGPU, "missing NVIDIA readiness result")
			require.True(t, foundOtherFatal, "missing unrelated fatal result")
		})
	}
}
