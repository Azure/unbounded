// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestCrictlVersionForKubernetesVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		kubernetesVersion string
		expected          string
		wantErr           bool
	}{
		{
			name:              "exact semver maps to minor patch zero",
			kubernetesVersion: "1.30.4",
			expected:          "1.30.0",
		},
		{
			name:              "leading v prefix maps to minor patch zero",
			kubernetesVersion: "v1.31.2",
			expected:          "1.31.0",
		},
		{
			name:              "prerelease suffix",
			kubernetesVersion: "1.32.0-rc.1",
			expected:          "1.32.0",
		},
		{
			name:              "non zero patch maps to zero",
			kubernetesVersion: "1.33.1",
			expected:          "1.33.0",
		},
		{
			name:              "missing patch defaults to zero",
			kubernetesVersion: "1.32",
			expected:          "1.32.0",
		},
		{
			name:              "invalid version",
			kubernetesVersion: "abc",
			wantErr:           true,
		},
	}

	for i := range tests {
		t.Run(tests[i].name, func(t *testing.T) {
			version, err := crictlVersionForKubernetesVersion(tests[i].kubernetesVersion)
			if tests[i].wantErr {
				if err == nil {
					t.Fatalf("expected an error for version %q", tests[i].kubernetesVersion)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if version != tests[i].expected {
				t.Fatalf("got version %q, want %q", version, tests[i].expected)
			}
		})
	}
}

func TestCrictlDownloadURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		version  string
		hostOS   string
		hostArch string
		want     string
	}{
		{
			name:     "linux amd64",
			version:  "1.30.4",
			hostOS:   "linux",
			hostArch: "amd64",
			want:     "https://github.com/kubernetes-sigs/cri-tools/releases/download/v1.30.4/crictl-v1.30.4-linux-amd64.tar.gz",
		},
		{
			name:     "darwin arm64",
			version:  "1.30.4",
			hostOS:   "darwin",
			hostArch: "arm64",
			want:     "https://github.com/kubernetes-sigs/cri-tools/releases/download/v1.30.4/crictl-v1.30.4-darwin-arm64.tar.gz",
		},
		{
			name:     "windows amd64",
			version:  "1.30.4",
			hostOS:   "windows",
			hostArch: "amd64",
			want:     "https://github.com/kubernetes-sigs/cri-tools/releases/download/v1.30.4/crictl-v1.30.4-windows-amd64.tar.gz",
		},
	}

	for i := range tests {
		t.Run(tests[i].name, func(t *testing.T) {
			got := crictlDownloadURL(tests[i].version, tests[i].hostOS, tests[i].hostArch)
			if got != tests[i].want {
				t.Fatalf("got URL %q, want %q", got, tests[i].want)
			}
		})
	}
}

func TestCrictlVersionMatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		crictlOutput    string
		expectedVersion string
		installBinary   bool
		wantMatch       bool
	}{
		{
			name:            "matching version",
			crictlOutput:    "crictl version v1.35.0",
			expectedVersion: "1.35.0",
			installBinary:   true,
			wantMatch:       true,
		},
		{
			name:            "mismatched version",
			crictlOutput:    "crictl version v1.35.1",
			expectedVersion: "1.35.0",
			installBinary:   true,
			wantMatch:       false,
		},
		{
			name:            "invalid output format",
			crictlOutput:    "v1.35.0",
			expectedVersion: "1.35.0",
			installBinary:   true,
			wantMatch:       false,
		},
		{
			name:            "missing crictl binary",
			expectedVersion: "1.35.0",
			installBinary:   false,
			wantMatch:       false,
		},
	}

	for i := range tests {
		t.Run(tests[i].name, func(t *testing.T) {
			destDir := t.TempDir()

			if tests[i].installBinary {
				crictlScript := "#!/usr/bin/env sh\necho '" + tests[i].crictlOutput + "'\n"
				if err := os.WriteFile(filepath.Join(destDir, "crictl"), []byte(crictlScript), 0o755); err != nil {
					t.Fatalf("write test crictl binary: %v", err)
				}
			}

			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			got := crictlVersionMatch(context.Background(), logger, destDir, tests[i].expectedVersion)
			if got != tests[i].wantMatch {
				t.Fatalf("got match=%t, want %t", got, tests[i].wantMatch)
			}
		})
	}
}
