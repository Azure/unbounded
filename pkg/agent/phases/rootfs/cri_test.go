// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"testing"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestContainerdDownloadURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		override *goalstates.DownloadSource
		version  string
		arch     string
		want     string
	}{
		{
			name:    "default",
			version: "2.0.4",
			arch:    "amd64",
			want:    "https://github.com/containerd/containerd/releases/download/v2.0.4/containerd-2.0.4-linux-amd64.tar.gz",
		},
		{
			name:     "base url override",
			override: &goalstates.DownloadSource{BaseURL: "https://mirror.example.com/containerd/"},
			version:  "2.0.4",
			arch:     "amd64",
			want:     "https://mirror.example.com/containerd/v2.0.4/containerd-2.0.4-linux-amd64.tar.gz",
		},
		{
			name:     "url override",
			override: &goalstates.DownloadSource{URL: "https://mirror.example.com/containerd-%s-%s-%s.tar.gz"},
			version:  "2.0.4",
			arch:     "amd64",
			want:     "https://mirror.example.com/containerd-2.0.4-2.0.4-amd64.tar.gz",
		},
	}

	for i := range tests {
		testCase := tests[i]
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := containerdDownloadURL(testCase.override, testCase.version, testCase.arch)
			if got != testCase.want {
				t.Fatalf("got URL %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestRuncDownloadURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		override *goalstates.DownloadSource
		version  string
		arch     string
		want     string
	}{
		{
			name:    "default",
			version: "1.1.12",
			arch:    "amd64",
			want:    "https://github.com/opencontainers/runc/releases/download/v1.1.12/runc.amd64",
		},
		{
			name:     "base url override",
			override: &goalstates.DownloadSource{BaseURL: "https://mirror.example.com/runc/"},
			version:  "1.1.12",
			arch:     "amd64",
			want:     "https://mirror.example.com/runc/v1.1.12/runc.amd64",
		},
	}

	for i := range tests {
		testCase := tests[i]
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := runcDownloadURL(testCase.override, testCase.version, testCase.arch)
			if got != testCase.want {
				t.Fatalf("got URL %q, want %q", got, testCase.want)
			}
		})
	}
}
