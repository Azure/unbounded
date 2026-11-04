// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import "testing"

func TestLibpathrsParsesKernelRelease(t *testing.T) {
	t.Parallel()

	tests := []struct {
		release string
		want    bool
	}{
		{release: "6.8.0-138-generic", want: true},
		{release: "6.6.159.1-2.azl3", want: true},
		{release: "6.18.58", want: true},
		{release: "5.15", want: true},
		{release: "6.18.58+", want: true},
		{release: "6.18.58.mshv1-1.azl3", want: false},
		{release: "6.18.58.", want: false},
		{release: "6..18", want: false},
		{release: ".6.18", want: false},
		{release: "6", want: false},
		{release: "6-generic", want: false},
		{release: "", want: false},
		{release: "linux-6.8", want: false},
		{release: "6.99999999999999999999", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.release, func(t *testing.T) {
			t.Parallel()

			if got := libpathrsParsesKernelRelease(tt.release); got != tt.want {
				t.Errorf("libpathrsParsesKernelRelease(%q) = %v, want %v", tt.release, got, tt.want)
			}
		})
	}
}

func TestDefaultRunCVersion(t *testing.T) {
	t.Parallel()

	if got := defaultRunCVersion("6.8.0-138-generic"); got != RunCVersion {
		t.Errorf("defaultRunCVersion(parseable) = %q, want %q", got, RunCVersion)
	}

	if got := defaultRunCVersion("6.18.58.mshv1-1.azl3"); got != RunCLibpathrsFallbackVersion {
		t.Errorf("defaultRunCVersion(mshv) = %q, want %q", got, RunCLibpathrsFallbackVersion)
	}

	if got := defaultRunCVersion(""); got != RunCLibpathrsFallbackVersion {
		t.Errorf("defaultRunCVersion(empty) = %q, want %q", got, RunCLibpathrsFallbackVersion)
	}
}
