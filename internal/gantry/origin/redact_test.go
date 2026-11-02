// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package origin

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
)

// installRecorder points the global provider at an in-memory span recorder for
// the duration of t and returns it.
//
// The origin transport is built from tracing.Provider(), which reads the global,
// so installing here is what makes the recorded url.full observable. The
// propagator is installed too because production gets it from tracing.Init and
// these tests bypass Init.
func installRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()

	rec := tracetest.NewSpanRecorder()

	prevProv := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()

	prov := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))

	otel.SetTracerProvider(prov)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	t.Cleanup(func() {
		otel.SetTracerProvider(prevProv)
		otel.SetTextMapPropagator(prevProp)
	})

	return rec
}

// expectedSig is the exact signature the storage stub will accept. Anything
// else, including the REDACTED placeholder, must produce a 403.
const expectedSig = "AZURE_SECRET"

// signedRedirectQuery is the query a pre-signed storage URL carries. Azure Blob
// Storage in the shape a registry redirect produces, plus the S3 and GCS
// spellings, so the test pins the whole vocabulary rather than one provider.
const signedRedirectQuery = "sv=2024-01-01&se=2030-01-01T00:00:00Z&sig=" + expectedSig + "&sp=r&sr=b&X-Amz-Credential=AKID&X-Amz-Signature=s3secret&X-Amz-Expires=3600&X-Goog-Signature=gcssecret&X-Goog-Signature-Algorithm=RSA"

// secretsIn returns the credential substrings that must never appear anywhere in
// a recorded URL.
func secretsIn() []string {
	return []string{"AZURE_SECRET", "s3secret", "gcssecret", "AKID", "2024-01-01"}
}

