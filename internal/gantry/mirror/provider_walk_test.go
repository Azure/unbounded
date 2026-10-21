// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror_test

import (
	"bytes"
	"context"
	"errors"
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
)

// walkOrigin counts origin traffic by method so tests can tell a metadata HEAD
// used to commit headers apart from Gantry pulling the blob itself.
type walkOrigin struct {
	heads atomic.Int32
	gets  atomic.Int32
}

// newWalkMirror builds a mirror with the production peer-path wiring: live
// stream-through, re-discovery enabled, and no per-round attempt cap.
func newWalkMirror(t *testing.T, body []byte, dht ifaces.DHT, dialer ifaces.PeerDialer, coldStart mirror.ColdStartResolver) (*httptest.Server, *walkOrigin) {
	t.Helper()

	d := digestOf(body)
	counts := &walkOrigin{}

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			counts.heads.Add(1)
		} else {
			counts.gets.Add(1)
		}

		ref := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]

		dg, err := digest.Parse(ref)
		if err != nil || dg != d {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))

		if r.Method == http.MethodHead {
			return
		}

		_, _ = w.Write(body) //nolint:errcheck // best-effort write
	}))
	t.Cleanup(up.Close)

	cfg := &config.Config{UpstreamRegistries: []config.UpstreamRegistry{{Name: "reg.example.com", Endpoint: up.URL}}}

	oc, err := origin.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	opts := []mirror.Option{
		mirror.WithLiveStreamThrough(),
		mirror.WithDiscovery(dht, dialer),
		mirror.WithPeerBudgets(0, 5*time.Second, 0),
		mirror.WithPeerRediscover(time.Minute, 10*time.Millisecond),
	}
	if coldStart != nil {
		opts = append(opts, mirror.WithColdStart(coldStart))
	}

	srv := httptest.NewServer(mirror.New(cfg, fakes.NewCache(), oc, opts...).Handler())
	t.Cleanup(srv.Close)

	return srv, counts
}

func staleProviders(n int) []ifaces.Provider {
	providers := make([]ifaces.Provider, 0, n)
	for i := range n {
		providers = append(providers, ifaces.Provider{
			NodeID: ifaces.NodeID(fmt.Sprintf("stale-%02d", i)),
			Addr:   fmt.Sprintf("stale-%02d:5001", i),
		})
	}

	return providers
}

// A lookup used to stop at 20 providers, so 20 stale records could hide a
// usable one. The walk must keep going until a provider serves.
func TestMirror_ProviderWalk_SkipsPastMoreThanTwentyStaleProviders(t *testing.T) {
	body := []byte("served by the only fresh provider behind stale records")
	d := digestOf(body)

	const freshAddr = "fresh:5001"

	dialer := newCountingPeerDialer()
	dialer.Put(freshAddr, d, body)

	stale := staleProviders(25)
	dht := fakes.NewDHT()
	dht.Inject(d, append(stale, ifaces.Provider{NodeID: "fresh", Addr: freshAddr})...)

	coldStart := &stubColdStart{err: errors.New("cold-start must not run")}
	srv, originCounts := newWalkMirror(t, body, dht, dialer, coldStart)

	resp, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	got, _ := io.ReadAll(resp.Body) //nolint:errcheck // best-effort in test
	_ = resp.Body.Close()           //nolint:errcheck // best-effort close

	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, body) {
		t.Fatalf("status = %d, body = %q; want 200 with the fresh provider's bytes", resp.StatusCode, got)
	}

	for _, provider := range stale {
		if calls := dialer.Calls(provider.Addr); calls != 1 {
			t.Fatalf("stale provider %s calls = %d, want 1", provider.Addr, calls)
		}
	}

	if calls := dialer.Calls(freshAddr); calls != 1 {
		t.Fatalf("fresh provider calls = %d, want 1", calls)
	}

	if calls := atomic.LoadInt32(&coldStart.calls); calls != 0 {
		t.Fatalf("cold-start calls = %d, want 0", calls)
	}

	if heads, gets := originCounts.heads.Load(), originCounts.gets.Load(); heads != 0 || gets != 0 {
		t.Fatalf("origin HEAD/GET = %d/%d, want 0/0", heads, gets)
	}

	if find, stream := dht.LookupCalls(); stream == 0 || find != 0 {
		t.Fatalf("lookups find/stream = %d/%d, want the walk to use only the uncapped stream", find, stream)
	}
}

