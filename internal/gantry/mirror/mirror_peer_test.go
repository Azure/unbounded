// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package mirror_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c" //nolint:staticcheck // h2c deliberate

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
	"github.com/Azure/unbounded/internal/gantry/mirror"
	"github.com/Azure/unbounded/internal/gantry/origin"
	"github.com/Azure/unbounded/internal/gantry/transfer"
)

type failAfterReader struct {
	reader    *bytes.Reader
	remaining int
}

func (r *failAfterReader) Read(buffer []byte) (int, error) {
	if r.remaining == 0 {
		return 0, context.DeadlineExceeded
	}

	if len(buffer) > r.remaining {
		buffer = buffer[:r.remaining]
	}

	n, err := r.reader.Read(buffer)
	r.remaining -= n

	return n, err
}

func (r *failAfterReader) Close() error { return nil }

type resumingPeerDialer struct {
	mu      sync.Mutex
	body    []byte
	offsets []int64
}

func (d *resumingPeerDialer) FetchFromPeer(_ context.Context, _ string, ref ifaces.OriginRef) (io.ReadCloser, int64, error) {
	d.mu.Lock()
	d.offsets = append(d.offsets, ref.Offset)
	call := len(d.offsets)
	d.mu.Unlock()

	if call == 1 {
		return &failAfterReader{reader: bytes.NewReader(d.body), remaining: 11}, int64(len(d.body)), nil
	}

	return io.NopCloser(bytes.NewReader(d.body[ref.Offset:])), int64(len(d.body)), nil
}

func (d *resumingPeerDialer) Offsets() []int64 {
	d.mu.Lock()
	defer d.mu.Unlock()

	return append([]int64(nil), d.offsets...)
}

// startPeerTransfer stands up a real :5001-style h2c transfer server on an
// ephemeral loopback port backed by the given Cache and returns its
// "host:port" address.
func startPeerTransfer(t *testing.T, c ifaces.LocalContentStore) string {
	t.Helper()

	s := transfer.New(c)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	h2s := &http2.Server{}

	hsrv := &http.Server{
		Handler:           h2c.NewHandler(s.Handler(), h2s), //nolint:staticcheck // h2c deliberate
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() { _ = hsrv.Serve(ln) }() //nolint:errcheck // best-effort

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		_ = hsrv.Shutdown(ctx) //nolint:errcheck // best-effort
		_ = ln.Close()         //nolint:errcheck // best-effort close
	})

	return ln.Addr().String()
}

func newMirrorWithPeer(t *testing.T, originBlobs map[digest.Digest][]byte, providers map[digest.Digest][]ifaces.Provider) (*httptest.Server, *fakes.Cache, *fakes.DHT, *int32, *int32) {
	t.Helper()

	var originHits int32

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&originHits, 1)

		path := r.URL.Path

		var refStart int

		switch {
		case strings.Contains(path, "/blobs/"):
			refStart = strings.LastIndex(path, "/blobs/") + len("/blobs/")
		case strings.Contains(path, "/manifests/"):
			refStart = strings.LastIndex(path, "/manifests/") + len("/manifests/")
		default:
			w.WriteHeader(404)
			return
		}

		ref := path[refStart:]

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

	dht := fakes.NewDHT()
	for d, provs := range providers {
		dht.Inject(d, provs...)
	}

	var peerFetches int32

	client := transfer.NewClient(transfer.WithDialTimeout(time.Second), transfer.WithRequestTimeout(5*time.Second))
	m := mirror.New(cfg, c, oc,
		mirror.WithDiscovery(dht, client),
		mirror.WithPeerBudgets(2*time.Second, 5*time.Second, 3),
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

	return srv, c, dht, &originHits, &peerFetches
}