// TestSignedRedirectRedactsSpanWithoutBreakingChain pins that a registry
// redirect to a pre-signed storage URL does not export the signature to the trace
// collector, and that redacting it does not break the fetch.
//
// This is the regression test for the leak otelhttp cannot fix by configuration:
// it records url.full with the complete query string, stripping only URL user
// information. Because blob fetches follow redirects to storage endpoints whose
// credentials live in the query, the pre-signed signature would otherwise land on
// the storage span, readable by anyone with dashboard access and replayable for
// the remaining lifetime of the signature.
//
// The contract is subtler than "leave the URL alone". The request that carries
// the signature is the one the redirect produced, and that request is what
// otelhttp both records and sends, so a transport cannot redact the span of that
// hop without redacting the hop itself. What must survive intact is the
// signature in the Location header, because that is where http.Client reads the
// next target from. So the chain has to complete end to end:
//
//	blob request -> signed 302 -> storage -> bytes
//
// with REDACTED on every recorded url.full and no credential anywhere in the
// recorded attributes. Reaching the storage stub rather than a 403 is the proof
// the signature was never lost, since the stub only serves the layer when it
// receives a signature it can see.
//
// Note the storage stub does receive the signature. That is not a leak: it is the
// credential's intended recipient, and it arrives via the Location header that
// http.Client resolves the redirect against, which is never a span attribute.
func TestSignedRedirectRedactsSpanWithoutBreakingChain(t *testing.T) {
	rec := installRecorder(t)

	const body = "layer-bytes-delivered-intact"

	var mu sync.Mutex

	var (
		sawSig       bool
		storageSeen  bool
		receivedSig  string
		receivedPath string
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		switch r.URL.Path {
		case "/v2/r/blobs/sha256:deadbeef":
			// A blob the mirror cannot serve locally, so the client follows the
			// registry's signed redirect to storage.
			http.Redirect(w, r, "/blobstore?"+signedRedirectQuery, http.StatusFound)
		case "/v2/r/blobs/sha256:cafe":
			http.Redirect(w, r, "/storage?"+signedRedirectQuery, http.StatusFound)
		case "/storage", "/blobstore":
			storageSeen = true
			receivedPath = r.URL.Path
			receivedSig = r.URL.Query().Get("sig")

			// The stub validates the signature against the exact value it issued,
			// not merely that a parameter is present. An earlier version accepted
			// any non-empty sig, which meant sig=REDACTED passed and the test
			// served a 200 for a request no real storage provider would serve. That
			// false pass is exactly how the wire mutation went unnoticed.
			sawSig = receivedSig == expectedSig

			if !sawSig {
				w.WriteHeader(http.StatusForbidden)

				return
			}

			fmt.Fprint(w, body)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(upstream.Close)

	client := newRegistryHTTPClient(http.DefaultTransport)

	resp, err := client.Get(upstream.URL + "/v2/r/blobs/sha256:deadbeef") //nolint:bodyclose // body read and closed below
	if err != nil {
		t.Fatalf("GET: %v", err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	// The chain completed. If redaction had leaked into the wire request, the
	// storage stub would have answered 403 and the body would not be the layer.
	if !storageSeen {
		t.Fatalf("never reached the storage stub; body=%q status=%d: redaction broke the signed redirect", got, resp.StatusCode)
	}

	if string(got) != body {
		t.Errorf("body = %q, want %q: the signed redirect did not deliver the layer", got, body)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (body %q)", resp.StatusCode, got)
	}

	mu.Lock()

	if !sawSig {
		t.Errorf("storage stub received sig=%q on %s, want exactly %q: the pre-signed signature must arrive on the wire unaltered, or real storage providers reject the pull", receivedSig, receivedPath, expectedSig)
	}

	mu.Unlock()

	spans := rec.Ended()

	if len(spans) < 2 {
		t.Fatalf("recorded spans = %d, want >= 2 (blob request and signed redirect)", len(spans))
	}

	var sawStorage bool

	for _, s := range spans {
		for _, kv := range s.Attributes() {
			if kv.Key != "url.full" {
				continue
			}

			full := kv.Value.AsString()

			for _, secret := range secretsIn() {
				if strings.Contains(full, secret) {
					t.Errorf("span %q leaked credential %q in url.full = %s", s.Name(), secret, full)
				}
			}

			if strings.Contains(full, "/storage") || strings.Contains(full, "/blobstore") {
				sawStorage = true

				if !strings.Contains(full, "REDACTED") {
					t.Errorf("span %q url.full = %s, want the signed values replaced with REDACTED", s.Name(), full)
				}
			}
		}
	}

	if !sawStorage {
		t.Fatal("no span recorded for the redirected storage request; cannot assert redaction")
	}
}

// TestRedactionDoesNotMutateCallerRequest pins that the transport leaves the
// caller's request untouched, which is what stops a redacted URL from
// propagating into a redirect hop built from it.
func TestRedactionDoesNotMutateCallerRequest(t *testing.T) {
	installRecorder(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	client := newRegistryHTTPClient(http.DefaultTransport)

	req, err := http.NewRequest(http.MethodGet, upstream.URL+"/v2/r/blobs/sha256:cafe?sig=KEEPME&sv=1", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	resp, err := client.Do(req) //nolint:bodyclose // body closed below
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if !strings.Contains(req.URL.RawQuery, "KEEPME") {
		t.Errorf("caller request was mutated by the transport: RawQuery = %s", req.URL.RawQuery)
	}
}

// TestRedactQueryVocabulary exercises redactQuery directly across provider
// spellings and the over-redaction tradeoff.
func TestRedactQueryVocabulary(t *testing.T) {
	cases := []struct {
		name      string
		query     string
		wantClear []string // values that must be gone
		wantKept  []string // key=value pairs expected to survive
	}{
		{
			name:      "azure sas",
			query:     "sv=2024-01-01&se=2030&sig=SECRET&sp=r&sr=b&srt=sco",
			wantClear: []string{"SECRET", "2024-01-01", "2030"},
			wantKept:  nil,
		},
		{
			name:      "s3",
			query:     "X-Amz-Credential=AKID&X-Amz-Signature=SECRET&X-Amz-Expires=3600&versionId=abc123&partNumber=2",
			wantClear: []string{"SECRET", "AKID", "3600"},
			// Ordinary parameters survive, which is what keeps the trace useful.
			wantKept: []string{"versionId=abc123", "partNumber=2"},
		},
		{
			name:      "gcs",
			query:     "X-Goog-Signature=SECRET&X-Goog-Expires=600",
			wantClear: []string{"SECRET", "600"},
		},
		{
			name:      "opaque short names",
			query:     "sig=SECRET&se=2030&st=2024&sv=1&sr=b",
			wantClear: []string{"SECRET", "2030", "2024", "1"},
		},
		{
			name:      "plain diagnostic query is untouched",
			query:     "n=3&n=4",
			wantClear: nil,
			wantKept:  []string{"n=3", "n=4"},
		},
		{
			// The short SAS names are exact-match only, so ordinary parameters
			// that merely contain them as a substring keep their values. Matching
			// "st" or "se" by substring would redact these and make a trace lie
			// about a request that carried no credential.
			name:      "ordinary names containing short signed names",
			query:     "last=abc&first=def&range=0-1023&name=layer&test=1&statement=x",
			wantClear: nil,
			wantKept:  []string{"last=abc", "first=def", "range=0-1023", "name=layer", "test=1", "statement=x"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse("https://registry.example/v2/blob?" + tc.query)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			redactQuery(u)

			for _, secret := range tc.wantClear {
				if strings.Contains(u.RawQuery, secret) {
					t.Errorf("RawQuery still contains %q: %s", secret, u.RawQuery)
				}
			}

			for _, kept := range tc.wantKept {
				if !strings.Contains(u.RawQuery, kept) {
					t.Errorf("RawQuery = %s, want it to preserve %q", u.RawQuery, kept)
				}
			}
		})
	}
}

// TestRedactQueryNoopWhenNothingSensitive pins that a request without
// credentials is passed through without a rewrite, which is what keeps the
// common path allocation-free.
func TestRedactQueryNoopWhenNothingSensitive(t *testing.T) {
	u, err := url.Parse("https://registry.example/v2/tags/list?last=abc&n=100")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	before := u.RawQuery
	if redactQuery(u) {
		t.Errorf("redactQuery reported a change for a non-credential query: before=%s after=%s", before, u.RawQuery)
	}

	if u.RawQuery != before {
		t.Errorf("RawQuery = %s, want unchanged %s", u.RawQuery, before)
	}
}

// TestWireKeepsSignatureSpanDoesNot is the core assertion of this whole
// mechanism, stated directly at the transport instead of through a redirect: the
// request that reaches the wrapped transport carries the original credential, and
// the span carries the placeholder.
//
// Everything else here is supporting detail. This is the pair that must hold
// simultaneously, and every wrong version of the fix satisfied one half by
// breaking the other.
func TestWireKeepsSignatureSpanDoesNot(t *testing.T) {
	rec := installRecorder(t)

	var wireQuery string

	rt := &tracingRoundTripper{base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		wireQuery = r.URL.RawQuery

		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})}

	req, err := http.NewRequest(http.MethodGet, "https://registry.example/v2/r/blobs/sha256:cafe?sig=AZURE_SECRET&sv=2024&n=100", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	if resp.Body != nil {
		_ = resp.Body.Close()
	}

	// The wire keeps the signature, in full, plus the non-credential parameter.
	for _, want := range []string{"sig=AZURE_SECRET", "sv=2024", "n=100"} {
		if !strings.Contains(wireQuery, want) {
			t.Errorf("wire RawQuery = %q, want it to contain %q", wireQuery, want)
		}
	}

	if strings.Contains(wireQuery, "REDACTED") {
		t.Errorf("wire RawQuery = %q, must never contain REDACTED", wireQuery)
	}

	// The caller's request is untouched.
	if !strings.Contains(req.URL.RawQuery, "AZURE_SECRET") {
		t.Errorf("caller request was mutated: RawQuery = %s", req.URL.RawQuery)
	}

	// The span shows the placeholder and never the secret.
	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}

	strAttrs := map[string]string{}

	redactedFlag := map[string]bool{}

	for _, kv := range spans[0].Attributes() {
		if kv.Value.Type() == attribute.BOOL {
			redactedFlag[string(kv.Key)] = kv.Value.AsBool()

			continue
		}

		strAttrs[string(kv.Key)] = kv.Value.AsString()
	}

	full := strAttrs["url.full"]

	if strings.Contains(full, "AZURE_SECRET") || strings.Contains(full, "2024") {
		t.Errorf("span url.full = %q, leaked a signed value", full)
	}

	if !strings.Contains(full, "REDACTED") {
		t.Errorf("span url.full = %q, want REDACTED placeholders", full)
	}

	// n=100 is not a credential and stays visible.
	if !strings.Contains(full, "n=100") {
		t.Errorf("span url.full = %q, want the non-credential n=100 preserved", full)
	}

	if !redactedFlag["gantry.url.redacted"] {
		t.Errorf("gantry.url.redacted = %v, want true (bool present and set)", redactedFlag["gantry.url.redacted"])
	}

	if _, ok := redactedFlag["gantry.url.redacted"]; !ok {
		t.Error("gantry.url.redacted attribute missing")
	}
}

// TestSpanRedactedFalseWhenNoCredential pins the flag that makes a dead matcher
// detectable: a request with nothing sensitive must report redacted=false.
func TestSpanRedactedFalseWhenNoCredential(t *testing.T) {
	rec := installRecorder(t)

	rt := &tracingRoundTripper{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})}

	req, err := http.NewRequest(http.MethodGet, "https://registry.example/v2/_catalog?n=50", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	if resp.Body != nil {
		_ = resp.Body.Close()
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}

	found := false

	for _, kv := range spans[0].Attributes() {
		if kv.Key != attribute.Key("gantry.url.redacted") {
			continue
		}

		found = true

		if kv.Value.Type() != attribute.BOOL {
			t.Errorf("gantry.url.redacted type = %v, want BOOL", kv.Value.Type())

			continue
		}

		if kv.Value.AsBool() {
			t.Error("gantry.url.redacted = true, want false for a request with no credential")
		}
	}

	if !found {
		t.Error("gantry.url.redacted attribute missing")
	}
}

// TestTraceparentInjectedOnWireRequest pins that the wire request still carries
// trace context. The redacted clone is what the span describes; propagation has
// to ride the request that actually leaves, or cross-node traces never join up.
func TestTraceparentInjectedOnWireRequest(t *testing.T) {
	installRecorder(t)

	var traceparent string

	rt := &tracingRoundTripper{base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		traceparent = r.Header.Get("traceparent")

		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})}

	req, err := http.NewRequest(http.MethodGet, "https://registry.example/v2/_catalog", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	if resp.Body != nil {
		_ = resp.Body.Close()
	}

	if traceparent == "" {
		t.Error("wire request carried no traceparent; downstream peers cannot join this trace")
	}

	// The caller's header map must not have been written into.
	if req.Header.Get("traceparent") != "" {
		t.Error("transport injected traceparent into the caller's request headers")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// TestWireUnalteredWhenTracingDisabled pins that an operator who never turned on
// tracing is not exposed to transport behavior they did not ask for.
//
// The signature has to survive with the provider disabled. Redaction only ever
// touches the span, so a no-op provider changes what is recorded, never what is
// sent. This matters beyond tidiness: the earlier design redacted
// unconditionally, before consulting the provider at all, so a stock deploy that
// had not opted into OTLP still got sig=REDACTED on the wire and storage
// rejected the pull. Tracing being off was never supposed to mean behavior
// changes, and it is not a state anyone would think to check when a pull starts
// failing.
func TestWireUnalteredWhenTracingDisabled(t *testing.T) {
	prev := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()

	otel.SetTracerProvider(noop.NewTracerProvider())
	otel.SetTextMapPropagator(propagation.TraceContext{})

	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		otel.SetTextMapPropagator(prevProp)
	})

	rt := &tracingRoundTripper{base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if !strings.Contains(r.URL.RawQuery, "AZURE_SECRET") {
			t.Errorf("wire RawQuery = %q with tracing disabled, want the original credential intact", r.URL.RawQuery)
		}

		if strings.Contains(r.URL.RawQuery, "REDACTED") {
			t.Errorf("wire RawQuery = %q with tracing disabled, must not contain REDACTED", r.URL.RawQuery)
		}

		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})}

	req, err := http.NewRequest(http.MethodGet, "https://registry.example/v2/r/blobs/sha256:cafe?sig=AZURE_SECRET&sv=2024", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	if resp.Body != nil {
		_ = resp.Body.Close()
	}

	// A disabled provider still propagates, so traceparent is expected here even
	// though nothing is recorded. That split is deliberate; see tracing.disable.
	if req.Header.Get("traceparent") != "" {
		t.Error("transport wrote traceparent into the caller's request")
	}
}

// TestRecordedURLDropsUserInfo pins that URL user information never reaches the
// span.
//
// Supported legacy endpoints may carry Basic credentials in the registry
// endpoint itself, and urlWithPath copies r.base including its User, so every
// derived request URL holds the configured username and password. Query
// redaction says nothing about those. The wire keeps them, because that is how
// the endpoint authenticates, and the span must not.
//
// This is a second and independent credential channel from the signed query
// parameters, and the reason a fix aimed at "signed URLs" still leaks: the
// placeholder substitution only ever looks at RawQuery.
func TestRecordedURLDropsUserInfo(t *testing.T) {
	rec := installRecorder(t)

	rt := &tracingRoundTripper{base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		// The wire keeps the credential. That is how Basic-over-URL endpoints
		// authenticate, so clearing it here would break the pull.
		pass, ok := r.URL.User.Password()
		if !ok || pass != "WIREPASS" {
			t.Errorf("wire request lost its credential: user=%v", r.URL.User)
		}

		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})}

	req, err := http.NewRequest(http.MethodGet, "https://octocat:WIREPASS@registry.example/v2/_catalog?sig=SIGVAL", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	if resp.Body != nil {
		_ = resp.Body.Close()
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}

	for _, kv := range spans[0].Attributes() {
		if kv.Key != attribute.Key("url.full") {
			continue
		}

		full := kv.Value.AsString()

		for _, secret := range []string{"WIREPASS", "octocat"} {
			if strings.Contains(full, secret) {
				t.Errorf("span url.full = %q, leaked user information %q", full, secret)
			}
		}

		if !strings.Contains(full, "registry.example") {
			t.Errorf("span url.full = %q, want the host preserved for diagnostics", full)
		}
	}

	// The caller's request keeps its credential.
	if req.URL.User == nil {
		t.Error("caller request lost its user information")
	}
}

