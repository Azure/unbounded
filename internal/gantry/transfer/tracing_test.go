// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package transfer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
	"github.com/Azure/unbounded/internal/gantry/tracing"
)

// installTracer points the global provider at an in-memory recorder for the
// duration of the test. Both the client transport and the server handler
// resolve the provider lazily, so installing here is what makes the recorded
// spans observable.
func installTracer(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()

	rec := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))

	previous := tracing.SetProviderForTest(provider)

	t.Cleanup(func() { tracing.SetProviderForTest(previous) })

	return rec
}

// TestPeerFetchPropagatesTraceContext is the end-to-end assertion behind the
// whole change: a fetch issued by the transfer client must carry a W3C
// traceparent, and the serving peer must continue that trace rather than
// starting an unrelated one. Without either wrapper the two spans share
// nothing and a cold-start cascade is unreadable.
func TestPeerFetchPropagatesTraceContext(t *testing.T) {
	rec := installTracer(t)

	cache := fakes.NewCache()
	addr := startTransferOnEphemeral(t, cache)

	body := []byte("layer-bytes-for-trace")
	d := mustDigest(body)
	cache.Put(d, body)

	// The requester's span: stand-in for the mirror span containerd started.
	ctx, requesterSpan := tracing.Tracer().Start(context.Background(), "gantry.mirror.serve_digest")
	requesterTraceID := requesterSpan.SpanContext().TraceID()

	client := NewClient()

	rc, _, _, err := client.FetchFromPeer(ctx, addr, ifaces.OriginRef{
		Repository: "library/nginx",
		Digest:     d,
		Kind:       ifaces.KindBlob,
	})
	if err != nil {
		t.Fatalf("FetchFromPeer: %v", err)
	}

	_ = rc.Close() //nolint:errcheck // best-effort close

	requesterSpan.End()

	spans := rec.Ended()

	var fetchSpan, serveSpan bool

	for _, sp := range spans {
		switch sp.Name() {
		case "gantry.peer.fetch":
			fetchSpan = true

			if got := sp.SpanContext().TraceID(); got != requesterTraceID {
				t.Errorf("client span trace id = %s, want requester trace id %s", got, requesterTraceID)
			}

			// The client span must be a child of the requester span, which is
			// what stitches Gantry onto containerd's trace.
			if got := sp.Parent().SpanID(); got != requesterSpan.SpanContext().SpanID() {
				t.Errorf("client span parent = %s, want requester span id %s", got, requesterSpan.SpanContext().SpanID())
			}
		case "gantry.transfer.serve":
			serveSpan = true

			if got := sp.SpanContext().TraceID(); got != requesterTraceID {
				t.Errorf("peer serve span trace id = %s, want requester trace id %s (traceparent was not propagated)", got, requesterTraceID)
			}
		}
	}

	if !fetchSpan {
		t.Fatalf("no gantry.peer.fetch span recorded; spans: %v", spanNames(spans))
	}

	if !serveSpan {
		t.Fatalf("no gantry.transfer.serve span recorded; spans: %v", spanNames(spans))
	}
}

// TestTransferHandlerExtractsTraceparentFromHeader checks the server side in
// isolation: a request that arrives with a traceparent must adopt that trace
// ID even when the caller is not this client.
func TestTransferHandlerExtractsTraceparentFromHeader(t *testing.T) {
	rec := installTracer(t)

	srv := New(fakes.NewCache())
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	const (
		traceID = "0af7651916cd43dd8448eb211c80319c"
		spanID  = "b7ad6b7169203331"
	)

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/v2/", nil)
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
		t.Fatal("no spans recorded for instrumented request")
	}

	got := spans[len(spans)-1].SpanContext().TraceID().String()
	if got != traceID {
		t.Fatalf("serve span trace id = %s, want the incoming %s", got, traceID)
	}

	// The incoming span is the remote parent, which is what puts the whole
	// Gantry hop underneath containerd's PullImage span.
	if !spans[len(spans)-1].Parent().IsRemote() {
		t.Fatal("expected a remote parent from the incoming traceparent")
	}
}

