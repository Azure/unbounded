// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/metrics"
	"github.com/Azure/unbounded/internal/gantry/mirror"
	"github.com/Azure/unbounded/pkg/racersdk"
)

// Most handler tests exercise single-attempt failure paths. Retry tests opt
// in explicitly with WithRetryBudget.
func TestMain(m *testing.M) {
	handlerDefaultRetryBudget = 0

	os.Exit(m.Run())
}

// retryClient injects failures before delegating to a real fake SDK client.
type retryClient struct {
	Client
	mu        sync.Mutex
	gets      int
	stats     int
	getErr    func(call int, opts []racersdk.ReadOptions) error
	statErr   func(call int) error
	getHook   func(ctx context.Context, call int, req racersdk.Request, opts []racersdk.ReadOptions) (*racersdk.Object, error, bool)
	afterCall func()
}

func (c *retryClient) Get(ctx context.Context, req racersdk.Request, opts ...racersdk.ReadOptions) (*racersdk.Object, error) {
	c.mu.Lock()
	c.gets++
	call := c.gets
	c.mu.Unlock()

	if c.afterCall != nil {
		defer c.afterCall()
	}

	if c.getErr != nil {
		if err := c.getErr(call, opts); err != nil {
			return nil, err
		}
	}

	if c.getHook != nil {
		if value, err, ok := c.getHook(ctx, call, req, opts); ok {
			return value, err
		}
	}

	return c.Client.Get(ctx, req, opts...)
}

func (c *retryClient) Stat(ctx context.Context, req racersdk.Request) (racersdk.Metadata, error) {
	c.mu.Lock()
	c.stats++
	call := c.stats
	c.mu.Unlock()

	if c.statErr != nil {
		if err := c.statErr(call); err != nil {
			return racersdk.Metadata{}, err
		}
	}

	return c.Client.Stat(ctx, req)
}

type retryStages struct {
	mu     sync.Mutex
	stages map[string]int
}

func (r *retryStages) observe(stage string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.stages == nil {
		r.stages = map[string]int{}
	}

	r.stages[stage]++
}

func (r *retryStages) count(stage string) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.stages[stage]
}

func retryHandler(client Client, budget time.Duration, stages *retryStages) *Handler {
	h := NewHandler(client, nil, nil, WithRetryBudget(budget), WithRetryObserver(stages.observe))
	h.retry.base = time.Millisecond
	h.retry.cap = 5 * time.Millisecond

	return h
}

// serveRetry runs ServeContent and reports whether the handler aborted.
func serveRetry(h *Handler, r *http.Request, ref ifaces.OriginRef) (rec *httptest.ResponseRecorder, aborted bool) {
	rec = httptest.NewRecorder()

	defer func() {
		if p := recover(); p != nil {
			if p != http.ErrAbortHandler {
				panic(p)
			}

			aborted = true
		}
	}()

	h.ServeContent(rec, r, ref)

	return rec, false
}

func retryRef(d digest.Digest) ifaces.OriginRef {
	return ifaces.OriginRef{Registry: "registry.example", Repository: "repo", Digest: d, Kind: ifaces.KindBlob}
}

func unavailable() error { return fmt.Errorf("overloaded: %w", racersdk.ErrUnavailable) }

func staticOrigin(t *testing.T, data []byte) racersdk.Origin {
	t.Helper()

	metadata := racerMetadata(t, racerDigest(data), len(data))

	return func(_ context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		if req.Head {
			return metadata, nil, nil
		}

		return metadata, io.NopCloser(bytes.NewReader(data[req.Offset:])), nil
	}
}

