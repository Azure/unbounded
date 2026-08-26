// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package mirror_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
	"github.com/Azure/unbounded/internal/gantry/inflight"
	"github.com/Azure/unbounded/internal/gantry/mirror"
	"github.com/Azure/unbounded/internal/gantry/origin"
	"github.com/Azure/unbounded/internal/gantry/transfer"
)

// coldStartExhaustedStub forces the cold-start cascade to exit with
// ErrColdStartExhausted on every call. This emulates the
// precondition for direct-origin-fallback firing: all warm paths drained.
type coldStartExhaustedStub struct{ calls int32 }

func (s *coldStartExhaustedStub) Resolve(_ context.Context, _ digest.Digest, _ ifaces.OriginRefKind, _, _ string, _ int64) (*mirror.ColdStartResolution, error) {
	atomic.AddInt32(&s.calls, 1)
	return nil, mirror.ErrColdStartExhausted
}

// buildMirrorWithNF5 wires a mirror with a stub cold-start (returning
// ErrColdStartExhausted) and the supplied direct-origin-fallback controller. If `nf5` is
// nil, the path is disabled.
func buildMirrorWithNF5(t *testing.T, originBlobs map[digest.Digest][]byte, cs mirror.ColdStartResolver, nf5 *mirror.DirectOriginFallbackController) (*httptest.Server, *int32) {
	t.Helper()

	var originHits int32

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&originHits, 1)

		path := r.URL.Path
		ref := ""

		for _, sep := range []string{"/blobs/", "/manifests/"} {
			if idx := stringsLastIndex(path, sep); idx >= 0 {
				ref = path[idx+len(sep):]
				break
			}
		}

		d, err := digest.Parse(ref)
		if err != nil {
			w.WriteHeader(404)
			return
		}

		body, ok := originBlobs[d]
		if !ok {
			w.WriteHeader(404)
			return
		}

		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		_, _ = w.Write(body) //nolint:errcheck // best-effort write
	}))
	t.Cleanup(up.Close)

	cfg := &config.Config{
		UpstreamRegistries: []config.UpstreamRegistry{
			{Name: "reg.example.com", Endpoint: up.URL},
		},
	}
	c := fakes.NewCache()

	oc, err := origin.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	dht := fakes.NewDHT() // empty providers - forces cold-start path
	client := transfer.NewClient()

	opts := []mirror.Option{
		mirror.WithDiscovery(dht, client),
		mirror.WithColdStart(cs),
	}
	if nf5 != nil {
		opts = append(opts, mirror.WithNF5(nf5))
	}

	m := mirror.New(cfg, c, oc, opts...)
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)

	return srv, &originHits
}

// TestMirror_NF5_ColdStartExhaustedNoNF5_Returns503 confirms the
// default behavior: cold-start exhausted + no direct-origin-fallback wired -> 5xx.
func TestMirror_NF5_ColdStartExhaustedNoNF5_Returns503(t *testing.T) {
	body := []byte("origin would-have-served")
	d := digestOf(body)
	cs := &coldStartExhaustedStub{}

	srv, originHits := buildMirrorWithNF5(t, map[digest.Digest][]byte{d: body}, cs, nil)

	resp, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503 (NF5 disabled, no fallback)", resp.StatusCode)
	}

	if atomic.LoadInt32(originHits) != 0 {
		t.Errorf("origin hits = %d; want 0 (NF5 disabled must not origin-pull)", *originHits)
	}

	if atomic.LoadInt32(&cs.calls) != 1 {
		t.Errorf("cold-start calls = %d; want 1", cs.calls)
	}
}

// TestMirror_NF5_AllGatesPassServesFromOrigin is the happy path:
// cold-start exhausted + direct-origin-fallback fully permissive -> mirror pulls origin
// and returns 200. OnFallback fires exactly once.
func TestMirror_NF5_AllGatesPassServesFromOrigin(t *testing.T) {
	body := []byte("nf5 origin bytes")
	d := digestOf(body)
	cs := &coldStartExhaustedStub{}

	var fallbacks int32

	nf5 := mirror.NewDirectOriginFallback(mirror.DirectOriginFallbackOptions{
		Inflight:      inflight.New(inflight.DefaultStalls(), nil),
		InBootstrap:   func() bool { return false },
		HealthyEnough: func() bool { return true },
		ClusterSize:   func() int { return 1 }, // no jitter
		Recheck:       func(context.Context, digest.Digest) bool { return false },
		OnFallback:    func() { atomic.AddInt32(&fallbacks, 1) },
	})

	srv, originHits := buildMirrorWithNF5(t, map[digest.Digest][]byte{d: body}, cs, nf5)

	resp, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d; want 200 (NF5 should have served origin)", resp.StatusCode)
	}

	got, _ := io.ReadAll(resp.Body)
	if string(got) != string(body) {
		t.Errorf("body mismatch: got %q want %q", got, body)
	}

	if atomic.LoadInt32(originHits) != 1 {
		t.Errorf("origin hits = %d; want 1", *originHits)
	}

	if atomic.LoadInt32(&fallbacks) != 1 {
		t.Errorf("OnFallback fires = %d; want 1", fallbacks)
	}
}