// TestSpanEndsWhenBodyCompletes pins that an origin span covers the download,
// not just the round trip to first byte.
//
// A blob fetch streams the body long after RoundTrip returns. Ending the span on
// return records a multi-gigabyte layer as the latency to headers, which is
// exactly the number a "why was this pull slow" trace is opened to read, and it
// reads as a fast upstream when the upstream was slow.
func TestSpanEndsWhenBodyCompletes(t *testing.T) {
	rec := installRecorder(t)

	release := make(chan struct{})

	rt := &tracingRoundTripper{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       &blockingBody{release: release},
		}, nil
	})}

	req, err := http.NewRequest(http.MethodGet, "https://registry.example/v2/r/blobs/sha256:cafe", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	// Headers have arrived and RoundTrip has returned. The span must still be
	// open, because nothing has been transferred yet.
	if got := len(rec.Ended()); got != 0 {
		t.Fatalf("ended spans = %d right after RoundTrip, want 0: the span must cover the body", got)
	}

	// Read on another goroutine because the body blocks until released, and the
	// release has to happen after the span-still-open check above.
	readDone := make(chan error, 1)

	go func() {
		_, err := io.ReadAll(resp.Body)
		readDone <- err
	}()

	// Still open while the transfer is in flight.
	time.Sleep(50 * time.Millisecond)

	if got := len(rec.Ended()); got != 0 {
		t.Errorf("ended spans = %d mid-transfer, want 0", got)
	}

	close(release)

	if err := <-readDone; err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	if err := resp.Body.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}

	if spans[0].EndTime().Before(spans[0].StartTime()) {
		t.Error("span ends before it starts")
	}
}

