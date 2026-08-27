// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package mirror_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// headTestStack wires a full mirror with origin upstream + DHT + peer
// transfer + cold-start resolver and exposes the counters needed to
// pin the fourteenth-review HEAD short-circuit contract:
//
//	HEAD is metadata-only:
//	 - HEAD MUST NOT bump p2p_origin_pull_total (origin.Pull start).
//	 - HEAD MUST NOT consult the cold-start resolver
//	 (no please_pull RPCs from a metadata probe).
//	 - HEAD MUST NOT issue a peer body-GET (no cache warming).
//	 - HEAD MUST issue at most one upstream HEAD request.
type headTestStack struct {
	srv            *httptest.Server
	originHeadHits *int32
	originPullHits *int32
	pullStarts     *int32 // origin.WithMetrics onPullStart
	peerFetches    *int32 // mirror.WithPeerMetrics "hit"
	coldStartCalls *int32 // stub resolver invocations
	dht            *fakes.DHT
	cacheDir       *fakes.Cache
}

// fakeColdStart counts invocations. Its return value never matters for
// the HEAD tests below - the contract is "Resolve must NOT be called",
// so any error / providers value works as long as the counter trips
// only when serveDigest actually invokes it.
type fakeColdStart struct {
	calls int32
}

func (f *fakeColdStart) Resolve(_ context.Context, _ digest.Digest, _ ifaces.OriginRefKind, _, _ string, _ int64) (*mirror.ColdStartResolution, error) {
	atomic.AddInt32(&f.calls, 1)
	return &mirror.ColdStartResolution{Providers: nil, Outcome: "stub"}, nil
}

func newHeadTestStack(t *testing.T, originBlobs map[digest.Digest][]byte, providers map[digest.Digest][]ifaces.Provider) *headTestStack {
	t.Helper()

	var originHeadHits, originPullHits int32

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			atomic.AddInt32(&originHeadHits, 1)
		case http.MethodGet:
			atomic.AddInt32(&originPullHits, 1)
		}

		path := r.URL.Path

		var refStart int

		switch {
		case strings.Contains(path, "/blobs/"):
			refStart = strings.LastIndex(path, "/blobs/") + len("/blobs/")
		case strings.Contains(path, "/manifests/"):
			refStart = strings.LastIndex(path, "/manifests/") + len("/manifests/")
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}

		ref := path[refStart:]

		d, err := digest.Parse(ref)
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		body, ok := originBlobs[d]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))

		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}

		_, _ = w.Write(body) //nolint:errcheck // best-effort write
	}))
	t.Cleanup(up.Close)

	cfg := &config.Config{
		UpstreamRegistries: []config.UpstreamRegistry{
			{Name: "reg.example.com", Endpoint: up.URL},
		},
	}
	c := fakes.NewCache()

	var pullStarts int32

	oc, err := origin.New(cfg,
		origin.WithMetrics(
			func(_ string) { atomic.AddInt32(&pullStarts, 1) },
			func(_, _ string) {},
		),
	)
	if err != nil {
		t.Fatal(err)
	}

	dht := fakes.NewDHT()
	for d, provs := range providers {
		dht.Inject(d, provs...)
	}

	var peerFetches int32

	client := transfer.NewClient(transfer.WithDialTimeout(time.Second), transfer.WithRequestTimeout(5*time.Second))
	cs := &fakeColdStart{}
	m := mirror.New(cfg, c, oc,
		mirror.WithDiscovery(dht, client),
		mirror.WithColdStart(cs),
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

	return &headTestStack{
		srv:            srv,
		originHeadHits: &originHeadHits,
		originPullHits: &originPullHits,
		pullStarts:     &pullStarts,
		peerFetches:    &peerFetches,
		coldStartCalls: &cs.calls,
		dht:            dht,
		cacheDir:       c,
	}
}

