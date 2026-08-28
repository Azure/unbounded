// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package mirror_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
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

type busyThenPeerDialer struct {
	body      []byte
	busyCount int32
	attempts  atomic.Int32
	hardError error
}

type gatedBusyPeerDialer struct {
	body     []byte
	ready    <-chan struct{}
	attempts atomic.Int32
}

type busyThenStalledPeerDialer struct {
	body     []byte
	attempts atomic.Int32
}

type stalledThenPeerDialer struct {
	body     []byte
	attempts atomic.Int32
}

type partialBusyThenPeerDialer struct {
	body     []byte
	attempts atomic.Int32
	mu       sync.Mutex
	offsets  []int64
}

type blockingReadCloser struct {
	closed chan struct{}
	once   sync.Once
}

func (r *blockingReadCloser) Read([]byte) (int, error) {
	<-r.closed

	return 0, errors.New("reader closed")
}

func (r *blockingReadCloser) Close() error {
	r.once.Do(func() { close(r.closed) })

	return nil
}

func (d *stalledThenPeerDialer) FetchFromPeer(ctx context.Context, _ string, _ ifaces.OriginRef) (io.ReadCloser, int64, string, error) {
	if d.attempts.Add(1) == 1 {
		return &contextReadCloser{ctx: ctx}, int64(len(d.body)), "application/octet-stream", nil
	}

	return io.NopCloser(bytes.NewReader(d.body)), int64(len(d.body)), "application/octet-stream", nil
}

type contextReadCloser struct {
	ctx context.Context
}

func (r *contextReadCloser) Read([]byte) (int, error) {
	<-r.ctx.Done()

	return 0, r.ctx.Err()
}

func (*contextReadCloser) Close() error { return nil }

func (d *partialBusyThenPeerDialer) FetchFromPeer(_ context.Context, addr string, ref ifaces.OriginRef) (io.ReadCloser, int64, string, error) {
	d.mu.Lock()
	d.offsets = append(d.offsets, ref.Offset)
	d.mu.Unlock()

	switch d.attempts.Add(1) {
	case 1, 3:
		return nil, 0, "", &ifaces.ErrPeerHTTPStatus{
			PeerAddr:   addr,
			StatusCode: http.StatusTooManyRequests,
			RetryAfter: 10 * time.Millisecond,
		}
	case 2:
		return &failAfterReader{reader: bytes.NewReader(d.body), remaining: 7}, int64(len(d.body)), "application/octet-stream", nil
	default:
		return io.NopCloser(bytes.NewReader(d.body[ref.Offset:])), int64(len(d.body)), "application/octet-stream", nil
	}
}

func (d *partialBusyThenPeerDialer) Offsets() []int64 {
	d.mu.Lock()
	defer d.mu.Unlock()

	return append([]int64(nil), d.offsets...)
}

func (d *busyThenStalledPeerDialer) FetchFromPeer(_ context.Context, addr string, _ ifaces.OriginRef) (io.ReadCloser, int64, string, error) {
	switch d.attempts.Add(1) {
	case 1, 2:
		return nil, 0, "", &ifaces.ErrPeerHTTPStatus{
			PeerAddr:   addr,
			StatusCode: http.StatusTooManyRequests,
			RetryAfter: 10 * time.Millisecond,
		}
	case 3:
		return &blockingReadCloser{closed: make(chan struct{})}, int64(len(d.body)), "application/octet-stream", nil
	default:
		return io.NopCloser(bytes.NewReader(d.body)), int64(len(d.body)), "application/octet-stream", nil
	}
}

func (d *gatedBusyPeerDialer) FetchFromPeer(_ context.Context, addr string, _ ifaces.OriginRef) (io.ReadCloser, int64, string, error) {
	d.attempts.Add(1)

	select {
	case <-d.ready:
		return io.NopCloser(bytes.NewReader(d.body)), int64(len(d.body)), "application/octet-stream", nil
	default:
		return nil, 0, "", &ifaces.ErrPeerHTTPStatus{
			PeerAddr:   addr,
			StatusCode: http.StatusTooManyRequests,
			RetryAfter: 10 * time.Millisecond,
		}
	}
}

type providerThenErrorDHT struct {
	provider ifaces.Provider
	calls    atomic.Int32
}

func (d *providerThenErrorDHT) FindProviders(context.Context, digest.Digest) ([]ifaces.Provider, error) {
	if d.calls.Add(1) == 1 {
		return []ifaces.Provider{d.provider}, nil
	}

	return nil, fmt.Errorf("DHT unavailable")
}