// TestSpanEndsOnEOFWithoutClose pins EOF as an end trigger in its own right.
// Without it, a body read to completion and then left unclosed ends its span
// whenever the collector happens to free it, putting a duration on the trace that
// no request experienced.
func TestSpanEndsOnEOFWithoutClose(t *testing.T) {
	rec := installRecorder(t)

	rt := &tracingRoundTripper{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("payload")),
		}, nil
	})}

	req, err := http.NewRequest(http.MethodGet, "https://registry.example/v2/_catalog", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	if got := len(rec.Ended()); got != 1 {
		t.Fatalf("ended spans = %d after reading to EOF without Close, want 1", got)
	}
}

// TestHTTPStatusMarksSpanError pins that an HTTP failure fails the span.
//
// A 401, 429, or 500 is a successful round trip at the transport, so nothing
// else here marks it. Callers reject these responses, so the spans this affects
// are precisely the failed upstream hops an operator looks for, and leaving them
// Unset drops them from every error-filtered trace view.
func TestHTTPStatusMarksSpanError(t *testing.T) {
	for _, tc := range []struct {
		status    int
		wantError bool
	}{
		{status: http.StatusOK, wantError: false},
		{status: http.StatusPartialContent, wantError: false},
		{status: http.StatusMovedPermanently, wantError: false}, // redirects are routine
		{status: http.StatusUnauthorized, wantError: true},
		{status: http.StatusTooManyRequests, wantError: true},
		{status: http.StatusInternalServerError, wantError: true},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			rec := installRecorder(t)

			rt := &tracingRoundTripper{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: tc.status,
					Body:       io.NopCloser(strings.NewReader("x")),
				}, nil
			})}

			req, err := http.NewRequest(http.MethodGet, "https://registry.example/v2/_catalog", nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}

			resp, err := rt.RoundTrip(req)
			if err != nil {
				t.Fatalf("RoundTrip: %v", err)
			}

			_, _ = io.ReadAll(resp.Body)

			if err := resp.Body.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			spans := rec.Ended()
			if len(spans) != 1 {
				t.Fatalf("spans = %d, want 1", len(spans))
			}

			gotErr := spans[0].Status().Code == codes.Error

			if gotErr != tc.wantError {
				t.Errorf("status %d: span error = %v (%q), want %v", tc.status, gotErr, spans[0].Status().Description, tc.wantError)
			}
		})
	}
}

