// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package mirror is the loopback OCI registry mirror containerd talks to via
// hosts.toml. These tests cover the OpenTelemetry seam: continuing the trace
// containerd started, and describing the serve decision on the span.
package mirror_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
	"github.com/Azure/unbounded/internal/gantry/mirror"
	"github.com/Azure/unbounded/internal/gantry/origin"
	"github.com/Azure/unbounded/internal/gantry/tracing"
	"github.com/Azure/unbounded/internal/gantry/transfer"
)

// installTracer points the global provider at an in-memory recorder and
// restores the previous provider on cleanup.
func installTracer(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()

	rec := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))

	previous := tracing.SetProviderForTest(provider)

	t.Cleanup(func() { tracing.SetProviderForTest(previous) })

	return rec
}

func mustDigest(s string) digest.Digest {
	sum := sha256.Sum256([]byte(s))
	return digest.MustParse("sha256:" + hex.EncodeToString(sum[:]))
}

// TestMirrorContinuesInboundTrace is the seam that makes the whole feature
// work: containerd's registry client sends a W3C traceparent, and the mirror
// must serve underneath that trace ID rather than starting a fresh one. If
// this regresses, kubelet -> containerd -> Gantry stops being one trace.
func TestMirrorContinuesInboundTrace(t *testing.T) {
	rec := installTracer(t)

	const (
		traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
		spanID  = "00f067aa0ba902b7"
	)

	body := []byte("layer content for trace test")
	d := mustDigest(string(body))

	cache := fakes.NewCache()
	cache.Put(d, body)

	cfg := &config.Config{
		UpstreamRegistries: []config.UpstreamRegistry{
			{Name: "reg.example.com", Endpoint: "https://reg.example.com"},
		},
	}

	srv := httptest.NewServer(mirror.New(cfg, cache, fakes.NewOriginPuller()).Handler())
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		fmt.Sprintf("%s/v2/library/nginx/blobs/%s", srv.URL, d), nil)
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	spans := rec.Ended()
	if len(spans) == 0 {
		t.Fatal("no spans recorded for an instrumented mirror request")
	}

	found := false

	for i := range spans {
		if spans[i].Name() != "gantry.mirror.serve_digest" {
			continue
		}

		found = true

		if got := spans[i].SpanContext().TraceID().String(); got != traceID {
			t.Errorf("mirror span trace id = %s, want inbound %s", got, traceID)
		}

		if !spans[i].Parent().IsRemote() {
			t.Error("mirror span must have the inbound span as its remote parent")
		}

		attrs := map[string]string{}
		for _, kv := range spans[i].Attributes() {
			attrs[string(kv.Key)] = kv.Value.String()
		}

		// The attributes the spec asks the span to carry: which digest, and
		// that it came from the local store rather than a peer or the registry.
		if attrs[tracing.AttrDigest] != d.String() {
			t.Errorf("%s = %q, want %s", tracing.AttrDigest, attrs[tracing.AttrDigest], d)
		}

		if attrs[tracing.AttrCacheResult] != tracing.CacheHit {
			t.Errorf("%s = %q, want %q (local store held the digest)", tracing.AttrCacheResult, attrs[tracing.AttrCacheResult], tracing.CacheHit)
		}

		if attrs[tracing.AttrSource] != tracing.SourceLocal {
			t.Errorf("%s = %q, want %q", tracing.AttrSource, attrs[tracing.AttrSource], tracing.SourceLocal)
		}

		// MetricLabel, not String: the observability vocabulary for a blob is
		// "layer". See ifaces.OriginRefKind.MetricLabel for why the two must
		// not be conflated.
		if attrs[tracing.AttrKind] != "layer" {
			t.Errorf("%s = %q, want layer", tracing.AttrKind, attrs[tracing.AttrKind])
		}
	}

	if !found {
		t.Fatalf("no gantry.mirror.serve_digest span recorded; got %d spans", len(spans))
	}
}

// TestMirrorSpansNothingWhenTracingDisabled pins the off-by-default guarantee:
// with the no-op provider installed the mirror still serves the layer and
// records nothing.
func TestMirrorSpansNothingWhenTracingDisabled(t *testing.T) {
	previous := tracing.SetProviderForTest(noopProvider())

	t.Cleanup(func() { tracing.SetProviderForTest(previous) })

	body := []byte("layer content when tracing off")
	d := mustDigest(string(body))

	cache := fakes.NewCache()
	cache.Put(d, body)

	cfg := &config.Config{
		UpstreamRegistries: []config.UpstreamRegistry{
			{Name: "reg.example.com", Endpoint: "https://reg.example.com"},
		},
	}

	srv := httptest.NewServer(mirror.New(cfg, cache, fakes.NewOriginPuller()).Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v2/library/nginx/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 with tracing disabled", resp.StatusCode)
	}
}