func (*providerThenErrorDHT) Provide(context.Context, digest.Digest) error  { return nil }
func (*providerThenErrorDHT) Withdraw(context.Context, digest.Digest) error { return nil }
func (*providerThenErrorDHT) Health() float64                               { return 1 }

func (d *busyThenPeerDialer) FetchFromPeer(_ context.Context, addr string, _ ifaces.OriginRef) (io.ReadCloser, int64, string, error) {
	attempt := d.attempts.Add(1)
	if attempt <= d.busyCount {
		return nil, 0, "", &ifaces.ErrPeerHTTPStatus{
			PeerAddr:   addr,
			StatusCode: http.StatusTooManyRequests,
			RetryAfter: 20 * time.Millisecond,
		}
	}

	if d.hardError != nil {
		return nil, 0, "", d.hardError
	}

	return io.NopCloser(bytes.NewReader(d.body)), int64(len(d.body)), "application/octet-stream", nil
}

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

func TestMirror_Rediscover_BusyDoesNotFallThroughAfterBudget(t *testing.T) {
	body := []byte("eventually served after transient peer saturation")
	d := digestOf(body)

	var originHits atomic.Int32

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		originHits.Add(1)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		_, _ = w.Write(body) //nolint:errcheck // best-effort write
	}))
	t.Cleanup(up.Close)

	cfg := &config.Config{UpstreamRegistries: []config.UpstreamRegistry{{Name: "reg.example.com", Endpoint: up.URL}}}

	oc, err := origin.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	dht := fakes.NewDHT()
	dht.Inject(d, ifaces.Provider{NodeID: "busy-peer", Addr: "busy-peer:5001"})

	dialer := &busyThenPeerDialer{body: body, busyCount: 4}
	m := mirror.New(cfg, fakes.NewCache(), oc,
		mirror.WithDiscovery(dht, dialer),
		mirror.WithPeerBudgets(time.Second, time.Second, 20),
		mirror.WithPeerRediscover(time.Millisecond, time.Millisecond),
	)
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)

	started := time.Now()

	resp, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if elapsed := time.Since(started); elapsed < 80*time.Millisecond {
		t.Fatalf("elapsed = %v, want all four 20ms Retry-After hints honored", elapsed)
	}

	if got := dialer.attempts.Load(); got != 5 {
		t.Fatalf("peer attempts = %d, want 5", got)
	}

	if got := originHits.Load(); got != 0 {
		t.Fatalf("origin hits = %d, want 0", got)
	}
}

func TestMirror_Rediscover_BusyFlushesMetadataAndKeepsRotatingPeers(t *testing.T) {
	body := []byte("peer body delivered after outer response headers")
	d := digestOf(body)

	var (
		headRequests atomic.Int32
		getRequests  atomic.Int32
	)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		w.Header().Set("Content-Type", "application/octet-stream")

		if r.Method == http.MethodHead {
			headRequests.Add(1)
			w.WriteHeader(http.StatusOK)

			return
		}

		getRequests.Add(1)

		_, _ = w.Write(body) //nolint:errcheck // unexpected origin GET remains visible to the assertion
	}))
	t.Cleanup(up.Close)

	cfg := &config.Config{UpstreamRegistries: []config.UpstreamRegistry{{Name: "reg.example.com", Endpoint: up.URL}}}

	oc, err := origin.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	peerReady := make(chan struct{})
	dialer := &gatedBusyPeerDialer{body: body, ready: peerReady}
	dht := fakes.NewDHT()
	dht.Inject(d, ifaces.Provider{NodeID: "busy-peer", Addr: "busy-peer:5001"})

	m := mirror.New(cfg, fakes.NewCache(), oc,
		mirror.WithLiveStreamThrough(),
		mirror.WithDiscovery(dht, dialer),
		mirror.WithPeerBudgets(time.Second, time.Second, 20),
		mirror.WithPeerRediscover(time.Millisecond, 10*time.Millisecond),
	)
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)

	client := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 200 * time.Millisecond}}

	resp, err := client.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatalf("GET before response-header deadline: %v", err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if resp.ContentLength != int64(len(body)) {
		t.Fatalf("Content-Length = %d, want %d", resp.ContentLength, len(body))
	}

	if got := resp.Header.Get("Docker-Content-Digest"); got != d.String() {
		t.Fatalf("Docker-Content-Digest = %q, want %q", got, d)
	}

	attemptsAtHeaders := dialer.attempts.Load()
	deadline := time.NewTimer(300 * time.Millisecond)
	ticker := time.NewTicker(5 * time.Millisecond)

	for dialer.attempts.Load() <= attemptsAtHeaders {
		select {
		case <-deadline.C:
			t.Fatal("peer attempts did not continue after outer 200 headers")
		case <-ticker.C:
		}
	}

	deadline.Stop()
	ticker.Stop()
	close(peerReady)

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read peer body: %v", err)
	}

	if !bytes.Equal(got, body) {
		t.Fatalf("body = %q, want %q", got, body)
	}

	if got := headRequests.Load(); got != 1 {
		t.Fatalf("origin HEAD requests = %d, want 1", got)
	}

	if got := getRequests.Load(); got != 0 {
		t.Fatalf("origin GET requests = %d, want 0", got)
	}
}

