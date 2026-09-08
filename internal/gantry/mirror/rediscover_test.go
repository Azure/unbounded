// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror_test

import (
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