func TestMirror_PeerFallback_ServesFromPeerNotOrigin(t *testing.T) {
	body := []byte("the canonical bytes for this digest")
	d := digestOf(body)

	peerCache := fakes.NewCache()
	peerCache.Put(d, body)
	peerAddr := startPeerTransfer(t, peerCache)

	srv, _, dht, originHits, peerFetches := newMirrorWithPeer(
		t,
		map[digest.Digest][]byte{d: body},
		map[digest.Digest][]ifaces.Provider{d: {{NodeID: "peer-a", Addr: peerAddr}}},
	)

	resp, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	got, _ := io.ReadAll(resp.Body)
	if string(got) != string(body) {
		t.Errorf("body mismatch: got %q want %q", got, body)
	}

	if atomic.LoadInt32(originHits) != 0 {
		t.Errorf("origin hits = %d, want 0 (should have served from peer)", *originHits)
	}

	if atomic.LoadInt32(peerFetches) != 1 {
		t.Errorf("peer fetches = %d, want 1", *peerFetches)
	}
	// the step 7: after a successful peer fetch the digest MUST be
	// re-advertised to the DHT so the provider set grows. Provide is
	// fire-and-forget in a goroutine; poll up to 2s.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if dht.ProvideCount(d) >= 1 {
			break
		}

		time.Sleep(20 * time.Millisecond)
	}

	if got := dht.ProvideCount(d); got < 1 {
		t.Errorf("dht.Provide call count = %d, want >= 1 (post-peer-fetch re-advertise)", got)
	}
}

func TestMirror_PeerFallback_LiveStreamResumesFromAnotherPeer(t *testing.T) {
	body := []byte("one complete digest assembled across two peer streams")
	d := digestOf(body)
	dialer := &resumingPeerDialer{body: body}
	dht := fakes.NewDHT()
	dht.Inject(d,
		ifaces.Provider{NodeID: "peer-a", Addr: "10.0.0.1:5001"},
		ifaces.Provider{NodeID: "peer-b", Addr: "10.0.0.2:5001"},
	)

	cfg, originSrc := newMirrorOriginNotFound(t)

	var hits, stalls int32

	m := mirror.New(cfg, &writerSpyCache{}, originSrc,
		mirror.WithLiveStreamThrough(),
		mirror.WithDiscovery(dht, dialer),
		mirror.WithPeerBudgets(time.Second, time.Second, 2),
		mirror.WithPeerMetrics(func(outcome string) {
			switch outcome {
			case "hit":
				atomic.AddInt32(&hits, 1)
			case "stall":
				atomic.AddInt32(&stalls, 1)
			}
		}, nil),
	)
	ts := httptest.NewServer(m.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	if !bytes.Equal(got, body) {
		t.Fatalf("body = %q, want %q", got, body)
	}

	if offsets := dialer.Offsets(); len(offsets) != 2 || offsets[0] != 0 || offsets[1] != 11 {
		t.Fatalf("peer offsets = %v, want [0 11]", offsets)
	}

	if got := atomic.LoadInt32(&stalls); got != 1 {
		t.Fatalf("stall outcomes = %d, want 1", got)
	}

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("hit outcomes = %d, want 1", got)
	}
}

func TestMirror_PeerFallback_NoProvidersFallsThroughToOrigin(t *testing.T) {
	body := []byte("only at origin")
	d := digestOf(body)

	srv, _, _, originHits, peerFetches := newMirrorWithPeer(
		t,
		map[digest.Digest][]byte{d: body},
		nil,
	)

	resp, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	got, _ := io.ReadAll(resp.Body)
	if string(got) != string(body) {
		t.Errorf("body mismatch: got %q want %q", got, body)
	}

	if atomic.LoadInt32(originHits) != 1 {
		t.Errorf("origin hits = %d, want 1", *originHits)
	}

	if atomic.LoadInt32(peerFetches) != 0 {
		t.Errorf("peer fetches = %d, want 0", *peerFetches)
	}
}

func TestMirror_PeerFallback_PeerNotFoundExhaustsWarmPath(t *testing.T) {
	body := []byte("origin-only")
	d := digestOf(body)

	// Peer cache is empty but DHT says peer has it (stale DHT record).
	peerCache := fakes.NewCache()
	peerAddr := startPeerTransfer(t, peerCache)

	srv, _, _, originHits, peerFetches := newMirrorWithPeer(
		t,
		map[digest.Digest][]byte{d: body},
		map[digest.Digest][]ifaces.Provider{d: {{NodeID: "peer-stale", Addr: peerAddr}}},
	)

	resp, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close
	// the design doc v1 transfer policy: warm path exhausted -> 5xx, NOT origin pull.
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (warm path exhausted)", resp.StatusCode)
	}

	if atomic.LoadInt32(originHits) != 0 {
		t.Errorf("origin hits = %d, want 0 (the design: containerd handles origin via hosts.toml)", *originHits)
	}

	if atomic.LoadInt32(peerFetches) != 0 {
		t.Errorf("peer hits = %d, want 0", *peerFetches)
	}
}