// TestMirrorHealthzRecordsNoSpan pins that the kubelet probe produces no span
// at all. A named-but-separate span is not enough: the parent-based sampler
// always samples a request that arrives with no traceparent, so naming alone
// still exports one span per probe. Filtering also means the probe never
// extracts a traceparent, which is correct since nothing propagates a trace
// through a health check.
func TestMirrorHealthzRecordsNoSpan(t *testing.T) {
	rec := installTracer(t)

	cache := fakes.NewCache()
	cfg := &config.Config{
		UpstreamRegistries: []config.UpstreamRegistry{
			{Name: "reg.example.com", Endpoint: "https://reg.example.com"},
		},
	}

	srv := httptest.NewServer(mirror.New(cfg, cache, fakes.NewOriginPuller()).Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; span filtering must not change the response", resp.StatusCode)
	}

	if spans := rec.Ended(); len(spans) != 0 {
		t.Fatalf("healthz must record no spans, got %d", len(spans))
	}
}

// noopProvider mirrors the provider tracing.Init installs when
// OTEL_TRACES_EXPORTER is unset: one that records nothing.
func noopProvider() trace.TracerProvider {
	return noop.NewTracerProvider()
}

// awaitSpans waits until a span with the given name has been recorded, or the
// deadline passes.
//
// Needed because the mirror span ends when the handler returns, and in live
// stream-through mode the response body is copied to containerd asynchronously
// relative to the assertion below. Reading the recorder immediately races that
// and sees only the spans that finished first. The span is always created; this
// only waits for it to end.
func awaitSpans(t *testing.T, rec *tracetest.SpanRecorder, name string) []sdktrace.ReadOnlySpan {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)

	for {
		spans := rec.Ended()

		for _, sp := range spans {
			if sp.Name() == name {
				return spans
			}
		}

		if time.Now().After(deadline) {
			names := make([]string, 0, len(spans))
			for _, sp := range spans {
				names = append(names, sp.Name())
			}

			t.Fatalf("no span named %q before deadline; recorded: %v", name, names)
		}

		time.Sleep(20 * time.Millisecond)
	}
}

// spanAttrs returns the attributes of the first span with the given name.
func spanAttrs(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) map[string]string {
	t.Helper()

	for _, sp := range spans {
		if sp.Name() != name {
			continue
		}

		attrs := map[string]string{}
		for _, kv := range sp.Attributes() {
			attrs[string(kv.Key)] = kv.Value.String()
		}

		return attrs
	}

	names := make([]string, 0, len(spans))
	for _, sp := range spans {
		names = append(names, sp.Name())
	}

	t.Fatalf("no span named %q; recorded: %v", name, names)

	return nil
}

// TestMirrorServeDigestSpanSourcePerBackend pins that gantry.source names the
// backend that actually served, on every path. A cold-start investigation rests
// on this single attribute: local means the bytes never left the node, peer
// means one hop inside the cluster, upstream means the registry.
//
// The metric reports source as cache|peer|origin while the span reports
// local|peer|upstream. The mapping in fireMirrorBytesServed is asserted here so
// a change to either vocabulary fails loudly rather than silently splitting the
// same concept across two spellings.
func TestMirrorServeDigestSpanSourcePerBackend(t *testing.T) {
	body := []byte("span source attribution payload")
	d := digestOf(body)

	tests := []struct {
		name       string
		server     func(t *testing.T) *httptest.Server
		wantSource string
	}{
		{
			name: "local_cache",
			server: func(t *testing.T) *httptest.Server {
				cache := fakes.NewCache()
				cache.Put(d, body)

				cfg, oc := newMirrorOriginNotFound(t)

				return httptest.NewServer(mirror.New(cfg, cache, oc).Handler())
			},
			wantSource: tracing.SourceLocal,
		},
		{
			name: "peer",
			server: func(t *testing.T) *httptest.Server {
				cfg, oc := newMirrorOriginNotFound(t)

				peerCache := fakes.NewCache()
				peerCache.Put(d, body)
				peerAddr := startPeerTransfer(t, peerCache)

				dht := fakes.NewDHT()
				dht.Inject(d, ifaces.Provider{NodeID: "peer-a", Addr: peerAddr})

				return httptest.NewServer(mirror.New(cfg, fakes.NewCache(), oc,
					mirror.WithLiveStreamThrough(),
					mirror.WithDiscovery(dht, transfer.NewClient()),
					mirror.WithPeerBudgets(time.Second, 5*time.Second, 1),
				).Handler())
			},
			wantSource: tracing.SourcePeer,
		},
		{
			name: "upstream",
			server: func(t *testing.T) *httptest.Server {
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/octet-stream")
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

				return httptest.NewServer(mirror.New(cfg, fakes.NewCache(), oc).Handler())
			},
			wantSource: tracing.SourceUpstream,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := installTracer(t)

			srv := tc.server(t)
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + "/v2/library/nginx/blobs/" + d.String())
			if err != nil {
				t.Fatal(err)
			}

			defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}

			attrs := spanAttrs(t, awaitSpans(t, rec, "gantry.mirror.serve_digest"), "gantry.mirror.serve_digest")

			if got := attrs[tracing.AttrSource]; got != tc.wantSource {
				t.Errorf("%s = %q, want %q", tracing.AttrSource, got, tc.wantSource)
			}

			// Every served response must report the bytes it delivered.
			if attrs[tracing.AttrBytes] == "" || attrs[tracing.AttrBytes] == "0" {
				t.Errorf("%s = %q, want a non-zero delivered byte count", tracing.AttrBytes, attrs[tracing.AttrBytes])
			}

			if attrs[tracing.AttrDigest] != d.String() {
				t.Errorf("%s = %q, want %s", tracing.AttrDigest, attrs[tracing.AttrDigest], d)
			}
		})
	}
}