// TestMirror_HEAD_CacheMiss_DHTEmpty_DoesNotConsultColdStart pins the
// reviewer's primary fourteenth-review case: HEAD on a cache miss with
// the DHT empty MUST NOT consult the cold-start resolver. Before the
// fix, serveDigest fell through to tryPeerFallback whose empty-DHT
// branch calls s.coldStart.Resolve - which can issue please_pull RPCs
// and trigger a closest-peer puller to origin-pull the digest. A
// HEAD is supposed to be a no-side-effects metadata probe.
//
// Required behaviour:
// - cold-start invocations = 0
// - peer fetches = 0
// - origin.Pull starts = 0 (p2p_origin_pull_total)
// - origin GETs upstream = 0
// - origin HEADs upstream = 1 (the only metadata round-trip)
func TestMirror_HEAD_CacheMiss_DHTEmpty_DoesNotConsultColdStart(t *testing.T) {
	body := []byte("metadata-probe-target")
	d := digestOf(body)

	stack := newHeadTestStack(t,
		map[digest.Digest][]byte{d: body},
		nil, // empty DHT - this is the case where cold-start used to fire
	)

	req, _ := http.NewRequest(http.MethodHead, stack.srv.URL+"/v2/r/blobs/"+d.String(), nil)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	_, _ = io.Copy(io.Discard, resp.Body) //nolint:errcheck // best-effort
	resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("HEAD status = %d, want 200", resp.StatusCode)
	}

	if n := atomic.LoadInt32(stack.coldStartCalls); n != 0 {
		t.Errorf("cold-start invocations = %d, want 0 (HEAD must be metadata-only - please_pull on a probe is a the stampede risk)", n)
	}

	if n := atomic.LoadInt32(stack.peerFetches); n != 0 {
		t.Errorf("peer fetches = %d, want 0 (HEAD must not cache-warm)", n)
	}

	if n := atomic.LoadInt32(stack.pullStarts); n != 0 {
		t.Errorf("origin.Pull starts = %d, want 0 (HEAD must take origin.Head, never bump p2p_origin_pull_total)", n)
	}

	if n := atomic.LoadInt32(stack.originPullHits); n != 0 {
		t.Errorf("upstream GET hits = %d, want 0 (HEAD must never issue a GET to origin)", n)
	}

	if n := atomic.LoadInt32(stack.originHeadHits); n != 1 {
		t.Errorf("upstream HEAD hits = %d, want 1 (HEAD must issue exactly one metadata round-trip)", n)
	}
}

// TestMirror_HEAD_CacheMiss_DHTProviders_DoesNotPeerFetch pins the
// second half of the same contract: when the DHT DOES have providers,
// HEAD must still skip the peer fetch loop. Before the fix,
// fetchOneProvider would GET the full body from the peer and commit
// it to local cache, then return only headers to the HEAD caller -
// a metadata probe that silently warmed the cache and burned peer
// fetch budget.
//
// The provider in this test points at a real h2c transfer server
// holding the body, so a misrouted peer GET would succeed and trip
// the peerFetches counter. The test asserts that path is dead.
func TestMirror_HEAD_CacheMiss_DHTProviders_DoesNotPeerFetch(t *testing.T) {
	body := []byte("provider-has-it-but-head-does-not-care")
	d := digestOf(body)

	peerCache := fakes.NewCache()
	peerCache.Put(d, body)
	peerAddr := startPeerTransfer(t, peerCache)

	stack := newHeadTestStack(t,
		map[digest.Digest][]byte{d: body},
		map[digest.Digest][]ifaces.Provider{d: {{NodeID: "peer-a", Addr: peerAddr}}},
	)

	req, _ := http.NewRequest(http.MethodHead, stack.srv.URL+"/v2/r/blobs/"+d.String(), nil)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	_, _ = io.Copy(io.Discard, resp.Body) //nolint:errcheck // best-effort
	resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("HEAD status = %d, want 200", resp.StatusCode)
	}

	if n := atomic.LoadInt32(stack.peerFetches); n != 0 {
		t.Errorf("peer fetches = %d, want 0 (HEAD must not body-GET from a peer even when the DHT has providers)", n)
	}

	if n := atomic.LoadInt32(stack.coldStartCalls); n != 0 {
		t.Errorf("cold-start invocations = %d, want 0 (HEAD must not consult cold-start)", n)
	}

	if n := atomic.LoadInt32(stack.pullStarts); n != 0 {
		t.Errorf("origin.Pull starts = %d, want 0", n)
	}

	if n := atomic.LoadInt32(stack.originHeadHits); n != 1 {
		t.Errorf("upstream HEAD hits = %d, want 1", n)
	}
}