func TestRacerRetryGetTransientThenSuccess(t *testing.T) {
	data := []byte("abc")
	client := &retryClient{
		Client: racerFakeClient(t, staticOrigin(t, data)),
		getErr: func(call int, _ []racersdk.ReadOptions) error {
			if call <= 2 {
				return unavailable()
			}

			return nil
		},
	}

	var stages retryStages

	rec, aborted := serveRetry(retryHandler(client, 5*time.Second, &stages), httptest.NewRequest(http.MethodGet, "/", nil), retryRef(racerDigest(data)))
	if aborted || rec.Code != http.StatusOK || rec.Body.String() != "abc" {
		t.Fatalf("aborted=%v status=%d body=%q", aborted, rec.Code, rec.Body.String())
	}

	if client.gets != 3 || stages.count("get") != 2 {
		t.Fatalf("gets=%d retries=%d", client.gets, stages.count("get"))
	}
}

func TestRacerRetryStatTransientThenSuccess(t *testing.T) {
	data := []byte("abc")
	client := &retryClient{
		Client: racerFakeClient(t, staticOrigin(t, data)),
		statErr: func(call int) error {
			if call == 1 {
				return unavailable()
			}

			return nil
		},
	}

	var stages retryStages

	rec, aborted := serveRetry(retryHandler(client, 5*time.Second, &stages), httptest.NewRequest(http.MethodHead, "/", nil), retryRef(racerDigest(data)))
	if aborted || rec.Code != http.StatusOK || rec.Header().Get("Content-Length") != "3" {
		t.Fatalf("aborted=%v status=%d headers=%v", aborted, rec.Code, rec.Header())
	}

	if client.stats != 2 || stages.count("stat") != 1 {
		t.Fatalf("stats=%d retries=%d", client.stats, stages.count("stat"))
	}
}

func TestRacerRetryGivesUpAfterBudget(t *testing.T) {
	data := []byte("abc")
	client := &retryClient{
		Client: racerFakeClient(t, staticOrigin(t, data)),
		getErr: func(int, []racersdk.ReadOptions) error { return unavailable() },
	}

	var stages retryStages

	started := time.Now()

	rec, aborted := serveRetry(retryHandler(client, 50*time.Millisecond, &stages), httptest.NewRequest(http.MethodGet, "/", nil), retryRef(racerDigest(data)))
	if aborted || rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("aborted=%v status=%d", aborted, rec.Code)
	}

	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("exceeded budget: %v", elapsed)
	}

	if client.gets < 2 || stages.count("get") != client.gets-1 {
		t.Fatalf("gets=%d retries=%d", client.gets, stages.count("get"))
	}
}

func TestRacerRetryDisabledWithZeroBudget(t *testing.T) {
	data := []byte("abc")
	client := &retryClient{
		Client: racerFakeClient(t, staticOrigin(t, data)),
		getErr: func(int, []racersdk.ReadOptions) error { return unavailable() },
	}

	var stages retryStages

	rec, _ := serveRetry(retryHandler(client, 0, &stages), httptest.NewRequest(http.MethodGet, "/", nil), retryRef(racerDigest(data)))
	if rec.Code != http.StatusServiceUnavailable || client.gets != 1 || stages.count("get") != 0 {
		t.Fatalf("status=%d gets=%d", rec.Code, client.gets)
	}
}

func TestRacerRetryNotOnPermanentErrors(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{
		{racersdk.ErrNotFound, http.StatusNotFound},
		{racersdk.ErrUnauthorized, http.StatusUnauthorized},
		{racersdk.ErrForbidden, http.StatusForbidden},
		{fmt.Errorf("%w: %w", racersdk.ErrDestination, racersdk.ErrUnavailable), http.StatusServiceUnavailable},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			data := []byte("abc")
			client := &retryClient{
				Client: racerFakeClient(t, staticOrigin(t, data)),
				getErr: func(int, []racersdk.ReadOptions) error { return tc.err },
			}

			var stages retryStages

			rec, _ := serveRetry(retryHandler(client, 5*time.Second, &stages), httptest.NewRequest(http.MethodGet, "/", nil), retryRef(racerDigest(data)))
			if rec.Code != tc.status || client.gets != 1 || stages.count("get") != 0 {
				t.Fatalf("status=%d gets=%d retries=%d", rec.Code, client.gets, stages.count("get"))
			}
		})
	}
}

