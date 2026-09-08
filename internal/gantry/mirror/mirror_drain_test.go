// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
	"github.com/Azure/unbounded/internal/gantry/mirror"
	"github.com/Azure/unbounded/internal/gantry/origin"
)

// TestMirror_DrainReturns503 verifies the graceful-shutdown
// contract: once Drain has been called, every /v2/ request gets a
// 503 immediately so containerd's hosts.toml falls through to origin.
// The 503 (not 404) is load-bearing per the design doc.
func TestMirror_DrainReturns503(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	cfg := &config.Config{
		UpstreamRegistries: []config.UpstreamRegistry{
			{Name: "reg.example.com", Endpoint: upstream.URL},
		},
	}
	c := fakes.NewCache()

	oc, err := origin.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	m := mirror.New(cfg, c, oc)
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)

	// Pre-drain: a digest request misses cache and would normally
	// hit upstream. We only check status semantics here, so any
	// non-503 result is fine.
	d := digestOf([]byte("anything"))
	preReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/repo/blobs/"+d.String(), nil)

	preResp, err := http.DefaultClient.Do(preReq)
	if err != nil {
		t.Fatalf("pre-drain request failed: %v", err)
	}

	_ = preResp.Body.Close() //nolint:errcheck // best-effort body close
	if preResp.StatusCode == http.StatusServiceUnavailable {
		t.Fatalf("pre-drain returned 503 unexpectedly; mirror should serve normally before Drain()")
	}

	// Flip into drain mode.
	m.Drain()

	cases := []struct {
		name string
		path string
	}{
		{"manifest by tag", "/v2/repo/manifests/v1"},
		{"manifest by digest", "/v2/repo/manifests/" + d.String()},
		{"blob by digest", "/v2/repo/blobs/" + d.String()},
		{"v2 root", "/v2/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, srv.URL+tc.path, nil)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}

			defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503 (Drain mode)", resp.StatusCode)
			}
			// The 503 path must still advertise the OCI Distribution
			// API version so containerd's mirror chain recognizes
			// the response as a registry (and falls through).
			if v := resp.Header.Get("Docker-Distribution-API-Version"); v != "registry/2.0" {
				t.Errorf("Docker-Distribution-API-Version = %q, want registry/2.0", v)
			}
		})
	}
}

// TestMirror_DrainIdempotent verifies Drain can be safely called
// more than once (signal-handler robustness).
func TestMirror_DrainIdempotent(t *testing.T) {
	cfg := &config.Config{
		UpstreamRegistries: []config.UpstreamRegistry{
			{Name: "reg.example.com", Endpoint: "http://example.invalid"},
		},
	}
	c := fakes.NewCache()

	oc, err := origin.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	m := mirror.New(cfg, c, oc)

	m.Drain()
	m.Drain()
	m.Drain()

	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)

	d := digestOf([]byte("anything"))
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/repo/blobs/"+d.String(), nil)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}