// Advertised providers that all fail without a busy signal must not hold the
// request open for the re-discovery budget. No headers are committed, so the
// 503 lets fail-open containerd pull from origin immediately.
func TestMirror_ProviderWalk_StaleOnlyFailsOverWithoutWaiting(t *testing.T) {
	body := []byte("only origin has these bytes")
	d := digestOf(body)

	stale := staleProviders(3)
	dht := fakes.NewDHT()
	dht.Inject(d, stale...)

	dialer := newCountingPeerDialer()
	coldStart := &stubColdStart{err: errors.New("cold-start must not run")}
	srv, originCounts := newWalkMirror(t, body, dht, dialer, coldStart)

	start := time.Now()

	resp, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close() //nolint:errcheck // best-effort close

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("request took %v, want an immediate failover instead of the re-discovery budget", elapsed)
	}

	for _, provider := range stale {
		if calls := dialer.Calls(provider.Addr); calls != 1 {
			t.Fatalf("stale provider %s calls = %d, want 1", provider.Addr, calls)
		}
	}

	if calls := atomic.LoadInt32(&coldStart.calls); calls != 0 {
		t.Fatalf("cold-start calls = %d, want 0", calls)
	}

	if heads, gets := originCounts.heads.Load(), originCounts.gets.Load(); heads != 0 || gets != 0 {
		t.Fatalf("origin HEAD/GET = %d/%d, want 0/0 (no headers committed, no Gantry origin pull)", heads, gets)
	}
}

// busyThenServeDialer answers 429 from busyAddr for the first busyCalls
// requests, then serves body. Every other address returns not found.
type busyThenServeDialer struct {
	busyAddr string
	body     []byte

	mu        sync.Mutex
	busyCalls int
	calls     map[string]int
}

func (d *busyThenServeDialer) FetchFromPeer(_ context.Context, addr string, ref ifaces.OriginRef) (io.ReadCloser, int64, string, error) {
	d.mu.Lock()
	d.calls[addr]++
	busy := addr == d.busyAddr && d.busyCalls > 0

	if busy {
		d.busyCalls--
	}
	d.mu.Unlock()

	if addr != d.busyAddr {
		return nil, 0, "", &ifaces.ErrNotFound{Digest: ref.Digest}
	}

	if busy {
		return nil, 0, "", &ifaces.ErrPeerHTTPStatus{PeerAddr: addr, StatusCode: http.StatusTooManyRequests}
	}

	return io.NopCloser(bytes.NewReader(d.body)), int64(len(d.body)), "application/octet-stream", nil
}

func (d *busyThenServeDialer) HeadFromPeer(_ context.Context, addr string, ref ifaces.OriginRef) (int64, string, error) {
	if addr != d.busyAddr {
		return 0, "", &ifaces.ErrNotFound{Digest: ref.Digest}
	}

	return int64(len(d.body)), "application/octet-stream", nil
}

func (d *busyThenServeDialer) Calls(addr string) int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.calls[addr]
}

