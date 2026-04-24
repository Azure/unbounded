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

	"github.com/Azure/unbounded/pkg/agent/goalstates"
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
		testCase := tests[i]
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			version, err := crictlVersionForKubernetesVersion(testCase.kubernetesVersion)
			if testCase.wantErr {
				if err == nil {
					t.Fatalf("expected an error for version %q", testCase.kubernetesVersion)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if version != testCase.expected {
				t.Fatalf("got version %q, want %q", version, testCase.expected)
			}
		})
	}
}

func TestCrictlDownloadURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		override *goalstates.DownloadSource
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
		{
			name:     "base url override",
			override: &goalstates.DownloadSource{BaseURL: "https://mirror.example.com/cri-tools"},
			version:  "1.30.4",
			hostOS:   "linux",
			hostArch: "amd64",
			want:     "https://mirror.example.com/cri-tools/v1.30.4/crictl-v1.30.4-linux-amd64.tar.gz",
		},
		{
			name:     "base url override strips trailing slash",
			override: &goalstates.DownloadSource{BaseURL: "https://mirror.example.com/cri-tools/"},
			version:  "1.30.4",
			hostOS:   "linux",
			hostArch: "amd64",
			want:     "https://mirror.example.com/cri-tools/v1.30.4/crictl-v1.30.4-linux-amd64.tar.gz",
		},
		{
			name:     "full url override",
			override: &goalstates.DownloadSource{URL: "https://mirror.example.com/crictl/%s/crictl-v%s-%s-%s.tgz"},
			version:  "1.30.4",
			hostOS:   "linux",
			hostArch: "amd64",
			want:     "https://mirror.example.com/crictl/1.30.4/crictl-v1.30.4-linux-amd64.tgz",
		},
	}

	for i := range tests {
		testCase := tests[i]
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := crictlDownloadURL(testCase.override, testCase.version, testCase.hostOS, testCase.hostArch)
			if got != testCase.want {
				t.Fatalf("got URL %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestResolveCrictlVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		override          *goalstates.DownloadSource
		kubernetesVersion string
		want              string
	}{
		{
			name:              "no override aligns to kubernetes minor",
			kubernetesVersion: "1.33.5",
			want:              "1.33.0",
		},
		{
			name:              "nil override version falls through",
			override:          &goalstates.DownloadSource{BaseURL: "https://mirror.example.com"},
			kubernetesVersion: "1.33.5",
			want:              "1.33.0",
		},
		{
			name:              "version override wins over minor alignment",
			override:          &goalstates.DownloadSource{Version: "1.34.1"},
			kubernetesVersion: "1.33.5",
			want:              "1.34.1",
		},
	}

	for i := range tests {
		testCase := tests[i]
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveCrictlVersion(testCase.override, testCase.kubernetesVersion)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got != testCase.want {
				t.Fatalf("got version %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestKubernetesBinaryURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		override *goalstates.DownloadSource
		version  string
		arch     string
		binary   string
		want     string
	}{
		{
			name:    "default",
			version: "1.33.5",
			arch:    "amd64",
			binary:  "kubelet",
			want:    "https://dl.k8s.io/v1.33.5/bin/linux/amd64/kubelet",
		},
		{
			name:     "base url override",
			override: &goalstates.DownloadSource{BaseURL: "https://mirror.example.com/k8s/"},
			version:  "1.33.5",
			arch:     "amd64",
			binary:   "kubelet",
			want:     "https://mirror.example.com/k8s/v1.33.5/bin/linux/amd64/kubelet",
		},
		{
			name:     "url override",
			override: &goalstates.DownloadSource{URL: "https://mirror.example.com/%s/%s/%s"},
			version:  "1.33.5",
			arch:     "amd64",
			binary:   "kubelet",
			want:     "https://mirror.example.com/1.33.5/amd64/kubelet",
		},
	}

	for i := range tests {
		testCase := tests[i]
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := kubernetesBinaryURL(testCase.override, testCase.version, testCase.arch, testCase.binary)
			if got != testCase.want {
				t.Fatalf("got URL %q, want %q", got, testCase.want)
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