// blockingBody blocks until release is closed, then reports EOF.
type blockingBody struct {
	release chan struct{}
	done    bool
}

func (b *blockingBody) Read(p []byte) (int, error) {
	if b.done {
		return 0, io.EOF
	}

	<-b.release
	b.done = true

	return 0, io.EOF
}

func (b *blockingBody) Close() error { return nil }

// TestReadFailureFailsSpanAndEndsIt pins the two things a mid-body read failure
// must do.
//
// A download truncated after the headers arrived is invisible to RoundTrip,
// which already returned successfully, so without this the span is both marked
// fine and left open. It also must not leak: whatever else happens, the body
// finishing ends the span.
func TestReadFailureFailsSpanAndEndsIt(t *testing.T) {
	rec := installRecorder(t)

	rt := &tracingRoundTripper{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(&failingBody{}),
		}, nil
	})}

	req, err := http.NewRequest(http.MethodGet, "https://registry.example/v2/r/blobs/sha256:cafe", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("expected the body read to fail")
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d after a read failure, want 1: the span must not be left open", len(spans))
	}

	if spans[0].Status().Code != codes.Error {
		t.Errorf("span status = %v, want Error for a truncated download", spans[0].Status().Code)
	}
}

// failingBody returns data once and then a non-EOF error.
type failingBody struct {
	sent bool
}