// A busy provider is alive and will serve, so a stale record next to it must
// not turn the request into a 503. The request commits headers and waits for
// a serve slot instead.
func TestMirror_ProviderWalk_BusyProviderHoldsRequestDespiteStaleRecords(t *testing.T) {
	body := []byte("served once the busy provider frees a slot")
	d := digestOf(body)

	const busyAddr = "busy:5001"

	dialer := &busyThenServeDialer{busyAddr: busyAddr, body: body, busyCalls: 2, calls: map[string]int{}}
	stale := staleProviders(1)

	dht := fakes.NewDHT()
	dht.Inject(d, append(stale, ifaces.Provider{NodeID: "busy", Addr: busyAddr})...)

	coldStart := &stubColdStart{err: errors.New("cold-start must not run")}
	srv, originCounts := newWalkMirror(t, body, dht, dialer, coldStart)

	resp, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	got, _ := io.ReadAll(resp.Body) //nolint:errcheck // best-effort in test
	_ = resp.Body.Close()           //nolint:errcheck // best-effort close

	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, body) {
		t.Fatalf("status = %d, body = %q; want 200 with the busy provider's bytes", resp.StatusCode, got)
	}

	if calls := dialer.Calls(busyAddr); calls != 3 {
		t.Fatalf("busy provider calls = %d, want 3 (two 429s, then a serve)", calls)
	}

	if calls := dialer.Calls(stale[0].Addr); calls != 1 {
		t.Fatalf("stale provider calls = %d, want 1 (filtered on later rounds)", calls)
	}

	if calls := atomic.LoadInt32(&coldStart.calls); calls != 0 {
		t.Fatalf("cold-start calls = %d, want 0", calls)
	}

	if gets := originCounts.gets.Load(); gets != 0 {
		t.Fatalf("origin GETs = %d, want 0 (only a metadata HEAD may commit headers)", gets)
	}
}

type partialWalkDHT struct {
	*fakes.DHT
}

func (d partialWalkDHT) StreamProviders(ctx context.Context, ref digest.Digest, budget time.Duration) (<-chan ifaces.Provider, func() error) {
	providers, _ := d.DHT.StreamProviders(ctx, ref, budget)
	return providers, func() error { return context.DeadlineExceeded }
}

func TestMirror_ProviderWalk_PartialLookupPreservesBusyPeerOnRetry(t *testing.T) {
	body := []byte("a partial lookup still found a provider that can serve")
	d := digestOf(body)

	const busyAddr = "busy:5001"

	dht := partialWalkDHT{fakes.NewDHT()}
	dht.Inject(d, ifaces.Provider{NodeID: "busy", Addr: busyAddr})
	dialer := &busyThenServeDialer{busyAddr: busyAddr, body: body, busyCalls: 2, calls: map[string]int{}}
	srv, counts := newWalkMirror(t, body, dht, dialer, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v2/r/blobs/"+d.String(), nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	got, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Equal(got, body) || resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %q, error = %v; want the busy provider's bytes", resp.StatusCode, got, err)
	}

	if calls := dialer.Calls(busyAddr); calls != 3 {
		t.Fatalf("busy provider calls = %d, want two 429s followed by a serve", calls)
	}

	if counts.heads.Load() != 0 || counts.gets.Load() != 0 {
		t.Fatal("partial lookup sent peer recovery traffic to origin")
	}
}

type busyWithFailingPeer struct {
	*busyThenServeDialer
	onHead func()
}

func (d busyWithFailingPeer) FetchFromPeer(ctx context.Context, addr string, ref ifaces.OriginRef) (io.ReadCloser, int64, string, error) {
	if addr != d.busyAddr {
		return nil, 0, "", &ifaces.ErrPeerHTTPStatus{PeerAddr: addr, StatusCode: http.StatusServiceUnavailable}
	}

	return d.busyThenServeDialer.FetchFromPeer(ctx, addr, ref)
}

func (d busyWithFailingPeer) HeadFromPeer(ctx context.Context, addr string, ref ifaces.OriginRef) (int64, string, error) {
	size, contentType, err := d.busyThenServeDialer.HeadFromPeer(ctx, addr, ref)
	if err == nil {
		d.onHead()
	}

	return size, contentType, err
}