func TestRacerRetryStopsOnClientCancel(t *testing.T) {
	data := []byte("abc")
	ctx, cancel := context.WithCancel(context.Background())

	client := &retryClient{
		Client:    racerFakeClient(t, staticOrigin(t, data)),
		getErr:    func(int, []racersdk.ReadOptions) error { return unavailable() },
		afterCall: cancel,
	}

	var stages retryStages

	h := retryHandler(client, 10*time.Second, &stages)
	h.retry.base = time.Second
	h.retry.cap = time.Second
	started := time.Now()

	rec, _ := serveRetry(h, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx), retryRef(racerDigest(data)))
	if rec.Code != http.StatusServiceUnavailable || client.gets != 1 || time.Since(started) > 500*time.Millisecond {
		t.Fatalf("status=%d gets=%d elapsed=%v", rec.Code, client.gets, time.Since(started))
	}
}

// truncatingOrigin serves data, but the first body read fails at cut.
func truncatingOrigin(t *testing.T, data []byte, cut int, calls *atomic.Int32) racersdk.Origin {
	t.Helper()

	metadata := racerMetadata(t, racerDigest(data), len(data))

	return func(_ context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		if req.Head {
			return metadata, nil, nil
		}

		end := int64(len(data))
		if req.Length > 0 {
			end = min(end, req.Offset+req.Length)
		}

		if calls.Add(1) == 1 {
			return metadata, io.NopCloser(io.MultiReader(bytes.NewReader(data[req.Offset:cut]), racerReadError{})), nil
		}

		return metadata, io.NopCloser(bytes.NewReader(data[req.Offset:end])), nil
	}
}

func TestRacerRetryResumesMidBody(t *testing.T) {
	for _, offset := range []int{0, 65536} {
		t.Run(fmt.Sprint(offset), func(t *testing.T) {
			data := racerDistinctPayload(7, 1024*1024)

			var calls atomic.Int32

			client := &retryClient{Client: racerFakeClient(t, truncatingOrigin(t, data, len(data)/2+123, &calls))}

			var stages retryStages

			r := httptest.NewRequest(http.MethodGet, "/", nil)
			ref := retryRef(racerDigest(data))
			ref.Offset = int64(offset)

			rec, aborted := serveRetry(retryHandler(client, 5*time.Second, &stages), r, ref)
			if aborted || !bytes.Equal(rec.Body.Bytes(), data[offset:]) {
				t.Fatalf("aborted=%v status=%d bytes=%d", aborted, rec.Code, rec.Body.Len())
			}

			if stages.count("write_body") < 1 || client.gets < 2 {
				t.Fatalf("retries=%v gets=%d", stages.stages, client.gets)
			}
		})
	}
}

func TestRacerRetryResumeGivesUpAfterBudget(t *testing.T) {
	data := racerDistinctPayload(8, 1024*1024)
	metadata := racerMetadata(t, racerDigest(data), len(data))
	client := &retryClient{Client: racerFakeClient(t, func(_ context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		if req.Head {
			return metadata, nil, nil
		}

		return metadata, io.NopCloser(io.MultiReader(bytes.NewReader(data[req.Offset:req.Offset+1]), racerReadError{})), nil
	})}

	var stages retryStages

	rec, aborted := serveRetry(retryHandler(client, 50*time.Millisecond, &stages), httptest.NewRequest(http.MethodGet, "/", nil), retryRef(racerDigest(data)))
	if !aborted || rec.Body.Len() >= len(data) || !bytes.Equal(rec.Body.Bytes(), data[:rec.Body.Len()]) {
		t.Fatalf("aborted=%v bytes=%d", aborted, rec.Body.Len())
	}

	if stages.count("write_body") < 1 {
		t.Fatalf("retries=%v", stages.stages)
	}
}