func (b *failingBody) Read(p []byte) (int, error) {
	if b.sent {
		return 0, errors.New("unexpected EOF")
	}

	b.sent = true

	return copy(p, "partial"), nil
}

func (b *failingBody) Close() error { return nil }

// TestPercentEncodedParameterNameIsRedacted pins that a signed parameter written
// with an escaped name cannot smuggle its value into the recorded URL.
//
// url.URL.Query decodes parameter names, so %73ig is recognized as sig, but the
// recorded string is built from RawQuery, which keeps the original escaping. A
// fix that substitutes into the decoded values and then re-encodes can therefore
// report a value as redacted while the serialized URL still shows it.
func TestPercentEncodedParameterNameIsRedacted(t *testing.T) {
	rec := installRecorder(t)

	rt := &tracingRoundTripper{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})}

	// %73 is "s", so the name decodes to sig while the raw text does not.
	req, err := http.NewRequest(http.MethodGet, "https://registry.example/v2/r/blobs/sha256:cafe?%73ig=SECRETVALUE&sv=2024", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	if resp.Body != nil {
		_ = resp.Body.Close()
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}

	for _, kv := range spans[0].Attributes() {
		if kv.Key != attribute.Key("url.full") {
			continue
		}

		if strings.Contains(kv.Value.AsString(), "SECRETVALUE") {
			t.Errorf("span url.full = %q, leaked the value behind an escaped parameter name", kv.Value.AsString())
		}
	}
}

