// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import (
	"log/slog"
	"strings"
	"testing"
)

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

func TestRunCLinksLibpathrs(t *testing.T) {
	t.Parallel()

	tests := map[string]bool{
		"1.5.0":      true,
		"v1.5.0":     true,
		"1.5.0-rc.1": true,
		"1.6.2":      true,
		"2.0.0":      true,
		"1.4.3":      false,
		"v1.4.3":     false,
		"1.4":        false,
		"1.3.0":      false,
		"0.9.9":      false,
		"":           true,
		"latest":     true,
		"1.x.0":      true,
	}

	for version, want := range tests {
		if got := runcLinksLibpathrs(version); got != want {
			t.Errorf("runcLinksLibpathrs(%q) = %t, want %t", version, got, want)
		}
	}
}

func TestResolveRunCVersion(t *testing.T) {
	t.Parallel()

	const (
		mshvKernel    = "6.18.58.mshv1-1.azl3"
		regularKernel = "6.8.0-138-generic"
	)

	override := func(version string) *DownloadOverrides {
		return &DownloadOverrides{Runc: &DownloadSource{URL: "file:///bundle/runc/v%s/runc.%s", Version: version}}
	}

	tests := []struct {
		name       string
		kernel     string
		configured string
		downloads  *DownloadOverrides
		want       string
		wantErr    bool
	}{
		{name: "regular kernel default", kernel: regularKernel, want: RunCVersion},
		{name: "mshv kernel default falls back", kernel: mshvKernel, want: RunCLibpathrsFallbackVersion},
		{name: "mshv kernel with nil runc override falls back", kernel: mshvKernel, downloads: &DownloadOverrides{}, want: RunCLibpathrsFallbackVersion},
		{name: "mshv kernel with URL-only override falls back", kernel: mshvKernel, downloads: &DownloadOverrides{Runc: &DownloadSource{BaseURL: "https://mirror"}}, want: RunCLibpathrsFallbackVersion},
		{name: "mshv kernel rejects incompatible override", kernel: mshvKernel, downloads: override("1.5.0"), wantErr: true},
		{name: "mshv kernel rejects incompatible v-prefixed override", kernel: mshvKernel, downloads: override("v1.6.0"), wantErr: true},
		{name: "mshv kernel accepts compatible override", kernel: mshvKernel, downloads: override(RunCLibpathrsFallbackVersion), want: RunCLibpathrsFallbackVersion},
		{name: "regular kernel accepts libpathrs override", kernel: regularKernel, downloads: override("1.5.0"), want: RunCVersion},
		{name: "regular kernel accepts older override", kernel: regularKernel, downloads: override("1.4.3"), want: RunCVersion},
		{name: "configured pin on mshv kernel is honored", kernel: mshvKernel, configured: "1.5.0", want: "1.5.0"},
		{name: "configured pin with matching override on mshv kernel is honored", kernel: mshvKernel, configured: "1.5.0", downloads: override("1.5.0"), want: "1.5.0"},
		{name: "configured pin on regular kernel is honored", kernel: regularKernel, configured: "1.4.3", want: "1.4.3"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveRunCVersion(slog.New(slog.DiscardHandler), tt.kernel, tt.configured, tt.downloads)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveRunCVersion() = %q, want error", got)
				}

				if !strings.Contains(err.Error(), tt.kernel) || !strings.Contains(err.Error(), RunCLibpathrsFallbackVersion) {
					t.Errorf("error %q should name kernel and fallback version", err)
				}

				return
			}

			if err != nil {
				t.Fatalf("resolveRunCVersion() error = %v", err)
			}

			if got != tt.want {
				t.Errorf("resolveRunCVersion() = %q, want %q", got, tt.want)
			}
		})
	}
}