func TestMirror_Rediscover_RotatesAfterPeerHeadersWithoutBody(t *testing.T) {
	body := []byte("body supplied by a different peer")
	d := digestOf(body)

	var getRequests atomic.Int32

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		w.Header().Set("Content-Type", "application/octet-stream")

		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)

			return
		}

		getRequests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(up.Close)

	cfg := &config.Config{UpstreamRegistries: []config.UpstreamRegistry{{Name: "reg.example.com", Endpoint: up.URL}}}

	oc, err := origin.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	dialer := &busyThenStalledPeerDialer{body: body}
	dht := fakes.NewDHT()
	dht.Inject(d,
		ifaces.Provider{NodeID: "peer-a", Addr: "peer-a:5001"},
		ifaces.Provider{NodeID: "peer-b", Addr: "peer-b:5001"},
	)

	m := mirror.New(cfg, fakes.NewCache(), oc,
		mirror.WithLiveStreamThrough(),
		mirror.WithDiscovery(dht, dialer),
		mirror.WithPeerBudgets(time.Second, time.Second, 20),
		mirror.WithPeerRediscover(20*time.Millisecond, 10*time.Millisecond),
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
		t.Fatalf("read body after peer rotation: %v", err)
	}

	if !bytes.Equal(got, body) {
		t.Fatalf("body = %q, want %q", got, body)
	}

	if got := dialer.attempts.Load(); got != 4 {
		t.Fatalf("peer attempts = %d, want 2 busy, 1 header-only, and 1 body", got)
	}

	if got := getRequests.Load(); got != 0 {
		t.Fatalf("origin GET requests = %d, want 0", got)
	}
}

func TestMirror_Rediscover_InitialPeerHeadersWithoutBodyRotates(t *testing.T) {
	body := []byte("body supplied after the initial peer stalls")
	d := digestOf(body)
	dialer := &stalledThenPeerDialer{body: body}
	dht := fakes.NewDHT()
	dht.Inject(d,
		ifaces.Provider{NodeID: "peer-a", Addr: "peer-a:5001"},
		ifaces.Provider{NodeID: "peer-b", Addr: "peer-b:5001"},
	)

	cfg, oc := newMirrorOriginNotFound(t)
	m := mirror.New(cfg, fakes.NewCache(), oc,
		mirror.WithLiveStreamThrough(),
		mirror.WithDiscovery(dht, dialer),
		mirror.WithPeerBudgets(time.Second, time.Second, 20),
		mirror.WithPeerRediscover(20*time.Millisecond, 10*time.Millisecond),
	)
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)

	client := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 200 * time.Millisecond}}

	resp, err := client.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatalf("GET before response-header deadline: %v", err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body after initial peer rotation: %v", err)
	}

	if !bytes.Equal(got, body) {
		t.Fatalf("body = %q, want %q", got, body)
	}

	if got := dialer.attempts.Load(); got != 2 {
		t.Fatalf("peer attempts = %d, want one stalled and one successful", got)
	}
}

