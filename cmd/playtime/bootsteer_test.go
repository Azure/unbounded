// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBootStateSetGet(t *testing.T) {
	t.Parallel()

	var s bootState

	if httpBoot, uri := s.get(); httpBoot || uri != "" {
		t.Fatalf("zero bootState: got httpBoot=%v uri=%q, want false/empty", httpBoot, uri)
	}

	s.set(true, "http://example.com/boot.efi")

	if httpBoot, uri := s.get(); !httpBoot || uri != "http://example.com/boot.efi" {
		t.Fatalf("after set: got httpBoot=%v uri=%q", httpBoot, uri)
	}

	s.set(false, "")

	if httpBoot, uri := s.get(); httpBoot || uri != "" {
		t.Fatalf("after clear: got httpBoot=%v uri=%q", httpBoot, uri)
	}
}

func TestDeriveHTTPBoot(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name            string
		target, enabled string
		uri             string
		want            bool
	}{
		{"active continuous", "UefiHttp", "Continuous", "http://x/boot.efi", true},
		{"active once mixed case", "uefihttp", "Once", "http://x/boot.efi", true},
		{"disabled", "UefiHttp", "Disabled", "http://x/boot.efi", false},
		{"wrong target pxe", "Pxe", "Continuous", "http://x/boot.efi", false},
		{"missing uri", "UefiHttp", "Continuous", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := deriveHTTPBoot(tc.target, tc.enabled, tc.uri); got != tc.want {
				t.Fatalf("deriveHTTPBoot(%q,%q,%q)=%v, want %v", tc.target, tc.enabled, tc.uri, got, tc.want)
			}
		})
	}
}

func TestBootProxyForwardsToBootServer(t *testing.T) {
	t.Parallel()

	var gotPath string

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path

		_, _ = w.Write([]byte("BOOTIMAGE"))
	}))
	defer backend.Close()

	state := &bootState{}
	state.set(true, backend.URL+"/images/boot.efi")

	proxy := httptest.NewServer(newBootProxy(state))
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/images/boot.efi") //nolint:noctx // test
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if gotPath != "/images/boot.efi" {
		t.Fatalf("backend saw path %q, want /images/boot.efi", gotPath)
	}
}
