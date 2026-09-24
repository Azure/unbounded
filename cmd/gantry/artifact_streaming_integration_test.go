// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c" //nolint:staticcheck // h2c is Gantry's peer protocol

	"github.com/Azure/unbounded/internal/gantry/advertise"
	"github.com/Azure/unbounded/internal/gantry/chairs"
	"github.com/Azure/unbounded/internal/gantry/coldstart"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/httprange"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
	"github.com/Azure/unbounded/internal/gantry/inflight"
	"github.com/Azure/unbounded/internal/gantry/streaming"
	"github.com/Azure/unbounded/internal/gantry/transfer"
)

func TestArtifactStreamingManifestPullTransitionsOriginToPeer(t *testing.T) {
	t.Parallel()

	layerBody := []byte("0123456789")
	layerDigest := trackerDigestOf(layerBody)
	configBody := []byte(`{"architecture":"amd64","os":"linux"}`)
	configDigest := trackerDigestOf(configBody)
	manifestBody := []byte(fmt.Sprintf(`{
		"schemaVersion":2,
		"mediaType":"application/vnd.oci.image.manifest.v1+json",
		"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},
		"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":%q,"size":%d,
			"annotations":{"containerd.io/snapshot/overlaybd/blob-digest":%q,"containerd.io/snapshot/overlaybd/blob-size":%q}}]
	}`, configDigest.String(), len(configBody), layerDigest.String(), len(layerBody), layerDigest.String(), fmt.Sprint(len(layerBody))))
	manifestDigest := trackerDigestOf(manifestBody)

	var originRangeRequests atomic.Int32

	originServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originRangeRequests.Add(1)

		if got := r.Header.Get("Range"); got != "bytes=2-5" {
			t.Errorf("origin Range = %q, want bytes=2-5", got)
		}

		w.Header().Set("Content-Range", "bytes 2-5/10")
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(layerBody[2:6]) //nolint:errcheck // best-effort test response
	}))
	t.Cleanup(originServer.Close)

	peerCache := fakes.NewCache()

	var peerRangeRequests atomic.Int32

	peerAddr := startArtifactStreamingPeer(t, transfer.New(peerCache,
		transfer.WithMetrics(func() { peerRangeRequests.Add(1) }, nil),
	).Handler())

	dht := newAdvertisingDHT(ifaces.Provider{NodeID: "chair-peer", Addr: peerAddr})
	advertiser := advertise.New(&artifactStreamingInventory{Cache: peerCache}, dht,
		advertise.WithProvideTimeout(time.Second))

	originPuller := fakes.NewOriginPuller()
	originPuller.Put(configDigest, configBody)
	originPuller.Put(layerDigest, layerBody)
	coordinator := &artifactStreamingChairCoordinator{
		origin:     originPuller,
		cache:      peerCache,
		advertiser: advertiser,
		inflight:   inflight.New(inflight.DefaultStalls(), nil),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	const epoch = int64(7)

	chair := chairs.Chair{
		ID: 0,
		Holder: ifaces.PeerEndpoint{
			PeerID:       "chair-peer",
			P2PAddrs:     []string{"/ip4/127.0.0.1/tcp/1"},
			TransferAddr: peerAddr,
		},
		Generation:      3,
		AssignmentEpoch: epoch,
	}
	resolver := coldstart.NewChairResolver(coldstart.ChairOptions{
		Chairs:       &artifactStreamingChairSnapshot{snapshot: chairs.Snapshot{Epoch: epoch, Chairs: []chairs.Chair{chair}}},
		Discovery:    dht,
		Coord:        coordinator,
		Inflight:     inflight.New(inflight.DefaultStalls(), nil),
		SelfPeerID:   "requester",
		CurrentEpoch: func() int64 { return epoch },
		QueryTimeout: time.Second,
		APITimeout:   time.Second,
		SeedCount:    1,
		PollManifest: time.Millisecond,
		PollLayer:    time.Millisecond,
	})

	manifestCache := fakes.NewCache()
	manifestCache.Put(manifestDigest, manifestBody)
	prefetcher := &layerPrefetchAdapter{cache: manifestCache, resolver: resolver, logger: slog.Default()}

	policy := streaming.URLPolicy{AllowedHostSuffixes: []string{"localhost"}, AllowHTTP: true}

	originClient := streaming.NewOriginClient(policy)

	streamServer, err := streaming.NewServer(
		artifactStreamingMissingRangeStore{},
		dht,
		transfer.NewClient(transfer.WithRequestTimeout(5*time.Second)),
		originClient,
		streaming.Options{URLPolicy: policy, PeerLookupTimeout: time.Second, MaxPeerAttempts: 1},
	)
	if err != nil {
		t.Fatal(err)
	}

	originURL := strings.Replace(originServer.URL, "127.0.0.1", "localhost", 1) +
		"?d=" + layerDigest.String() + "&sig=redacted"
	requestRange := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, streaming.HandlerPrefix+originURL, nil)
		req.Header.Set("Range", "bytes=2-5")

		response := httptest.NewRecorder()
		streamServer.ServeHTTP(response, req)

		return response
	}

	first := requestRange()
	if first.Code != http.StatusPartialContent || first.Body.String() != "2345" {
		t.Fatalf("origin response = %d %q, want 206 2345", first.Code, first.Body.String())
	}

	prefetcher.OnManifestServed(t.Context(), "app.azurecr.io", "team/app", manifestDigest)

	if !coordinator.pulled(layerDigest) {
		t.Fatalf("streaming layer %s was not dispatched to the chair", layerDigest)
	}

	if present, hasErr := peerCache.Has(t.Context(), layerDigest); hasErr != nil || !present {
		t.Fatalf("chair cache Has(%s) = %t, %v; want true, nil", layerDigest, present, hasErr)
	}

	if got := dht.provideCount(layerDigest); got != 1 {
		t.Fatalf("DHT Provide(%s) calls = %d, want 1", layerDigest, got)
	}

	second := requestRange()
	if second.Code != http.StatusPartialContent || second.Body.String() != "2345" {
		t.Fatalf("peer response = %d %q, want 206 2345", second.Code, second.Body.String())
	}

	if got := originRangeRequests.Load(); got != 1 {
		t.Fatalf("origin range requests = %d, want 1 after peer advertisement", got)
	}

	if got := peerRangeRequests.Load(); got != 1 {
		t.Fatalf("peer range requests = %d, want 1", got)
	}
}

