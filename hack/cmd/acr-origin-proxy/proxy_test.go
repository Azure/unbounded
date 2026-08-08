// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestProxyCountsUpstreamAndClientBytesByPhase(t *testing.T) {
	var upstreamAuthorization string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamAuthorization = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "hello-proxy") //nolint:errcheck // Test server writes are asserted through the client response.
	}))
	defer upstream.Close()

	controller, observer, handler := testProxy(t, upstream.URL)
	if err := controller.set(phaseBaseline); err != nil {
		t.Fatalf("set phase: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/v2/acme/app/blobs/"+testDigest(), nil)
	request.Header.Set("Authorization", "Bearer inbound")
	request.Header.Set("User-Agent", "containerd/v2.3.2")

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.String() != "hello-proxy" {
		t.Fatalf("response = (%d, %q)", response.Code, response.Body.String())
	}

	wantAuthorization := "Basic " + base64.StdEncoding.EncodeToString([]byte("demo-user:demo-pass"))
	if upstreamAuthorization != wantAuthorization {
		t.Fatalf("upstream Authorization = %q, want %q", upstreamAuthorization, wantAuthorization)
	}

	labels := []string{string(pathClassBlob), string(clientClassContainerd), "200", "run-1", string(phaseBaseline)}
	if got := testutil.ToFloat64(observer.bytesUpstream.WithLabelValues(labels...)); got != 11 {
		t.Fatalf("upstream bytes = %v, want 11", got)
	}

	if got := testutil.ToFloat64(observer.bytesToClient.WithLabelValues(labels...)); got != 11 {
		t.Fatalf("client bytes = %v, want 11", got)
	}

	snapshot := observer.snapshot(time.Now())

	baseline := snapshot.Totals.ByPhase[phaseBaseline]
	if baseline.RequestsCompleted != 1 || baseline.BytesUpstream != 11 || baseline.BytesToClient != 11 {
		t.Fatalf("baseline totals = %+v", baseline)
	}
}

func TestProxyRecordsHTTPErrorStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()

	_, observer, handler := testProxy(t, upstream.URL)
	response := httptest.NewRecorder()
	handler.ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "/v2/acme/app/manifests/"+testDigest(), nil),
	)

	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusTooManyRequests)
	}

	setup := observer.snapshot(time.Now()).Totals.ByPhase[phaseSetup]
	if setup.ByStatus["429"] != 1 {
		t.Fatalf("status totals = %+v, want one 429", setup.ByStatus)
	}

	if len(setup.UpstreamErrors) != 0 {
		t.Fatalf("transport errors = %+v, want none", setup.UpstreamErrors)
	}
}

func TestProxyRecordsUpstreamTransportError(t *testing.T) {
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, syscall.ECONNREFUSED
	})}
	_, observer, handler := testProxyWithClient(t, "https://registry.example", "basic", client)

	response := httptest.NewRecorder()
	handler.ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "/v2/acme/app/blobs/"+testDigest(), nil),
	)

	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadGateway)
	}

	setup := observer.snapshot(time.Now()).Totals.ByPhase[phaseSetup]
	if setup.ByStatus["502"] != 1 {
		t.Fatalf("status totals = %+v, want one 502", setup.ByStatus)
	}

	if setup.UpstreamErrors[upstreamErrorConnectionRefused] != 1 {
		t.Fatalf("transport errors = %+v, want one connection_refused", setup.UpstreamErrors)
	}

	labels := []string{
		string(upstreamErrorConnectionRefused),
		http.MethodGet,
		string(pathClassBlob),
		string(clientClassOther),
		"run-1",
		string(phaseSetup),
	}
	if got := testutil.ToFloat64(observer.upstreamErrors.WithLabelValues(labels...)); got != 1 {
		t.Fatalf("upstream error metric = %v, want 1", got)
	}
}

