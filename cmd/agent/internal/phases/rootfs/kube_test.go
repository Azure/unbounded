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
			name:              "missing patch",
			kubernetesVersion: "1.32",
			wantErr:           true,
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

	got := crictlDownloadURL("1.30.4", "linux", "amd64")
	want := "https://github.com/kubernetes-sigs/cri-tools/releases/download/v1.30.4/crictl-v1.30.4-linux-amd64.tar.gz"
	if got != want {
		t.Fatalf("got URL %q, want %q", got, want)
	}
}