// TestMirror_NF5_DeclinesUnderUnhealthyDHT_Returns503 covers the
// safety gate: when direct-origin-fallback reports the DHT is below the unhealthy
// threshold, the mirror must NOT origin-pull.
func TestMirror_NF5_DeclinesUnderUnhealthyDHT_Returns503(t *testing.T) {
	body := []byte("must-not-serve")
	d := digestOf(body)
	cs := &coldStartExhaustedStub{}

	var fallbacks int32

	nf5 := mirror.NewDirectOriginFallback(mirror.DirectOriginFallbackOptions{
		Inflight:      inflight.New(inflight.DefaultStalls(), nil),
		InBootstrap:   func() bool { return false },
		HealthyEnough: func() bool { return false }, // gate trips
		ClusterSize:   func() int { return 1 },
		OnFallback:    func() { atomic.AddInt32(&fallbacks, 1) },
	})

	srv, originHits := buildMirrorWithNF5(t, map[digest.Digest][]byte{d: body}, cs, nf5)

	resp, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503 (DHT unhealthy -> NF5 declines)", resp.StatusCode)
	}

	if atomic.LoadInt32(originHits) != 0 {
		t.Errorf("origin hits = %d; want 0", *originHits)
	}

	if atomic.LoadInt32(&fallbacks) != 0 {
		t.Errorf("OnFallback fires = %d; want 0", fallbacks)
	}
}

// TestMirror_NF5_BootstrapWindowSuppresses_Returns503 covers the
// bootstrap-window gate: while the DHT is still converging, direct-origin-fallback must
// decline so the cluster doesn't thunder the origin in the first 30s.
func TestMirror_NF5_BootstrapWindowSuppresses_Returns503(t *testing.T) {
	body := []byte("bootstrap-blocked")
	d := digestOf(body)
	cs := &coldStartExhaustedStub{}

	nf5 := mirror.NewDirectOriginFallback(mirror.DirectOriginFallbackOptions{
		Inflight:      inflight.New(inflight.DefaultStalls(), nil),
		InBootstrap:   func() bool { return true }, // gate trips
		HealthyEnough: func() bool { return true },
		ClusterSize:   func() int { return 1 },
		OnFallback:    func() { t.Fatalf("must not fire in bootstrap window") },
	})

	srv, originHits := buildMirrorWithNF5(t, map[digest.Digest][]byte{d: body}, cs, nf5)

	resp, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503 (bootstrap window)", resp.StatusCode)
	}

	if atomic.LoadInt32(originHits) != 0 {
		t.Errorf("origin hits = %d; want 0", *originHits)
	}
}

// TestMirror_NF5_RecheckHitAbortsAfterJitter covers the
// post-jitter recheck: if a provider materialized while direct-origin-fallback was
// sleeping, direct-origin-fallback declines and the request 5xxs (client retries through
// the warm path).
func TestMirror_NF5_RecheckHitAbortsAfterJitter(t *testing.T) {
	body := []byte("recheck-hit-blocked")
	d := digestOf(body)
	cs := &coldStartExhaustedStub{}

	nf5 := mirror.NewDirectOriginFallback(mirror.DirectOriginFallbackOptions{
		Inflight:      inflight.New(inflight.DefaultStalls(), nil),
		InBootstrap:   func() bool { return false },
		HealthyEnough: func() bool { return true },
		ClusterSize:   func() int { return 1 }, // no jitter - recheck still runs
		Recheck:       func(context.Context, digest.Digest) bool { return true },
		OnFallback:    func() { t.Fatalf("must not fire when recheck hits") },
	})

	srv, originHits := buildMirrorWithNF5(t, map[digest.Digest][]byte{d: body}, cs, nf5)

	// Use a context with a small timeout in case the call blocks.
	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503 (recheck hit aborts NF5)", resp.StatusCode)
	}

	if atomic.LoadInt32(originHits) != 0 {
		t.Errorf("origin hits = %d; want 0", *originHits)
	}
}