func TestMirror_Rediscover_PartialStreamRetriesBusyPeer(t *testing.T) {
	body := []byte("body resumed after the alternate peer was initially busy")
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

	dialer := &partialBusyThenPeerDialer{body: body}
	dht := fakes.NewDHT()
	dht.Inject(d,
		ifaces.Provider{NodeID: "peer-a", Addr: "peer-a:5001"},
		ifaces.Provider{NodeID: "peer-b", Addr: "peer-b:5001"},
	)

	m := mirror.New(cfg, fakes.NewCache(), oc,
		mirror.WithLiveStreamThrough(),
		mirror.WithDiscovery(dht, dialer),
		mirror.WithPeerBudgets(time.Second, time.Second, 20),
		mirror.WithPeerRediscover(20*time.Millisecond, 10*time.Millisecond),
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
		t.Fatalf("read resumed body: %v", err)
	}

	if !bytes.Equal(got, body) {
		t.Fatalf("body = %q, want %q", got, body)
	}

	if offsets := dialer.Offsets(); !slices.Equal(offsets, []int64{0, 0, 7, 7}) {
		t.Fatalf("peer offsets = %v, want [0 0 7 7]", offsets)
	}
}

func TestMirror_Rediscover_BusySkipsColdStartBeforeMetadata(t *testing.T) {
	body := []byte("served after capacity metadata")
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

	dht := fakes.NewDHT()
	dht.Inject(d, ifaces.Provider{NodeID: "peer", Addr: "peer:5001"})

	dialer := &busyThenPeerDialer{body: body, busyCount: 1}
	coldStart := &stubColdStart{err: mirror.ErrColdStartExhausted}
	m := mirror.New(cfg, fakes.NewCache(), oc,
		mirror.WithLiveStreamThrough(),
		mirror.WithDiscovery(dht, dialer),
		mirror.WithColdStart(coldStart),
		mirror.WithPeerBudgets(time.Second, time.Second, 20),
		mirror.WithPeerRediscover(time.Millisecond, 10*time.Millisecond),
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

	if !bytes.Equal(got, body) {
		t.Fatalf("body = %q, want %q", got, body)
	}

	if got := atomic.LoadInt32(&coldStart.calls); got != 0 {
		t.Fatalf("cold-start calls = %d, want 0 before capacity metadata", got)
	}
}

func TestMirror_Rediscover_BusyPrimedResponseUsesConcurrentLocalFill(t *testing.T) {
	body := []byte("content completed by the local containerd store")
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

	peerReady := make(chan struct{})
	dialer := &gatedBusyPeerDialer{body: body, ready: peerReady}
	dht := fakes.NewDHT()
	dht.Inject(d, ifaces.Provider{NodeID: "busy-peer", Addr: "busy-peer:5001"})

	local := fakes.NewCache()

	m := mirror.New(cfg, local, oc,
		mirror.WithLiveStreamThrough(),
		mirror.WithDiscovery(dht, dialer),
		mirror.WithPeerBudgets(time.Second, time.Second, 20),
		mirror.WithPeerRediscover(time.Millisecond, 10*time.Millisecond),
	)
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close

	local.Put(d, body)

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read locally completed body: %v", err)
	}

	if !bytes.Equal(got, body) {
		t.Fatalf("body = %q, want %q", got, body)
	}
}

func TestMirror_Rediscover_HardFailureAfterPrimedHeadersTruncatesBody(t *testing.T) {
	body := []byte("body that must not come from origin")
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

	dht := fakes.NewDHT()
	dht.Inject(d, ifaces.Provider{NodeID: "peer", Addr: "peer:5001"})

	dialer := &busyThenPeerDialer{busyCount: 1, hardError: fmt.Errorf("peer unavailable")}
	m := mirror.New(cfg, fakes.NewCache(), oc,
		mirror.WithLiveStreamThrough(),
		mirror.WithDiscovery(dht, dialer),
		mirror.WithPeerBudgets(time.Second, time.Second, 20),
		mirror.WithPeerRediscover(time.Millisecond, 10*time.Millisecond),
	)
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want committed 200", resp.StatusCode)
	}

	got, err := io.ReadAll(resp.Body)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("read error = %v, want unexpected EOF", err)
	}

	if len(got) != 0 {
		t.Fatalf("truncated body = %q, want no injected HTTP error body", got)
	}
}

func TestMirror_Rediscover_BusyMetadataHeadFailureReturnsServiceUnavailable(t *testing.T) {
	body := []byte("unavailable metadata")
	d := digestOf(body)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(up.Close)

	cfg := &config.Config{UpstreamRegistries: []config.UpstreamRegistry{{Name: "reg.example.com", Endpoint: up.URL}}}

	oc, err := origin.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	dht := fakes.NewDHT()
	dht.Inject(d, ifaces.Provider{NodeID: "busy-peer", Addr: "busy-peer:5001"})

	dialer := &busyThenPeerDialer{busyCount: 100}
	m := mirror.New(cfg, fakes.NewCache(), oc,
		mirror.WithLiveStreamThrough(),
		mirror.WithDiscovery(dht, dialer),
		mirror.WithPeerBudgets(time.Second, time.Second, 20),
		mirror.WithPeerRediscover(time.Millisecond, 10*time.Millisecond),
	)
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 before outer headers are committed", resp.StatusCode)
	}
}