func TestMirror_ProviderWalk_ConfirmedBusySurvivesOtherPeerServerFailure(t *testing.T) {
	body := []byte("an unrelated 503 does not discard the confirmed busy provider")
	d := digestOf(body)

	const busyAddr = "busy:5001"

	provider := ifaces.Provider{NodeID: "busy", Addr: busyAddr}
	dht := fakes.NewDHT()
	dht.Inject(d, provider)

	dialer := busyWithFailingPeer{
		busyThenServeDialer: &busyThenServeDialer{busyAddr: busyAddr, body: body, busyCalls: 2, calls: map[string]int{}},
		onHead: func() {
			dht.Inject(d, provider, ifaces.Provider{NodeID: "failed", Addr: "failed:5001"})
		},
	}
	srv, counts := newWalkMirror(t, body, dht, dialer, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v2/r/blobs/"+d.String(), nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	got, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Equal(got, body) || resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %q, error = %v; want the busy provider's bytes", resp.StatusCode, got, err)
	}

	if calls := dialer.Calls(busyAddr); calls != 3 {
		t.Fatalf("busy provider calls = %d, want two 429s followed by a serve", calls)
	}

	if counts.heads.Load() != 0 || counts.gets.Load() != 0 {
		t.Fatal("an unrelated peer failure sent the busy request to origin")
	}
}

type busyBeforeSlowPeer struct {
	*busyThenServeDialer
	entered chan struct{}
	release <-chan struct{}
}

func (d busyBeforeSlowPeer) FetchFromPeer(ctx context.Context, addr string, ref ifaces.OriginRef) (io.ReadCloser, int64, string, error) {
	if addr == d.busyAddr {
		return d.busyThenServeDialer.FetchFromPeer(ctx, addr, ref)
	}

	close(d.entered)

	select {
	case <-d.release:
		return nil, 0, "", &ifaces.ErrNotFound{Digest: ref.Digest}
	case <-ctx.Done():
		return nil, 0, "", ctx.Err()
	}
}

func TestMirror_ProviderWalk_BusyHeadersPrecedeSlowLaterCandidates(t *testing.T) {
	body := []byte("busy headers must not wait for later unreachable providers")
	d := digestOf(body)

	const busyAddr = "busy:5001"

	release := make(chan struct{})

	var releaseOnce sync.Once

	releaseSlowPeer := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseSlowPeer()

	dialer := busyBeforeSlowPeer{
		busyThenServeDialer: &busyThenServeDialer{busyAddr: busyAddr, body: body, busyCalls: 1, calls: map[string]int{}},
		entered:             make(chan struct{}),
		release:             release,
	}
	dht := fakes.NewDHT()
	dht.Inject(d, ifaces.Provider{NodeID: "busy", Addr: busyAddr}, ifaces.Provider{NodeID: "slow", Addr: "slow:5001"})
	srv, counts := newWalkMirror(t, body, dht, dialer, nil)

	transport := &http.Transport{ResponseHeaderTimeout: time.Second}
	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v2/r/blobs/"+d.String(), nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("busy headers were delayed by a later provider: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want waiting headers", resp.StatusCode)
	}

	select {
	case <-dialer.entered:
	case <-ctx.Done():
		t.Fatal("provider walk stopped instead of continuing after busy headers")
	}

	releaseSlowPeer()

	got, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("body = %q, error = %v; want the busy provider's bytes", got, err)
	}

	if counts.heads.Load() != 0 || counts.gets.Load() != 0 {
		t.Fatal("busy headers or body used origin instead of the confirmed peer")
	}
}

// With nothing advertised, the request commits headers before cold-start so
// containerd keeps waiting while a chair pulls, then serves from the chair.
type cappedWalkDHT struct {
	*fakes.DHT
}

func (d cappedWalkDHT) FindProviders(ctx context.Context, ref digest.Digest) ([]ifaces.Provider, error) {
	providers, err := d.DHT.FindProviders(ctx, ref)
	return providers[:min(20, len(providers))], err
}

