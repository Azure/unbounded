// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package mirror_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

type byteObservation struct {
	kind   string
	source string
	bytes  int64
}

func pullMirrorBody(t *testing.T, handler http.Handler, d digest.Digest) []byte {
	t.Helper()

	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatalf("GET: %v", err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%q", resp.StatusCode, body)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	return body
}

func TestMirrorByteMetricsCacheSource(t *testing.T) {
	body := []byte("cached bytes served to local containerd")
	d := digestOf(body)
	cfg, oc := newMirrorOriginNotFound(t)
	cache := fakes.NewCache()
	cache.Put(d, body)

	var served, completed []byteObservation

	m := mirror.New(cfg, cache, oc,
		mirror.WithByteMetrics(func(kind, source string, bytes int64) {
			served = append(served, byteObservation{kind: kind, source: source, bytes: bytes})
		}),
		mirror.WithMirrorResponseCompletedHook(func(kind, source string) {
			completed = append(completed, byteObservation{kind: kind, source: source})
		}),
	)

	if got := pullMirrorBody(t, m.Handler(), d); string(got) != string(body) {
		t.Fatalf("body = %q, want %q", got, body)
	}

	want := byteObservation{kind: "layer", source: "cache", bytes: int64(len(body))}
	if len(served) != 1 || served[0] != want {
		t.Fatalf("served observations = %+v, want [%+v]", served, want)
	}

	if len(completed) != 1 || completed[0].kind != want.kind || completed[0].source != want.source {
		t.Fatalf("completed observations = %+v, want kind=%s source=%s", completed, want.kind, want.source)
	}
}

func TestMirrorByteMetricsPeerSource(t *testing.T) {
	body := []byte("peer bytes streamed to local containerd")
	d := digestOf(body)
	cfg, oc := newMirrorOriginNotFound(t)

	peerCache := fakes.NewCache()
	peerCache.Put(d, body)
	peerAddr := startPeerTransfer(t, peerCache)

	dht := fakes.NewDHT()
	dht.Inject(d, ifaces.Provider{NodeID: "peer-a", Addr: peerAddr})

	var fetched, served, completed []byteObservation

	peerClient := transfer.NewClient(transfer.WithClientByteMetrics(func(kind string, bytes int64) {
		fetched = append(fetched, byteObservation{kind: kind, bytes: bytes})
	}))

	m := mirror.New(cfg, fakes.NewCache(), oc,
		mirror.WithLiveStreamThrough(),
		mirror.WithDiscovery(dht, peerClient),
		mirror.WithPeerBudgets(time.Second, 5*time.Second, 1),
		mirror.WithByteMetrics(
			func(kind, source string, bytes int64) {
				served = append(served, byteObservation{kind: kind, source: source, bytes: bytes})
			},
		),
		mirror.WithMirrorResponseCompletedHook(func(kind, source string) {
			completed = append(completed, byteObservation{kind: kind, source: source})
		}),
	)

	if got := pullMirrorBody(t, m.Handler(), d); string(got) != string(body) {
		t.Fatalf("body = %q, want %q", got, body)
	}

	wantFetched := byteObservation{kind: "layer", bytes: int64(len(body))}
	if len(fetched) != 1 || fetched[0] != wantFetched {
		t.Fatalf("fetched observations = %+v, want [%+v]", fetched, wantFetched)
	}

	wantServed := byteObservation{kind: "layer", source: "peer", bytes: int64(len(body))}
	if len(served) != 1 || served[0] != wantServed {
		t.Fatalf("served observations = %+v, want [%+v]", served, wantServed)
	}

	if len(completed) != 1 || completed[0].kind != wantServed.kind || completed[0].source != wantServed.source {
		t.Fatalf("completed observations = %+v, want kind=%s source=%s", completed, wantServed.kind, wantServed.source)
	}
}

func TestMirrorByteMetricsOriginSource(t *testing.T) {
	body := []byte("origin bytes streamed to local containerd")
	d := digestOf(body)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/r/blobs/"+d.String() {
			http.NotFound(w, r)

			return
		}

		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		_, _ = w.Write(body) //nolint:errcheck // best-effort test response
	}))
	t.Cleanup(upstream.Close)

	cfg := &config.Config{UpstreamRegistries: []config.UpstreamRegistry{
		{Name: "reg.example.com", Endpoint: upstream.URL},
	}}

	var originBytes []byteObservation

	oc, err := origin.New(cfg, origin.WithByteMetrics(func(kind string, bytes int64) {
		originBytes = append(originBytes, byteObservation{kind: kind, bytes: bytes})
	}))
	if err != nil {
		t.Fatalf("origin.New: %v", err)
	}

	var served, completed []byteObservation

	m := mirror.New(cfg, fakes.NewCache(), oc,
		mirror.WithLiveStreamThrough(),
		mirror.WithByteMetrics(func(kind, source string, bytes int64) {
			served = append(served, byteObservation{kind: kind, source: source, bytes: bytes})
		}),
		mirror.WithMirrorResponseCompletedHook(func(kind, source string) {
			completed = append(completed, byteObservation{kind: kind, source: source})
		}),
	)

	if got := pullMirrorBody(t, m.Handler(), d); string(got) != string(body) {
		t.Fatalf("body = %q, want %q", got, body)
	}

	wantOrigin := byteObservation{kind: "layer", bytes: int64(len(body))}
	if len(originBytes) != 1 || originBytes[0] != wantOrigin {
		t.Fatalf("origin observations = %+v, want [%+v]", originBytes, wantOrigin)
	}

	wantServed := byteObservation{kind: "layer", source: "origin", bytes: int64(len(body))}
	if len(served) != 1 || served[0] != wantServed {
		t.Fatalf("served observations = %+v, want [%+v]", served, wantServed)
	}

	if len(completed) != 1 || completed[0].kind != wantServed.kind || completed[0].source != wantServed.source {
		t.Fatalf("completed observations = %+v, want kind=%s source=%s", completed, wantServed.kind, wantServed.source)
	}
}
