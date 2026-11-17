// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/pkg/racersdk"
)

func TestPageSizeFromEnv(t *testing.T) {
	for _, tc := range []struct {
		name       string
		gantry     string
		fallback   string
		want       int
		wantErr    bool
		errorMatch string
	}{
		{name: "unset"},
		{name: "gantry bytes", gantry: "4194304", want: 4 << 20},
		{name: "gantry suffix", gantry: "4MiB", want: 4 << 20},
		{name: "fallback bytes", fallback: "8192", want: 8 << 10},
		{name: "fallback suffix", fallback: "8MiB", want: 8 << 20},
		{name: "gantry wins", gantry: "4096", fallback: "8388608", want: 4 << 10},
		{name: "empty gantry falls back", gantry: "  ", fallback: "8192", want: 8 << 10},
		{name: "not power of two", gantry: "3MiB", wantErr: true, errorMatch: PageSizeEnv},
		{name: "below minimum", gantry: "2048", wantErr: true, errorMatch: "power of two"},
		{name: "above maximum", gantry: "32MiB", wantErr: true, errorMatch: "power of two"},
		{name: "garbage", fallback: "large", wantErr: true, errorMatch: PageSizeFallbackEnv},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(PageSizeEnv, tc.gantry)
			t.Setenv(PageSizeFallbackEnv, tc.fallback)

			got, err := PageSizeFromEnv()
			if tc.wantErr {
				if err == nil || tc.errorMatch == "" || !bytes.Contains([]byte(err.Error()), []byte(tc.errorMatch)) {
					t.Fatalf("PageSizeFromEnv() = %d, %v; want error containing %q", got, err, tc.errorMatch)
				}

				return
			}

			if err != nil || got != tc.want {
				t.Fatalf("PageSizeFromEnv() = %d, %v; want %d", got, err, tc.want)
			}
		})
	}
}

func TestParsePageSize(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
		ok    bool
	}{
		{"4096", 4 << 10, true},
		{"4KiB", 4 << 10, true},
		{"4kib", 4 << 10, true},
		{"16384", 16 << 10, true},
		{"1MiB", 1 << 20, true},
		{"8 MiB", 8 << 20, true},
		{"16MiB", 16 << 20, true},
		{"0", 0, false},
		{"-4096", 0, false},
		{"2048", 0, false},
		{"3072", 0, false},
		{"3MiB", 0, false},
		{"32MiB", 0, false},
		{"16777217", 0, false},
		{"4.5MiB", 0, false},
		{"", 0, false},
		{"large", 0, false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			got, err := parsePageSize(tc.value)
			if tc.ok && (err != nil || got != tc.want) {
				t.Fatalf("parsePageSize(%q) = %d, %v; want %d", tc.value, got, err, tc.want)
			}

			if !tc.ok && err == nil {
				t.Fatalf("parsePageSize(%q) = %d; want error", tc.value, got)
			}
		})
	}
}

func TestGantryRacerConfigsUseEnvPageSize(t *testing.T) {
	const pageSize = 4 << 20

	t.Setenv(PageSizeEnv, "4MiB")
	t.Setenv(PageSizeFallbackEnv, "")

	if got := ClientConfig(&config.Config{}, CacheName).PageSize; got != pageSize {
		t.Fatalf("ClientConfig PageSize = %d; want %d", got, pageSize)
	}

	if got := OriginConfig(&config.Config{}, CacheName).PageSize; got != pageSize {
		t.Fatalf("OriginConfig PageSize = %d; want %d", got, pageSize)
	}

	if got := NewHandler(nil, nil, nil).pageSize; got != pageSize {
		t.Fatalf("NewHandler pageSize = %d; want %d", got, pageSize)
	}
}

func TestInvalidEnvPageSizeFailsSDKConfig(t *testing.T) {
	t.Setenv(PageSizeEnv, "3MiB")
	t.Setenv(PageSizeFallbackEnv, "")

	client, err := racersdk.NewClient(ClientConfig(&config.Config{}, CacheName))
	if err == nil {
		_ = client.Close()

		t.Fatal("NewClient accepted an invalid page size")
	}

	if !bytes.Contains([]byte(err.Error()), []byte("PageSize")) {
		t.Fatalf("NewClient error = %v; want PageSize validation", err)
	}
}

func TestWithPageSizeIgnoresInvalidValue(t *testing.T) {
	t.Setenv(PageSizeEnv, "")
	t.Setenv(PageSizeFallbackEnv, "")

	handler := NewHandler(nil, nil, nil, WithPageSize(3<<10))
	if handler.pageSize != racersdk.PageSize {
		t.Fatalf("invalid WithPageSize changed page size to %d", handler.pageSize)
	}
}

func TestManifestLaneUsesConfiguredPageSize(t *testing.T) {
	const pageSize = 4 << 10

	for _, tc := range []struct {
		name  string
		size  int
		small bool
	}{
		{name: "fits one page", size: pageSize - 1, small: true},
		{name: "larger than one page", size: pageSize + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := bytes.Repeat([]byte("m"), tc.size)
			d := racerDigest(data)
			client := &racerTranscriptClient{
				Client:  racerFakeClient(t, racerPageOrigin(t, d, data)),
				options: make(chan []racersdk.ReadOptions, 1),
			}

			ref := ifaces.OriginRef{Registry: "registry.example", Repository: "library/image", Digest: d, Kind: ifaces.KindManifest}
			w := httptest.NewRecorder()
			NewHandler(client, nil, nil, WithPageSize(pageSize)).ServeContent(w, httptest.NewRequest(http.MethodGet, "/", nil), ref)

			if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), data) {
				t.Fatalf("manifest response: status=%d bytes=%d; want 200/%d", w.Code, w.Body.Len(), len(data))
			}

			if client.stats.Load() != 1 || client.gets.Load() != 1 {
				t.Fatalf("manifest transcript: Stat=%d Get=%d; want 1/1", client.stats.Load(), client.gets.Load())
			}

			options := <-client.options

			want := racersdk.ReadOptions{SmallObject: tc.small}
			if len(options) != 1 || options[0] != want {
				t.Fatalf("manifest options = %+v; want %+v", options, want)
			}
		})
	}
}