// TestTransferHandlerHealthzRecordsNoSpan pins that the kubelet probe produces
// no span at all. A named-but-separate span is not enough: the root sampler
// always samples a traceparent-less request, so naming alone still exports one
// span per probe to the collector.
func TestTransferHandlerHealthzRecordsNoSpan(t *testing.T) {
	rec := installTracer(t)

	srv := New(fakes.NewCache())
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; filtering must not change the response", resp.StatusCode)
	}

	if spans := rec.Ended(); len(spans) != 0 {
		t.Fatalf("healthz must record no spans, got %v", spanNames(spans))
	}
}

// TestPeerSpanNamesAvoidPerDigestURLs pins the aggregation property: two
// different digests must produce spans with the same name, otherwise every
// layer shows up as its own row in a trace viewer.
func TestPeerSpanNamesAvoidPerDigestURLs(t *testing.T) {
	rec := installTracer(t)

	cache := fakes.NewCache()
	addr := startTransferOnEphemeral(t, cache)
	client := NewClient()

	ctx := context.Background()

	for _, payload := range [][]byte{[]byte("layer-a"), []byte("layer-b")} {
		d := mustDigest(payload)
		cache.Put(d, payload)

		rc, _, _, err := client.FetchFromPeer(ctx, addr, ifaces.OriginRef{
			Repository: "library/nginx",
			Digest:     d,
			Kind:       ifaces.KindBlob,
		})
		if err != nil {
			t.Fatalf("FetchFromPeer: %v", err)
		}

		_ = rc.Close() //nolint:errcheck // best-effort close
	}

	names := map[string]int{}
	for _, sp := range rec.Ended() {
		names[sp.Name()]++
	}

	if names["gantry.peer.fetch"] != 2 {
		t.Errorf("gantry.peer.fetch span count = %d, want 2 (names must not vary per digest)", names["gantry.peer.fetch"])
	}

	if names["gantry.transfer.serve"] != 2 {
		t.Errorf("gantry.transfer.serve span count = %d, want 2 (names must not vary per digest)", names["gantry.transfer.serve"])
	}
}

// TestPeerFetchSpansNothingWhenTracingDisabled pins the off-by-default cost
// guarantee: with the no-op provider installed, the instrumented client and
// server still serve bytes and record nothing.
func TestPeerFetchSpansNothingWhenTracingDisabled(t *testing.T) {
	previous := tracing.SetProviderForTest(noopProvider())

	t.Cleanup(func() { tracing.SetProviderForTest(previous) })

	cache := fakes.NewCache()
	addr := startTransferOnEphemeral(t, cache)

	body := []byte("payload-when-disabled")
	d := mustDigest(body)
	cache.Put(d, body)

	client := NewClient()

	rc, size, _, err := client.FetchFromPeer(context.Background(), addr, ifaces.OriginRef{
		Repository: "library/nginx",
		Digest:     d,
		Kind:       ifaces.KindBlob,
	})
	if err != nil {
		t.Fatalf("FetchFromPeer with tracing disabled: %v", err)
	}

	defer func() { _ = rc.Close() }() //nolint:errcheck // best-effort close

	if size != int64(len(body)) {
		t.Fatalf("size = %d, want %d", size, len(body))
	}
}

// spanNames flattens recorded span names so assertion failures show what was
// actually recorded instead of just "not found".
func spanNames(spans []sdktrace.ReadOnlySpan) []string {
	names := make([]string, 0, len(spans))
	for _, sp := range spans {
		names = append(names, sp.Name())
	}

	return names
}

// noopProvider mirrors the state tracing.Init installs when
// OTEL_TRACES_EXPORTER is unset: a provider that records nothing.
func noopProvider() trace.TracerProvider {
	return noop.NewTracerProvider()
}