type artifactStreamingMissingRangeStore struct{}

func (artifactStreamingMissingRangeStore) OpenRange(_ context.Context, d digest.Digest, _ httprange.Range) (io.ReadCloser, int64, error) {
	return nil, 0, &ifaces.ErrNotFound{Digest: d}
}

type artifactStreamingInventory struct {
	*fakes.Cache
}

func (*artifactStreamingInventory) Inventory(context.Context) ([]digest.Digest, error) {
	return nil, nil
}

type artifactStreamingChairSnapshot struct {
	snapshot chairs.Snapshot
}

func (s *artifactStreamingChairSnapshot) Snapshot(context.Context, int64) (chairs.Snapshot, error) {
	return s.snapshot, nil
}

func (s *artifactStreamingChairSnapshot) RefreshChair(_ context.Context, id chairs.ID) (chairs.Chair, error) {
	for _, chair := range s.snapshot.Chairs {
		if chair.ID == id {
			return chair, nil
		}
	}

	return chairs.Chair{}, fmt.Errorf("chair %s not found", id.Name())
}

type artifactStreamingChairCoordinator struct {
	mu         sync.Mutex
	origin     ifaces.OriginPuller
	cache      ifaces.LocalContentStore
	advertiser *advertise.Advertiser
	inflight   *inflight.Map
	logger     *slog.Logger
	pulls      []digest.Digest
}

func (c *artifactStreamingChairCoordinator) PleasePullChair(ctx context.Context, _ ifaces.PeerEndpoint, registry, repository string, kind ifaces.OriginRefKind, digests []digest.Digest, _ ifaces.ChairAssignment) ([]ifaces.PleasePullOutcome, error) {
	outcomes := make([]ifaces.PleasePullOutcome, 0, len(digests))
	for _, child := range digests {
		c.mu.Lock()
		c.pulls = append(c.pulls, child)
		c.mu.Unlock()

		handle, existing, already := c.inflight.Start(child, kind, 0)
		if already {
			outcomes = append(outcomes, ifaces.PleasePullOutcome{Digest: child, Outcome: ifaces.PleasePullAlreadyPulling, StartedAt: existing.StartedAt})
			continue
		}

		runOriginPull(ctx, c.origin, c.cache, nil, c.logger, handle, registry, repository, child, kind, time.Second,
			func(markCtx context.Context, marked digest.Digest) bool {
				return c.advertiser.Notify(markCtx, marked, true)
			}, nil, nil, leaseMetricHooks{})
		outcomes = append(outcomes, ifaces.PleasePullOutcome{Digest: child, Outcome: ifaces.PleasePullStarted})
	}

	return outcomes, nil
}

func (c *artifactStreamingChairCoordinator) pulled(want digest.Digest) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, pulled := range c.pulls {
		if pulled == want {
			return true
		}
	}

	return false
}

type advertisingDHT struct {
	mu       sync.Mutex
	provider ifaces.Provider
	provided map[digest.Digest]int
}

func newAdvertisingDHT(provider ifaces.Provider) *advertisingDHT {
	return &advertisingDHT{provider: provider, provided: map[digest.Digest]int{}}
}

func (d *advertisingDHT) FindProviders(_ context.Context, child digest.Digest) ([]ifaces.Provider, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.provided[child] == 0 {
		return nil, nil
	}

	return []ifaces.Provider{d.provider}, nil
}

func (d *advertisingDHT) Provide(_ context.Context, child digest.Digest) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.provided[child]++

	return nil
}

func (d *advertisingDHT) Withdraw(_ context.Context, child digest.Digest) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	delete(d.provided, child)

	return nil
}

func (*advertisingDHT) Health() float64 { return 1 }

func (d *advertisingDHT) provideCount(child digest.Digest) int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.provided[child]
}

func startArtifactStreamingPeer(t *testing.T, handler http.Handler) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	httpServer := &http.Server{
		Handler:           h2c.NewHandler(handler, &http2.Server{}), //nolint:staticcheck // h2c is Gantry's peer protocol
		ReadHeaderTimeout: time.Second,
	}
	go func() { _ = httpServer.Serve(listener) }() //nolint:errcheck // test cleanup owns shutdown

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		_ = httpServer.Shutdown(ctx) //nolint:errcheck // best-effort test cleanup
	})

	return listener.Addr().String()
}
