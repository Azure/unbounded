// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import "testing"

func TestCrictlVersionForKubernetesVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		kubernetesVersion string
		expected          string
		wantErr           bool
	}{
		{
			name:              "exact semver",
			kubernetesVersion: "1.30.4",
			expected:          "1.30.4",
		},
		{
			name:              "leading v prefix",
			kubernetesVersion: "v1.31.2",
			expected:          "1.31.2",
		},
		{
			name:              "prerelease suffix",
			kubernetesVersion: "1.32.0-rc.1",
			expected:          "1.32.0",
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

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			version, err := crictlVersionForKubernetesVersion(tt.kubernetesVersion)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error for version %q", tt.kubernetesVersion)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if version != tt.expected {
				t.Fatalf("got version %q, want %q", version, tt.expected)
			}
		})
	}
}

func TestCrictlDownloadURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		version string
		hostOS  string
		hostArch string
		want    string
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

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := crictlDownloadURL(tt.version, tt.hostOS, tt.hostArch)
			if got != tt.want {
				t.Fatalf("got URL %q, want %q", got, tt.want)
			}
		})
	}
}