func TestRacerRetryResumeMetadataMismatchAborts(t *testing.T) {
	data := racerDistinctPayload(9, 1024*1024)
	other := racerDistinctPayload(10, 2*1024*1024)

	var calls atomic.Int32

	origin := truncatingOrigin(t, data, len(data)/2, &calls)
	otherOrigin := staticOrigin(t, other)
	fake := racerFakeClient(t, func(ctx context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		if req.Key.String() == racerDigest(other).Hex() {
			return otherOrigin(ctx, req)
		}

		return origin(ctx, req)
	})

	otherReq, err := Request(retryRef(racerDigest(other)), "")
	if err != nil {
		t.Fatal(err)
	}

	client := &retryClient{
		Client: fake,
		getHook: func(ctx context.Context, call int, _ racersdk.Request, opts []racersdk.ReadOptions) (*racersdk.Object, error, bool) {
			if call == 1 {
				return nil, nil, false
			}
			// Simulate a resumed read that returns a different object.
			value, err := fake.Get(ctx, otherReq, racersdk.ReadOptions{Offset: opts[0].Offset})

			return value, err, true
		},
	}

	var stages retryStages

	rec, aborted := serveRetry(retryHandler(client, 5*time.Second, &stages), httptest.NewRequest(http.MethodGet, "/", nil), retryRef(racerDigest(data)))
	if !aborted || rec.Body.Len() >= len(data) || !bytes.Equal(rec.Body.Bytes(), data[:rec.Body.Len()]) {
		t.Fatalf("aborted=%v bytes=%d", aborted, rec.Body.Len())
	}

	if client.gets != 2 {
		t.Fatalf("gets=%d", client.gets)
	}
}

func TestRacerRetryMetrics(t *testing.T) {
	m := NewMetrics(metrics.New())
	m.MirrorRetry("get")
	m.MirrorRetry("write_body")
	m.MirrorRetry("unexpected")

	for stage, want := range map[string]float64{"get": 1, "write_body": 1, "other": 1, "stat": 0} {
		if got := testutil.ToFloat64(m.retries.WithLabelValues(stage)); got != want {
			t.Fatalf("%s retries = %v, want %v", stage, got, want)
		}
	}
}

func TestRacerRetryWaitBudget(t *testing.T) {
	var stages retryStages

	r := &retryState{policy: retryPolicy{budget: time.Second, base: time.Millisecond, cap: time.Millisecond, observe: stages.observe}, deadline: time.Now().Add(time.Second)}
	if !r.wait(context.Background(), "get") || stages.count("get") != 1 {
		t.Fatal("expected retry")
	}

	expired := &retryState{policy: r.policy, deadline: time.Now()}
	if expired.wait(context.Background(), "get") {
		t.Fatal("retried past budget")
	}
}

// Resume through WrapHTTP over real connections, including the splice and
// userspace fallback paths, so leftover SDK write deadlines cannot break it.
func TestRacerRetryResumesOverHTTP(t *testing.T) {
	for _, mode := range []string{"plaintext", "TLS", "fallback"} {
		for _, offset := range []int{0, 65536} {
			t.Run(fmt.Sprintf("%s/%d", mode, offset), func(t *testing.T) {
				data := racerDistinctPayload(11, int(racersdk.PageSize)+65539)

				var calls atomic.Int32

				client := racerFakeClient(t, truncatingOrigin(t, data, int(racersdk.PageSize)/2+17, &calls))
				trap := &racerDirectTrap{}

				var stages retryStages

				h := retryHandler(client, 5*time.Second, &stages)
				handler := WrapHTTP(mirror.New(racerConfig(), trap, trap, mirror.WithContentBackend(h)).Handler(), time.Second, nil)
				server, _ := handlerStreamServer(t, mode, handler)
				rangeHeader, status := handlerRange(offset)

				resp := racerRequest(t, server, http.MethodGet, "blobs", racerDigest(data), rangeHeader, "")
				if resp.StatusCode != status {
					t.Fatalf("status=%d", resp.StatusCode)
				}

				racerCheckStreamHash(t, resp, data, offset)

				if stages.count("write_body") < 1 {
					t.Fatalf("retries=%v", stages.stages)
				}
			})
		}
	}
}