func TestMirror_ProviderWalk_BusyMetadataFindsPeerBeyondTwentyStale(t *testing.T) {
	body := []byte("the busy provider after twenty stale records owns these bytes")
	d := digestOf(body)

	const busyAddr = "busy:5001"

	dialer := &busyThenServeDialer{busyAddr: busyAddr, body: body, busyCalls: 2, calls: map[string]int{}}
	dht := cappedWalkDHT{fakes.NewDHT()}
	dht.Inject(d, append(staleProviders(25), ifaces.Provider{NodeID: "busy", Addr: busyAddr})...)
	srv, counts := newWalkMirror(t, body, dht, dialer, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v2/r/blobs/"+d.String(), nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	got, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("body = %q, error = %v; want the busy peer's bytes", got, err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if counts.heads.Load() != 0 || counts.gets.Load() != 0 {
		t.Fatal("busy headers used origin instead of the peer's metadata")
	}

	if find, stream := dht.LookupCalls(); find != 0 || stream != 3 {
		t.Fatalf("lookups find/stream = %d/%d; want three uncapped walks (two busy rounds, then a serve)", find, stream)
	}
}

type falseBusyDialer struct{}

func (falseBusyDialer) FetchFromPeer(_ context.Context, addr string, _ ifaces.OriginRef) (io.ReadCloser, int64, string, error) {
	return nil, 0, "", &ifaces.ErrPeerHTTPStatus{PeerAddr: addr, StatusCode: http.StatusTooManyRequests}
}

func (falseBusyDialer) HeadFromPeer(_ context.Context, _ string, ref ifaces.OriginRef) (int64, string, error) {
	return 0, "", &ifaces.ErrNotFound{Digest: ref.Digest}
}

func TestMirror_ProviderWalk_FalseBusyFailsBeforeHeaders(t *testing.T) {
	body := []byte("absent at the peer despite its 429")
	d := digestOf(body)
	dht := fakes.NewDHT()
	dht.Inject(d, ifaces.Provider{NodeID: "stale", Addr: "stale:5001"})

	coldStart := &stubColdStart{}
	srv, counts := newWalkMirror(t, body, dht, falseBusyDialer{}, coldStart)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v2/r/blobs/"+d.String(), nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when HEAD disproves the capacity signal", resp.StatusCode)
	}

	if counts.heads.Load() != 0 || counts.gets.Load() != 0 {
		t.Fatal("origin metadata must not turn a stale peer into a confirmed busy source")
	}

	if atomic.LoadInt32(&coldStart.calls) != 0 {
		t.Fatal("false-busy provider started cold-start")
	}
}

func TestMirror_ProviderWalk_NoProvidersCommitsHeadersThenColdStarts(t *testing.T) {
	body := []byte("pulled by a chair during cold-start")
	d := digestOf(body)

	const chairAddr = "chair:5001"

	dialer := newCountingPeerDialer()
	dialer.Put(chairAddr, d, body)

	headersCommitted := make(chan struct{})
	coldStart := &stubColdStart{
		providers: []ifaces.Provider{{NodeID: "chair", Addr: chairAddr}},
		onResolve: func(digest.Digest) {
			select {
			case <-headersCommitted:
			case <-time.After(5 * time.Second):
			}
		},
	}

	srv, originCounts := newWalkMirror(t, body, fakes.NewDHT(), dialer, coldStart)

	start := time.Now()

	resp, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	// Cold-start blocks until headersCommitted closes, so http.Get returning
	// early proves the response headers went out before cold-start began.
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("headers arrived after %v, want them committed before cold-start blocks", elapsed)
	}

	close(headersCommitted)

	got, _ := io.ReadAll(resp.Body) //nolint:errcheck // best-effort in test
	_ = resp.Body.Close()           //nolint:errcheck // best-effort close

	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, body) {
		t.Fatalf("status = %d, body = %q; want 200 with the chair's bytes", resp.StatusCode, got)
	}

	if calls := atomic.LoadInt32(&coldStart.calls); calls != 1 {
		t.Fatalf("cold-start calls = %d, want 1", calls)
	}

	if heads, gets := originCounts.heads.Load(), originCounts.gets.Load(); heads != 1 || gets != 0 {
		t.Fatalf("origin HEAD/GET = %d/%d, want 1/0 (metadata only)", heads, gets)
	}
}