func TestMirror_PeerFallback_StaleProviderFilteredOnNextRequest(t *testing.T) {
	body := []byte("origin-after-stale-filter")
	d := digestOf(body)

	peerCache := fakes.NewCache() // empty: peer will return 404 for d
	peerAddr := startPeerTransfer(t, peerCache)

	srv, _, _, originHits, _ := newMirrorWithPeer(
		t,
		map[digest.Digest][]byte{d: body},
		map[digest.Digest][]ifaces.Provider{d: {{NodeID: "peer-stale", Addr: peerAddr}}},
	)

	resp1, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	_ = resp1.Body.Close() //nolint:errcheck // best-effort body close
	if resp1.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("first status = %d, want 503", resp1.StatusCode)
	}

	resp2, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp2.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d, want 200 (stale provider should be filtered)", resp2.StatusCode)
	}

	if atomic.LoadInt32(originHits) != 1 {
		t.Errorf("origin hits = %d, want 1", *originHits)
	}
}

func TestMirror_PeerFallback_FiltersSelfProviderAfterLocalMiss(t *testing.T) {
	body := []byte("served-by-non-self-provider")
	d := digestOf(body)

	peerCache := fakes.NewCache()
	peerCache.Put(d, body)
	peerAddr := startPeerTransfer(t, peerCache)

	var originHits int32

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&originHits, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(up.Close)

	cfg := &config.Config{
		UpstreamRegistries: []config.UpstreamRegistry{{Name: "reg.example.com", Endpoint: up.URL}},
	}
	c := fakes.NewCache()

	oc, err := origin.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	dht := fakes.NewDHT()
	dht.Inject(d,
		ifaces.Provider{NodeID: "self-node", Addr: "127.0.0.1:1"},
		ifaces.Provider{NodeID: "self-peer-id", Addr: "127.0.0.1:2"},
		ifaces.Provider{NodeID: "peer-good", Addr: peerAddr},
	)

	var peerHits int32

	m := mirror.New(cfg, c, oc,
		mirror.WithDiscovery(dht, transfer.NewClient()),
		mirror.WithSelfNodeID("self-node"),
		mirror.WithSelfPeerID("self-peer-id"),
		mirror.WithPeerBudgets(time.Second, 2*time.Second, 1),
		mirror.WithPeerMetrics(func(outcome string) {
			if outcome == "hit" {
				atomic.AddInt32(&peerHits, 1)
			}
		}, nil),
	)
	ts := httptest.NewServer(m.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if atomic.LoadInt32(&peerHits) != 1 {
		t.Errorf("peer hits = %d, want 1", peerHits)
	}

	if atomic.LoadInt32(&originHits) != 0 {
		t.Errorf("origin hits = %d, want 0", originHits)
	}
}

func TestMirror_PeerFallback_DialFailureExhaustsWarmPath(t *testing.T) {
	body := []byte("unreachable-peer")
	d := digestOf(body)

	// Provide an unreachable peer addr (port 1 is reliably refused).
	dht := fakes.NewDHT()
	dht.Inject(d, ifaces.Provider{NodeID: "peer-dead", Addr: "127.0.0.1:1"})

	var originHits int32

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&originHits, 1)
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

	var dialFailures int32

	client := transfer.NewClient(transfer.WithDialTimeout(200*time.Millisecond), transfer.WithRequestTimeout(2*time.Second))
	m := mirror.New(cfg, c, oc,
		mirror.WithDiscovery(dht, client),
		mirror.WithPeerBudgets(time.Second, time.Second, 3),
		mirror.WithPeerMetrics(nil, func(success bool) {
			if !success {
				atomic.AddInt32(&dialFailures, 1)
			}
		}),
	)
	ts := httptest.NewServer(m.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close
	// the design doc v1 transfer policy: warm path exhausted -> 5xx.
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (warm path exhausted)", resp.StatusCode)
	}

	if atomic.LoadInt32(&originHits) != 0 {
		t.Errorf("origin hits = %d, want 0 (the design: agent must NOT origin-pull after warm-path exhaustion)", originHits)
	}

	if atomic.LoadInt32(&dialFailures) == 0 {
		t.Error("expected at least one dial failure metric")
	}
}

// nolint:unused // referenced by helpers below
var errCantHappen = errors.New("test scaffolding error")