// TestRepeatedParameterValuesAllRedacted pins that every value of a repeated
// parameter is redacted, not just the one Query().Get returns.
func TestRepeatedParameterValuesAllRedacted(t *testing.T) {
	rec := installRecorder(t)

	rt := &tracingRoundTripper{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})}

	req, err := http.NewRequest(http.MethodGet, "https://registry.example/v2/x?sig=FIRST&sig=SECOND&n=1", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	if resp.Body != nil {
		_ = resp.Body.Close()
	}

	for _, kv := range rec.Ended()[0].Attributes() {
		if kv.Key != attribute.Key("url.full") {
			continue
		}

		full := kv.Value.AsString()
		for _, secret := range []string{"FIRST", "SECOND"} {
			if strings.Contains(full, secret) {
				t.Errorf("span url.full = %q, leaked repeated value %q", full, secret)
			}
		}
	}
}

// TestRedactQueryEscapedNameNoRawValueLeft calls redactQuery at the level the
// bypass would occur, on a URL whose RawQuery escapes the parameter name, and
// requires that no trace of the original value survives anywhere on the URL.
//
// Query decodes names but RawQuery keeps the original escaping, so a substitution
// that only rewrites decoded values can mark a parameter redacted while the
// serialized URL still carries the secret. Checking the serialized forms rather
// than the parsed map is what makes this a real assertion: after re-encoding,
// Query() reports the placeholder no matter what the raw text says.
func TestRedactQueryEscapedNameNoRawValueLeft(t *testing.T) {
	u, err := url.Parse("https://registry.example/v2/r/blobs/sha256:cafe?%73ig=SECRETVALUE&sv=2024&n=100")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if !redactQuery(u) {
		t.Fatal("redactQuery reported no change, but %73ig decodes to sig")
	}

	for _, form := range []string{u.RawQuery, u.String(), u.EscapedPath() + u.RawQuery} {
		if strings.Contains(form, "SECRETVALUE") {
			t.Errorf("escaped parameter name bypassed redaction, %q still contains the value", form)
		}
	}

	if got := u.Query().Get("sig"); got != redactedQueryValue {
		t.Errorf("Query().Get(sig) = %q, want %q", got, redactedQueryValue)
	}

	if got := u.Query().Get("n"); got != "100" {
		t.Errorf("Query().Get(n) = %q, want the non-credential value preserved", got)
	}
}

// TestRedactQueryEscapedInteriorName pins a known, accepted limit rather than
// claiming it is safe.
//
// An escaped character in the middle of a parameter name changes the decoded
// name, so "si%6Fg" becomes "sigo" and matches no credential fragment, leaving
// the value recorded as-is. See sensitiveQuery for why that is accepted. The test
// asserts the current behavior so the day a provider starts emitting signed
// parameters outside the matched vocabulary, or someone reworks the matcher, this
// fails and the tradeoff gets re-examined instead of quietly becoming wrong.
func TestRedactQueryEscapedInteriorName(t *testing.T) {
	u, err := url.Parse("https://registry.example/v2/x?si%6Fg=SECRETVALUE")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if redactQuery(u) {
		t.Fatal("mangled parameter name is now redacted: update this test and the documented limit on sensitiveQuery")
	}

	if !strings.Contains(u.RawQuery, "SECRETVALUE") {
		t.Error("expected the current limit to leave the value intact")
	}
}