func TestClassifyUpstreamError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want upstreamErrorReason
	}{
		{name: "timeout", err: context.DeadlineExceeded, want: upstreamErrorTimeout},
		{name: "connection refused", err: syscall.ECONNREFUSED, want: upstreamErrorConnectionRefused},
		{name: "connection reset", err: syscall.ECONNRESET, want: upstreamErrorConnectionReset},
		{name: "other", err: errors.New("upstream failed"), want: upstreamErrorOther},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyUpstreamError(test.err); got != test.want {
				t.Fatalf("classifyUpstreamError() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestProxyCapturesPhaseAtRequestStart(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseResponse := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-releaseResponse

		_, _ = io.WriteString(w, "payload") //nolint:errcheck // Test server writes are asserted through the client response.
	}))
	defer upstream.Close()

	controller, observer, handler := testProxy(t, upstream.URL)
	if err := controller.set(phaseBaseline); err != nil {
		t.Fatalf("set baseline: %v", err)
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		handler.ServeHTTP(
			httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/v2/acme/app/blobs/"+testDigest(), nil),
		)
	}()

	<-requestStarted

	if err := controller.set(phaseGantryCold); !errors.Is(err, errPhaseInflight) {
		t.Fatalf("set gantry cold with request in flight error = %v, want errPhaseInflight", err)
	}

	close(releaseResponse)
	<-done

	if err := controller.set(phaseGantryCold); err != nil {
		t.Fatalf("set gantry cold after request completion: %v", err)
	}

	snapshot := observer.snapshot(time.Now())
	if got := snapshot.Totals.ByPhase[phaseBaseline].RequestsCompleted; got != 1 {
		t.Fatalf("baseline requests = %d, want 1", got)
	}

	if got := snapshot.Totals.ByPhase[phaseGantryCold].RequestsCompleted; got != 0 {
		t.Fatalf("gantry cold requests = %d, want 0", got)
	}
}

func TestProxyFollowsRedirectAndCountsFinalBody(t *testing.T) {
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "redirected-payload") //nolint:errcheck // Test server writes are asserted through the client response.
	}))
	defer blob.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, blob.URL+"/content", http.StatusTemporaryRedirect)
	}))
	defer upstream.Close()

	_, observer, handler := testProxy(t, upstream.URL)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v2/acme/app/blobs/"+testDigest(), nil))

	if response.Code != http.StatusOK || response.Body.String() != "redirected-payload" {
		t.Fatalf("response = (%d, %q)", response.Code, response.Body.String())
	}

	labels := []string{string(pathClassBlob), string(clientClassOther), "200", "run-1", string(phaseSetup)}
	if got := testutil.ToFloat64(observer.bytesUpstream.WithLabelValues(labels...)); got != 18 {
		t.Fatalf("upstream bytes = %v, want 18", got)
	}
}

func TestBearerChallengeRefreshesOnceAndReusesToken(t *testing.T) {
	var (
		tokenRequests atomic.Int64
		dataRequests  atomic.Int64
	)

	var upstream *httptest.Server

	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/token" {
			tokenRequests.Add(1)

			if r.Header.Get("Authorization") == "" {
				t.Error("token request has no Authorization header")
			}

			if got := r.URL.Query().Get("scope"); got != "repository:acme/app:pull" {
				t.Errorf("token scope = %q, want repository:acme/app:pull", got)
			}

			if err := json.NewEncoder(w).Encode(tokenResponse{AccessToken: "demo-token", ExpiresIn: 3600}); err != nil {
				t.Errorf("encode token response: %v", err)
			}

			return
		}

		dataRequests.Add(1)

		if r.Header.Get("Authorization") != "Bearer demo-token" {
			w.Header().Set(
				"WWW-Authenticate",
				`Bearer realm="`+upstream.URL+`/oauth2/token",service="registry",scope="repository:acme/app:pull"`,
			)
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		_, _ = io.WriteString(w, "manifest") //nolint:errcheck // Test server writes are asserted through the client response.
	}))
	defer upstream.Close()

	_, observer, handler := testProxyWithAuth(t, upstream.URL, "auto")

	for requestIndex := 0; requestIndex < 2; requestIndex++ {
		response := httptest.NewRecorder()
		handler.ServeHTTP(
			response,
			httptest.NewRequest(http.MethodGet, "/v2/acme/app/manifests/"+testDigest(), nil),
		)

		if response.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, body = %q", requestIndex+1, response.Code, response.Body.String())
		}
	}

	if got := tokenRequests.Load(); got != 1 {
		t.Fatalf("token requests = %d, want 1", got)
	}

	if got := dataRequests.Load(); got != 3 {
		t.Fatalf("data requests = %d, want 3", got)
	}

	if got := testutil.ToFloat64(observer.authRefresh.WithLabelValues("success", "run-1", string(phaseSetup))); got != 1 {
		t.Fatalf("successful auth refreshes = %v, want 1", got)
	}
}

