// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestContainerBuilderImage(t *testing.T) {
	builder := regexp.MustCompile(`(?m)^FROM --platform=\$BUILDPLATFORM (docker\.io/library/golang:\S+) AS builder$`)
	images := make(map[string]string)

	for _, name := range []string{"gantry", "racer-object"} {
		path := filepath.Join("..", "..", "images", name, "Containerfile")

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}

		match := builder.FindSubmatch(data)
		if len(match) != 2 {
			t.Fatalf("%s: missing Go builder image", path)
		}

		images[name] = string(match[1])
	}

	if images["racer-object"] != images["gantry"] {
		t.Fatalf("racer-object builder = %q, want %q to match gantry", images["racer-object"], images["gantry"])
	}
}

func TestLocalImageTrivyDryRun(t *testing.T) {
	for _, trivy := range []string{"", "   ", "1", "0"} {
		t.Run("TRIVY="+trivy, func(t *testing.T) {
			cmd := exec.Command("timeout", "--signal=TERM", "--kill-after=10s", "30s",
				"make", "--no-print-directory", "-n", "image-racer-object-local",
				"VERSION=test", "GIT_COMMIT=test", "BUILD_TIME=test", "CONTAINER_ENGINE=podman",
				"RACER_OBJECT_IMAGE=example.test/racer-object:custom", "TRIVY="+trivy,
				"TRIVY_IMAGE=aquasec/trivy:test", "TRIVY_SEVERITY=CRITICAL,HIGH", "TRIVY_EXIT_CODE=1")
			cmd.Dir = filepath.Join("..", "..")

			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("make dry run: %v\n%s", err, output)
			}

			text := string(output)
			build := strings.Index(text, "podman build")

			save := strings.Index(text, "podman image save -o $tmp example.test/racer-object:custom")
			if build < 0 || !strings.Contains(text, "-t example.test/racer-object:custom") {
				t.Fatalf("missing image build or tag:\n%s", text)
			}

			if strings.TrimSpace(trivy) == "" {
				if strings.Contains(text, "image save") || strings.Contains(text, "podman run") {
					t.Fatalf("scan enabled for blank TRIVY:\n%s", text)
				}
			} else if save <= build || !strings.Contains(text, "podman run --rm") ||
				!strings.Contains(text, "aquasec/trivy:test image --severity CRITICAL,HIGH --exit-code 1") {
				t.Fatalf("missing scan after build:\n%s", text)
			}
		})
	}
}

func TestLocalImageTrivyFailure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		engine    string
		scan      string
		wantError bool
		wantScan  bool
	}{
		{name: "success", engine: "true", scan: "true", wantScan: true},
		{name: "scan failure", engine: "true", scan: "false", wantError: true, wantScan: true},
		{name: "build failure", engine: "false", scan: "true", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("timeout", "--signal=TERM", "--kill-after=10s", "30s",
				"make", "--no-print-directory", "-s", "image-racer-object-local",
				"VERSION=test", "GIT_COMMIT=test", "BUILD_TIME=test", "TRIVY=1",
				"RACER_OBJECT_IMAGE=example.test/racer-object:custom", "CONTAINER_ENGINE="+tc.engine,
				"TRIVY_SCAN_CMD=printf 'scanned %s\\n' '$(1)'; "+tc.scan)
			cmd.Dir = filepath.Join("..", "..")

			output, err := cmd.CombinedOutput()
			if (err != nil) != tc.wantError {
				t.Fatalf("make error = %v, want error %t\n%s", err, tc.wantError, output)
			}

			if scanned := strings.Contains(string(output), "scanned example.test/racer-object:custom"); scanned != tc.wantScan {
				t.Fatalf("scan ran = %t, want %t\n%s", scanned, tc.wantScan, output)
			}
		})
	}
}
