// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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

// TestMirror_StartupGate_Returns503UntilMarkReady covers the // startup gate added in the fix: when a Server is built
// with WithStartupReadinessGate, every /v2/ request returns 503
// (with Retry-After: 5) until MarkReady is called. Without the gate
// the mirror's TCP listener accepts traffic the moment ListenAndServe
// returns - well before members informer sync, DHT routing-table
// convergence, self-announce, and cache scan complete. Every
// startup-window pull would race those subsystems and route to origin
// instead of through the coordinated cold-start path, silently
// breaking the cache-hit 'one origin pull per digest' invariant for the
// duration of the rollout window.
//
// 503 is load-bearing in exactly the same way Drain's 503 is: it is
// the response status that lets containerd's hosts.toml mirror chain
// fall through to the next entry rather than failing the pull. 404
// or 5xx-without-Docker-Distribution-API-Version would not produce
// the same fall-through behavior.
func TestMirror_StartupGate_Returns503UntilMarkReady(t *testing.T) {
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

	m := mirror.New(cfg, c, oc, mirror.WithStartupReadinessGate())
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)

	d := digestOf([]byte("anything"))

	t.Run("before MarkReady -> 503 with API-Version header and Retry-After", func(t *testing.T) {
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
					t.Errorf("status = %d, want 503 (startup gate)", resp.StatusCode)
				}

				if v := resp.Header.Get("Docker-Distribution-API-Version"); v != "registry/2.0" {
					t.Errorf("Docker-Distribution-API-Version = %q, want registry/2.0", v)
				}

				if v := resp.Header.Get("Retry-After"); v != "5" {
					t.Errorf("Retry-After = %q, want 5 (containerd-friendly retry hint)", v)
				}
			})
		}
	})

	t.Run("after MarkReady -> /v2/ serves normally", func(t *testing.T) {
		m.MarkReady()

		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/", nil)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}

		defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

		if resp.StatusCode == http.StatusServiceUnavailable {
			t.Errorf("status = 503 after MarkReady; want a non-503 response (gate should be open)")
		}
	})

	t.Run("MarkReady is sticky across subsequent /readyz flap simulation", func(t *testing.T) {
		// MarkReady was already called above. Call it again to
		// document the sticky-monotonic contract: there is no
		// 'mark not ready' path, and a transient /readyz blip on
		// the operator side must not flip the mirror back into
		// 503. (Drain is the only documented way to put the
		// mirror back into 503 mode.)
		m.MarkReady()

		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/", nil)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}

		defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

		if resp.StatusCode == http.StatusServiceUnavailable {
			t.Errorf("status = 503 after second MarkReady; want non-503 (stickiness)")
		}
	})
}

// TestMirror_DefaultsReadyImmediately documents the test-friendly
// default: without WithStartupReadinessGate the Server serves /v2/
// from the moment Handler is wired. This is required for the
// existing test fixtures (mirror_test.go, mirror_coldstart_test.go,
// mirror_peer_test.go, mirror_prefetch_test.go, mirror_nf5_
// integration_test.go) to keep working unchanged.
func TestMirror_DefaultsReadyImmediately(t *testing.T) {
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

	m := mirror.New(cfg, c, oc) // no WithStartupReadinessGate
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/", nil)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode == http.StatusServiceUnavailable {
		t.Errorf("default mirror (no startup gate) returned 503; want non-503 (test fixtures depend on this)")
	}
}

// TestMirror_StartupGate_DrainBeatsReady asserts ordering: once Drain
// has fired the mirror returns 503 regardless of the startup gate
// state. The drainGuard wraps startupGate from the outside so a
// concurrent MarkReady cannot un-drain the server.
func TestMirror_StartupGate_DrainBeatsReady(t *testing.T) {
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

	m := mirror.New(cfg, c, oc, mirror.WithStartupReadinessGate())
	m.MarkReady()
	m.Drain()

	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/", nil)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (Drain must win over MarkReady)", resp.StatusCode)
	}
	// Drain's 503 body says "agent shutting down"; startup-gate's
	// says "agent starting up". We don't assert exact body but do
	// assert the Retry-After header is absent (Drain doesn't set
	// it; startupGate does).
	if v := resp.Header.Get("Retry-After"); v != "" {
		t.Errorf("Retry-After = %q after Drain; want empty (Drain semantics, not startup-gate semantics)", v)
	}
}