func TestMirror_Rediscover_BusyMetadataRejectsMissingContentType(t *testing.T) {
	body := []byte("metadata without a media type")
	d := digestOf(body)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)

	cfg := &config.Config{UpstreamRegistries: []config.UpstreamRegistry{{Name: "reg.example.com", Endpoint: up.URL}}}

	oc, err := origin.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	dht := fakes.NewDHT()
	dht.Inject(d, ifaces.Provider{NodeID: "busy-peer", Addr: "busy-peer:5001"})

	dialer := &busyThenPeerDialer{busyCount: 1, hardError: errors.New("must not retry after invalid metadata")}
	m := mirror.New(cfg, fakes.NewCache(), oc,
		mirror.WithLiveStreamThrough(),
		mirror.WithDiscovery(dht, dialer),
		mirror.WithPeerBudgets(time.Second, time.Second, 20),
		mirror.WithPeerRediscover(time.Millisecond, 10*time.Millisecond),
	)
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 before outer headers are committed", resp.StatusCode)
	}
}

func TestMirror_Rediscover_BusyMetadataRejectsPeerSizeMismatch(t *testing.T) {
	body := []byte("peer body has the wrong declared size")
	d := digestOf(body)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)+1))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)

	cfg := &config.Config{UpstreamRegistries: []config.UpstreamRegistry{{Name: "reg.example.com", Endpoint: up.URL}}}

	oc, err := origin.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	dht := fakes.NewDHT()
	dht.Inject(d, ifaces.Provider{NodeID: "peer", Addr: "peer:5001"})

	dialer := &busyThenPeerDialer{body: body, busyCount: 1}
	m := mirror.New(cfg, fakes.NewCache(), oc,
		mirror.WithLiveStreamThrough(),
		mirror.WithDiscovery(dht, dialer),
		mirror.WithPeerBudgets(time.Second, time.Second, 20),
		mirror.WithPeerRediscover(time.Millisecond, 10*time.Millisecond),
	)
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want committed 200", resp.StatusCode)
	}

	got, err := io.ReadAll(resp.Body)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("read error = %v, want unexpected EOF", err)
	}

	if len(got) != 0 {
		t.Fatalf("mismatched body = %q, want no bytes", got)
	}
}

func TestMirror_Rediscover_HardFailureAfterBusyReturnsServiceUnavailable(t *testing.T) {
	body := []byte("unused")
	d := digestOf(body)
	cfg := &config.Config{UpstreamRegistries: []config.UpstreamRegistry{{Name: "reg.example.com", Endpoint: "http://origin.invalid"}}}

	oc, err := origin.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	dht := fakes.NewDHT()
	dht.Inject(d, ifaces.Provider{NodeID: "peer", Addr: "peer:5001"})

	dialer := &busyThenPeerDialer{busyCount: 1, hardError: fmt.Errorf("peer unavailable")}
	m := mirror.New(cfg, fakes.NewCache(), oc,
		mirror.WithDiscovery(dht, dialer),
		mirror.WithPeerBudgets(time.Second, time.Second, 20),
		mirror.WithPeerRediscover(time.Millisecond, time.Millisecond),
	)
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestMirror_Rediscover_DHTFailureAfterBusyReturnsServiceUnavailable(t *testing.T) {
	body := []byte("unused")
	d := digestOf(body)
	cfg := &config.Config{UpstreamRegistries: []config.UpstreamRegistry{{Name: "reg.example.com", Endpoint: "http://origin.invalid"}}}

	oc, err := origin.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	dht := &providerThenErrorDHT{provider: ifaces.Provider{NodeID: "peer", Addr: "peer:5001"}}
	dialer := &busyThenPeerDialer{busyCount: 100}
	m := mirror.New(cfg, fakes.NewCache(), oc,
		mirror.WithDiscovery(dht, dialer),
		mirror.WithPeerBudgets(time.Second, time.Second, 20),
		mirror.WithPeerRediscover(time.Millisecond, time.Millisecond),
	)
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}

	if got := dht.calls.Load(); got != 2 {
		t.Fatalf("DHT calls = %d, want 2", got)
	}
}
