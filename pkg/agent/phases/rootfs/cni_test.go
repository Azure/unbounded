// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"testing"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestCNIDownloadURL(t *testing.T) {
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
			version: "1.5.1",
			arch:    "amd64",
			want:    "https://github.com/containernetworking/plugins/releases/download/v1.5.1/cni-plugins-linux-amd64-v1.5.1.tgz",
		},
		{
			name:     "base url override",
			override: &goalstates.DownloadSource{BaseURL: "https://mirror.example.com/cni/"},
			version:  "1.5.1",
			arch:     "amd64",
			want:     "https://mirror.example.com/cni/v1.5.1/cni-plugins-linux-amd64-v1.5.1.tgz",
		},
		{
			name:     "url override",
			override: &goalstates.DownloadSource{URL: "https://mirror.example.com/cni-%s-%s-%s.tgz"},
			version:  "1.5.1",
			arch:     "amd64",
			want:     "https://mirror.example.com/cni-1.5.1-amd64-1.5.1.tgz",
		},
	}

	for i := range tests {
		testCase := tests[i]
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := cniDownloadURL(testCase.override, testCase.version, testCase.arch)
			if got != testCase.want {
				t.Fatalf("got URL %q, want %q", got, testCase.want)
			}
		})
	}
}