// peerServingPoison stands up a peer transfer server that returns bytes
// which do NOT hash to the requested digest, so the mirror's verifier
// must classify the result as a digest mismatch and quarantine the peer.
func peerServingPoison(t *testing.T, d digest.Digest, poison []byte) string {
	t.Helper()

	peerCache := fakes.NewCache()
	peerCache.Put(d, poison) // content-addressed store serves these bytes verbatim for d

	return startPeerTransfer(t, peerCache)
}

func newMirrorOriginNotFound(t *testing.T) (*config.Config, ifaces.OriginPuller) {
	t.Helper()

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(up.Close)

	cfg := &config.Config{
		UpstreamRegistries: []config.UpstreamRegistry{{Name: "reg.example.com", Endpoint: up.URL}},
	}

	oc, err := origin.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	return cfg, oc
}

// TestMirror_PeerFallback_LiveStreamDigestMismatchQuarantines asserts the
// live stream-through path classifies a corrupt peer (digestpipe mismatch)
// as digest_mismatch, which is the branch that quarantines the provider.
func TestMirror_PeerFallback_LiveStreamDigestMismatchQuarantines(t *testing.T) {
	d := digestOf([]byte("the-bytes-this-digest-stands-for"))
	poison := []byte("poison-bytes-that-do-not-match-the-digest")

	peerAddr := peerServingPoison(t, d, poison)
	cfg, oc := newMirrorOriginNotFound(t)

	dht := fakes.NewDHT()
	dht.Inject(d, ifaces.Provider{NodeID: "poison-peer", Addr: peerAddr})

	var digestMismatches int32

	m := mirror.New(cfg, fakes.NewCache(), oc,
		mirror.WithLiveStreamThrough(),
		mirror.WithDiscovery(dht, transfer.NewClient()),
		mirror.WithPeerBudgets(time.Second, 2*time.Second, 1),
		mirror.WithPeerMetrics(func(outcome string) {
			if outcome == "digest_mismatch" {
				atomic.AddInt32(&digestMismatches, 1)
			}
		}, nil),
	)
	ts := httptest.NewServer(m.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	_, _ = io.Copy(io.Discard, resp.Body) //nolint:errcheck // best-effort drain for server-side Verify
	_ = resp.Body.Close()                 //nolint:errcheck // best-effort body close

	if got := atomic.LoadInt32(&digestMismatches); got != 1 {
		t.Fatalf("digest_mismatch outcomes (live path) = %d, want 1", got)
	}
}

// TestMirror_PeerFallback_NonLiveCommitDigestMismatchQuarantines asserts the
// non-live (write-then-commit) path classifies a corrupt peer via the
// containerd-style commit failure (wrapped errdefs.ErrFailedPrecondition) as
// digest_mismatch, the provider-quarantine branch. This is the path the old
// substring match silently misclassified.
func TestMirror_PeerFallback_NonLiveCommitDigestMismatchQuarantines(t *testing.T) {
	d := digestOf([]byte("other-bytes-this-digest-stands-for"))
	poison := []byte("different-poison-bytes-that-do-not-match")

	peerAddr := peerServingPoison(t, d, poison)
	cfg, oc := newMirrorOriginNotFound(t)

	dht := fakes.NewDHT()
	dht.Inject(d, ifaces.Provider{NodeID: "poison-peer", Addr: peerAddr})

	var digestMismatches int32

	// No WithLiveStreamThrough: the mirror writes to its content store and
	// commits, so the fake store's Commit returns a wrapped
	// errdefs.ErrFailedPrecondition exactly like real containerd.
	m := mirror.New(cfg, fakes.NewCache(), oc,
		mirror.WithDiscovery(dht, transfer.NewClient()),
		mirror.WithPeerBudgets(time.Second, 2*time.Second, 1),
		mirror.WithPeerMetrics(func(outcome string) {
			if outcome == "digest_mismatch" {
				atomic.AddInt32(&digestMismatches, 1)
			}
		}, nil),
	)
	ts := httptest.NewServer(m.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	_, _ = io.Copy(io.Discard, resp.Body) //nolint:errcheck // best-effort drain
	_ = resp.Body.Close()                 //nolint:errcheck // best-effort body close

	if got := atomic.LoadInt32(&digestMismatches); got != 1 {
		t.Fatalf("digest_mismatch outcomes (non-live path) = %d, want 1", got)
	}
}