func TestProxyRecordsPartialClientWrite(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", copyBufferBytes+17)) //nolint:errcheck // The failing proxy client drives this test.
	}))
	defer upstream.Close()

	_, observer, handler := testProxy(t, upstream.URL)
	writer := &failingResponseWriter{header: make(http.Header), failAfter: 3}
	handler.ServeHTTP(
		writer,
		httptest.NewRequest(http.MethodGet, "/v2/acme/app/blobs/"+testDigest(), nil),
	)

	labels := []string{string(pathClassBlob), string(clientClassOther), "client_closed", "run-1", string(phaseSetup)}
	clientBytes := testutil.ToFloat64(observer.bytesToClient.WithLabelValues(labels...))
	upstreamBytes := testutil.ToFloat64(observer.bytesUpstream.WithLabelValues(labels...))

	if clientBytes != 3 {
		t.Fatalf("client bytes = %v, want 3", clientBytes)
	}

	if upstreamBytes < clientBytes {
		t.Fatalf("upstream bytes = %v, want at least client bytes %v", upstreamBytes, clientBytes)
	}
}

func TestProxyRejectsWriteMethods(t *testing.T) {
	upstreamCalls := 0

	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		upstreamCalls++
	}))
	defer upstream.Close()

	_, observer, handler := testProxy(t, upstream.URL)
	response := httptest.NewRecorder()
	handler.ServeHTTP(
		response,
		httptest.NewRequest(http.MethodPut, "/v2/acme/app/manifests/latest", strings.NewReader("body")),
	)

	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}

	if upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", upstreamCalls)
	}

	if got := observer.snapshot(time.Now()).Totals.RequestsCompleted; got != 0 {
		t.Fatalf("observed requests = %d, want 0", got)
	}
}

func TestClassifyRequest(t *testing.T) {
	digest := testDigest()
	tests := []struct {
		path       string
		wantClass  pathClass
		wantDigest string
	}{
		{path: "/v2/", wantClass: pathClassPing},
		{path: "/v2/acme/app/manifests/latest", wantClass: pathClassManifestByTag},
		{path: "/v2/acme/app/manifests/" + digest, wantClass: pathClassManifestByDigest, wantDigest: digest},
		{path: "/v2/acme/app/blobs/" + digest, wantClass: pathClassBlob, wantDigest: digest},
		{path: "/v2/acme/app/blobs/uploads/1", wantClass: pathClassOther},
	}

	for _, test := range tests {
		gotClass, gotDigest := classifyRequest(test.path)
		if gotClass != test.wantClass || gotDigest != test.wantDigest {
			t.Errorf("classifyRequest(%q) = (%q, %q), want (%q, %q)", test.path, gotClass, gotDigest, test.wantClass, test.wantDigest)
		}
	}
}

func testProxy(t *testing.T, upstreamURL string) (*phaseController, *observer, http.Handler) {
	t.Helper()

	return testProxyWithAuth(t, upstreamURL, "basic")
}

func testProxyWithAuth(t *testing.T, upstreamURL, authMode string) (*phaseController, *observer, http.Handler) {
	t.Helper()

	return testProxyWithClient(t, upstreamURL, authMode, http.DefaultClient)
}

func testProxyWithClient(t *testing.T, upstreamURL, authMode string, client *http.Client) (*phaseController, *observer, http.Handler) {
	t.Helper()

	parsed, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}

	controller, err := newPhaseController("run-1")
	if err != nil {
		t.Fatalf("new phase controller: %v", err)
	}

	config := &config{
		upstream:              parsed,
		user:                  "demo-user",
		pass:                  "demo-pass",
		authMode:              authMode,
		maxTokenLife:          defaultMaxTokenLife,
		refreshSkewSecs:       defaultRefreshSkewSecs,
		throttleRetryAfterSec: 5,
	}
	observer := newObserver(prometheus.NewRegistry(), time.Now(), controller)

	return controller, observer, proxyHandler(config, newTokenCache(defaultRefreshSkewSecs), observer, client)
}

func testDigest() string {
	return "sha256:" + strings.Repeat("a", 64)
}

type failingResponseWriter struct {
	header    http.Header
	written   int
	failAfter int
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func (w *failingResponseWriter) Header() http.Header {
	return w.header
}

func (w *failingResponseWriter) WriteHeader(_ int) {}

func (w *failingResponseWriter) Write(value []byte) (int, error) {
	remaining := w.failAfter - w.written
	if remaining <= 0 {
		return 0, errors.New("client closed")
	}

	if len(value) > remaining {
		w.written += remaining

		return remaining, errors.New("client closed")
	}

	w.written += len(value)

	return len(value), nil
}
