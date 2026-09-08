// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
	"github.com/Azure/unbounded/internal/gantry/mirror"
	"github.com/Azure/unbounded/internal/gantry/origin"
	"github.com/Azure/unbounded/internal/gantry/transfer"
)

// TestMirror_Rediscover_PicksUpFinisherMidSwarm proves the re-discovery loop:
// a node that misses the cache when NO provider is advertised yet must keep
// re-running FindProviders and pick up a finisher-seed that advertises later,
// serving from the peer instead of falling to origin.
func TestMirror_Rediscover_PicksUpFinisherMidSwarm(t *testing.T) {
	body := []byte("bytes that arrive from a finisher-seed mid-swarm")
	d := digestOf(body)

	// Origin serves the blob but counts hits: the whole point is that we must
	// NOT touch it because the re-discovery loop finds the peer first.
	var originHits int32

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&originHits, 1)

		ref := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]

		dg, err := digest.Parse(ref)
		if err != nil || dg != d {
			w.WriteHeader(http.StatusNotFound)
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

	oc, err := origin.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// A real peer transfer server that already holds the blob (the finisher).
	peerCache := fakes.NewCache()
	peerCache.Put(d, body)
	peerAddr := startPeerTransfer(t, peerCache)

	// DHT starts EMPTY: no provider is advertised when the request arrives.
	dht := fakes.NewDHT()

	var peerFetches int32

	client := transfer.NewClient(transfer.WithDialTimeout(time.Second), transfer.WithRequestTimeout(5*time.Second))
	m := mirror.New(cfg, fakes.NewCache(), oc,
		mirror.WithDiscovery(dht, client),
		mirror.WithPeerBudgets(500*time.Millisecond, 5*time.Second, 3),
		mirror.WithPeerRediscover(3*time.Second, 100*time.Millisecond),
		mirror.WithPeerMetrics(
			func(outcome string) {
				if outcome == "hit" {
					atomic.AddInt32(&peerFetches, 1)
				}
			},
			nil,
		),
	)
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)

	// The finisher advertises into the DHT only after the request is already
	// spinning in its re-discovery loop.
	go func() {
		time.Sleep(300 * time.Millisecond)
		dht.Inject(d, ifaces.Provider{NodeID: "finisher", Addr: peerAddr})
	}()

	resp, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	got, _ := io.ReadAll(resp.Body)
	if string(got) != string(body) {
		t.Errorf("body mismatch: got %q, want %q", got, body)
	}

	if n := atomic.LoadInt32(&originHits); n != 0 {
		t.Errorf("origin hits = %d, want 0 (re-discovery should have served from the finisher)", n)
	}

	if n := atomic.LoadInt32(&peerFetches); n != 1 {
		t.Errorf("peer fetches = %d, want 1", n)
	}
}

// A cold-exhausted round 0 has no busy peer to trigger the capacity headers,
// so without an explicit flush the client sees nothing while re-discovery
// runs. Fail-open containerd gives up at its own 30s response-header timeout
// and pulls the layer straight from origin, even though a seed typically
// advertises seconds later. Headers must therefore be flushed before the
// re-discovery loop, and the late provider must still fill the body.
func TestMirror_Rediscover_ColdExhaustedFlushesHeadersBeforeLateProvider(t *testing.T) {
	body := []byte("late seed advertises after cold-start exhaustion")
	d := digestOf(body)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("origin method = %s, want HEAD only", r.Method)
		}

		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)

	cfg := &config.Config{UpstreamRegistries: []config.UpstreamRegistry{{Name: "reg.example.com", Endpoint: up.URL}}}

	oc, err := origin.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	const lateAddr = "late-seed:5001"

	dialer := newCountingPeerDialer()
	dialer.Put(lateAddr, d, body)

	dht := fakes.NewDHT()
	coldStart := &stubColdStart{err: mirror.ErrColdStartExhausted}

	m := mirror.New(cfg, fakes.NewCache(), oc,
		mirror.WithLiveStreamThrough(),
		mirror.WithDiscovery(dht, dialer),
		mirror.WithColdStart(coldStart),
		mirror.WithPeerBudgets(time.Second, time.Second, 20),
		mirror.WithPeerRediscover(5*time.Millisecond, 3*time.Second),
	)
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)

	// Returns as soon as the headers are flushed; no provider exists yet.
	resp, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 flushed before any provider existed", resp.StatusCode)
	}

	if got := resp.ContentLength; got != int64(len(body)) {
		t.Fatalf("Content-Length = %d, want %d", got, len(body))
	}

	if got := dialer.Calls(lateAddr); got != 0 {
		t.Fatalf("peer calls before advertise = %d, want 0", got)
	}

	dht.Inject(d, ifaces.Provider{NodeID: "late-seed", Addr: lateAddr})

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if !bytes.Equal(got, body) {
		t.Fatalf("body = %q, want %q", got, body)
	}

	if calls := atomic.LoadInt32(&coldStart.calls); calls != 1 {
		t.Fatalf("cold-start calls = %d, want 1 (round 0 only)", calls)
	}
}