// TestMirrorCachedHeadSpanAttributes pins that a cache-hit HEAD is attributed.
// A HEAD is the request containerd issues when it holds a partial layer, so an
// unattributed HEAD leaves the trace unable to tell a cached hit from a miss.
func TestMirrorCachedHeadSpanAttributes(t *testing.T) {
	rec := installTracer(t)

	body := []byte("head hit payload")
	d := digestOf(body)

	cache := fakes.NewCache()
	cache.Put(d, body)

	cfg, oc := newMirrorOriginNotFound(t)

	srv := httptest.NewServer(mirror.New(cfg, cache, oc).Handler())
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodHead, srv.URL+"/v2/library/nginx/blobs/"+d.String(), nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	attrs := spanAttrs(t, rec.Ended(), "gantry.mirror.serve_digest")

	if attrs[tracing.AttrCacheResult] != tracing.CacheHit {
		t.Errorf("%s = %q, want %q for a cached HEAD", tracing.AttrCacheResult, attrs[tracing.AttrCacheResult], tracing.CacheHit)
	}

	if attrs[tracing.AttrSource] != tracing.SourceLocal {
		t.Errorf("%s = %q, want %q", tracing.AttrSource, attrs[tracing.AttrSource], tracing.SourceLocal)
	}
}

// TestMirrorSpanBytesAccumulateAcrossResume pins that gantry.bytes on the mirror
// span is the total delivered across a resumed live stream, not the final chunk.
//
// A peer that stalls partway is abandoned and another serves the remainder from
// the offset, so one mirror request calls fireMirrorBytesServed more than once.
// Overwriting the attribute would report only the second chunk against the whole
// layer, understating the pull by roughly half. The metric must keep seeing each
// incremental chunk so per-source cluster totals stay correct, so this asserts
// both shapes at once: the span gets the total, the counter gets the chunks.
func TestMirrorSpanBytesAccumulateAcrossResume(t *testing.T) {
	rec := installTracer(t)

	body := []byte("one complete digest assembled across two peer streams")
	d := digestOf(body)

	dialer := &resumingPeerDialer{body: body}

	dht := fakes.NewDHT()
	dht.Inject(d,
		ifaces.Provider{NodeID: "peer-a", Addr: "10.0.0.1:5001"},
		ifaces.Provider{NodeID: "peer-b", Addr: "10.0.0.2:5001"},
	)

	cfg, originSrc := newMirrorOriginNotFound(t)

	var metricChunks []int64

	m := mirror.New(cfg, &writerSpyCache{}, originSrc,
		mirror.WithLiveStreamThrough(),
		mirror.WithDiscovery(dht, dialer),
		mirror.WithPeerBudgets(time.Second, time.Second, 2),
		mirror.WithByteMetrics(func(_, _ string, bytes int64) {
			metricChunks = append(metricChunks, bytes)
		}),
	)

	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	if !bytes.Equal(got, body) {
		t.Fatalf("body = %q, want %q", got, body)
	}

	attrs := spanAttrs(t, awaitSpans(t, rec, "gantry.mirror.serve_digest"), "gantry.mirror.serve_digest")

	// Span: the total across both peer attempts.
	if want := strconv.Itoa(len(body)); attrs[tracing.AttrBytes] != want {
		t.Errorf("%s = %s, want the accumulated total %s across the resumed stream", tracing.AttrBytes, attrs[tracing.AttrBytes], want)
	}

	// Metric: each incremental chunk, unchanged from the pre-accumulation
	// contract. Two chunks summing to the whole.
	if len(metricChunks) < 2 {
		t.Fatalf("metric calls = %d, want >= 2 for a resumed stream (chunks %v)", len(metricChunks), metricChunks)
	}

	var metricTotal int64
	for _, n := range metricChunks {
		metricTotal += n
	}

	if metricTotal != int64(len(body)) {
		t.Errorf("metric chunks sum to %d, want %d (chunks %v)", metricTotal, len(body), metricChunks)
	}
}