// The re-discovery loop is silent whenever a round never reaches a peer: no
// provider means no peer fetch line, and cold-start is suppressed after round
// 0. A round 0 that resolves without providers therefore emits nothing at all
// while the loop spins, which is the majority of the observed fail-open
// bypasses. Headers must be flushed for that path too, not just for the
// cold-exhausted one that happens to log.
func TestMirror_Rediscover_SilentNoProviderRoundFlushesHeaders(t *testing.T) {
	body := []byte("no provider on round 0 and nothing logged at all")
	d := digestOf(body)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("origin method = %s, want HEAD only", r.Method)
		}

		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)

	cfg := &config.Config{UpstreamRegistries: []config.UpstreamRegistry{{Name: "reg.example.com", Endpoint: up.URL}}}

	oc, err := origin.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	const lateAddr = "late-seed:5001"

	dialer := newCountingPeerDialer()
	dialer.Put(lateAddr, d, body)

	dht := fakes.NewDHT()
	// Resolves cleanly with no providers, so round 0 is neither busy nor
	// cold-exhausted and logs nothing.
	coldStart := &stubColdStart{}

	m := mirror.New(cfg, fakes.NewCache(), oc,
		mirror.WithLiveStreamThrough(),
		mirror.WithDiscovery(dht, dialer),
		mirror.WithColdStart(coldStart),
		mirror.WithPeerBudgets(time.Second, time.Second, 20),
		mirror.WithPeerRediscover(5*time.Millisecond, 3*time.Second),
	)
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 flushed before any provider existed", resp.StatusCode)
	}

	if got := resp.ContentLength; got != int64(len(body)) {
		t.Fatalf("Content-Length = %d, want %d", got, len(body))
	}

	if got := dialer.Calls(lateAddr); got != 0 {
		t.Fatalf("peer calls before advertise = %d, want 0", got)
	}

	dht.Inject(d, ifaces.Provider{NodeID: "late-seed", Addr: lateAddr})

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if !bytes.Equal(got, body) {
		t.Fatalf("body = %q, want %q", got, body)
	}
}

// Re-discovery disabled must keep the terminal legs reachable: no headers are
// flushed, so an unused round 0 still falls through to the direct origin pull.
func TestMirror_Rediscover_DisabledStillFallsThroughToOrigin(t *testing.T) {
	body := []byte("served by gantry direct origin when rediscovery is off")
	d := digestOf(body)

	var originGets atomic.Int32

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		w.Header().Set("Content-Type", "application/octet-stream")

		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}

		originGets.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body) //nolint:errcheck // test server best-effort write
	}))
	t.Cleanup(up.Close)

	cfg := &config.Config{UpstreamRegistries: []config.UpstreamRegistry{{Name: "reg.example.com", Endpoint: up.URL}}}

	oc, err := origin.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	m := mirror.New(cfg, fakes.NewCache(), oc,
		mirror.WithLiveStreamThrough(),
		mirror.WithDiscovery(fakes.NewDHT(), newCountingPeerDialer()),
		mirror.WithColdStart(&stubColdStart{}),
		mirror.WithPeerBudgets(time.Second, time.Second, 20),
	)
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, body) {
		t.Fatalf("status = %d body = %q, want 200 and %q", resp.StatusCode, got, body)
	}

	if n := originGets.Load(); n != 1 {
		t.Fatalf("origin GETs = %d, want 1 direct origin pull", n)
	}
}

// Round 0's cold-start leg polls for the designated puller up to ResolveStall,
// which for a 1 GiB layer is 60s against the 30s response-header timeout
// fail-open containerd applies. Headers must therefore be flushed before round
// 0 runs, not after it returns. This drives a real ResponseHeaderTimeout
// against a cold-start that blocks, reproducing the production bypass.
func TestMirror_Rediscover_HeadersFlushedBeforeBlockingColdStart(t *testing.T) {
	body := []byte("cold-start polls longer than the client will wait")
	d := digestOf(body)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("origin method = %s, want HEAD only", r.Method)
		}

		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)

	cfg := &config.Config{UpstreamRegistries: []config.UpstreamRegistry{{Name: "reg.example.com", Endpoint: up.URL}}}

	oc, err := origin.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	gate := make(chan struct{})

	var once sync.Once

	release := func() { once.Do(func() { close(gate) }) }

	const lateAddr = "late-seed:5001"

	dialer := newCountingPeerDialer()
	dialer.Put(lateAddr, d, body)

	dht := fakes.NewDHT()
	// Stands in for pollDHT waiting on the designated puller.
	coldStart := &stubColdStart{onResolve: func(digest.Digest) { <-gate }}

	m := mirror.New(cfg, fakes.NewCache(), oc,
		mirror.WithLiveStreamThrough(),
		mirror.WithDiscovery(dht, dialer),
		mirror.WithColdStart(coldStart),
		mirror.WithPeerBudgets(time.Second, time.Second, 20),
		mirror.WithPeerRediscover(5*time.Millisecond, 3*time.Second),
	)
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)
	// Registered after srv.Close so LIFO cleanup unblocks the handler first.
	t.Cleanup(release)

	// Mirrors containerd: give up if response headers do not arrive in time.
	client := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 2 * time.Second}}

	resp, err := client.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatalf("no response headers while cold-start was still polling: %v", err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if got := resp.ContentLength; got != int64(len(body)) {
		t.Fatalf("Content-Length = %d, want %d", got, len(body))
	}

	dht.Inject(d, ifaces.Provider{NodeID: "late-seed", Addr: lateAddr})
	release()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if !bytes.Equal(got, body) {
		t.Fatalf("body = %q, want %q", got, body)
	}
}
