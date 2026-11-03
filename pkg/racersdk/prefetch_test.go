// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk/internal/fakeracer"
	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

var (
	prefetchKeyA = strings.Repeat("a", 64)
	prefetchKeyB = strings.Repeat("b", 64)
)

func prefetchClient(t *testing.T, config fakeracer.PrefetchConfig) *Client {
	t.Helper()

	return testClient(t, rawClientPeer(t, fakeracer.NewHandlerWithPrefetch(nil, fakeracer.NewPrefetches(config))), 4)
}

type captured struct {
	method, uri, contentType, auth string
	body                           string
}

// scriptedClient answers every request with status, header, and body and
// records what it received.
func scriptedClient(t *testing.T, status int, header http.Header, body string) (*Client, *[]captured) {
	t.Helper()

	var (
		mu  sync.Mutex
		got []captured
	)

	c := testClient(t, rawClientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body) //nolint:errcheck // Recorded for assertions only.

		mu.Lock()

		got = append(got, captured{r.Method, r.RequestURI, r.Header.Get("Content-Type"), r.Header.Get("Authorization"), string(raw)})
		mu.Unlock()

		for k, v := range header {
			w.Header()[k] = v
		}

		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})), 4)

	return c, &got
}

const okStatus = `{"id":"e","cache_uid":"u","state":"PARTIAL","reason":null,"priority":"high","generation":2,"sequence":5,"settled":true,` +
	`"ttl_seconds":600,"expires_at_ms":1793750400000,"created_at_ms":1793749800000,"updated_at_ms":1793749810000,` +
	`"refs_total":2,"refs_succeeded":1,"refs_failed":1,"refs_pending":0,"bytes":{"logical_requested":1,"aligned_requested":16777216,` +
	`"already_resident":2,"queued":3,"in_flight":4,"joined":5,"fetched_from_origin":6,"admitted":7,"failed":8},` +
	`"coverage":{"resident_bytes":9,"observed_at_ms":1793749811000},` +
	`"refs":[{"index":1,"key":"k","etag":null,"offset":0,"length":16777216,"outcome":"failed","error":"version_changed"}],` +
	`"next_cursor":"c1","future_field":true}`

func TestPrefetchRequestEncoding(t *testing.T) {
	c, got := scriptedClient(t, http.StatusCreated, http.Header{"Content-Type": {"application/json"}}, okStatus)

	s, err := c.Prefetch(context.Background(), "epoch-17", PrefetchSpec{
		CacheUID: "u", Priority: "high", Generation: 2, Sequence: 5, TTL: 1500 * time.Millisecond,
		MaxPendingBytes: 1 << 30, Authorization: "Bearer x",
		Refs: []PrefetchRef{{Key: prefetchKeyA, ETag: `"v"`, Offset: 1, Length: 2, Metadata: "m"}, {Key: prefetchKeyB}},
	})
	if err != nil {
		t.Fatal(err)
	}

	want := `{"cache_uid":"u","priority":"high","generation":2,"sequence":5,"ttl_seconds":2,"max_pending_bytes":1073741824,` +
		`"refs":[{"key":"` + prefetchKeyA + `","etag":"\"v\"","offset":1,"length":2,"metadata":"m"},{"key":"` + prefetchKeyB + `"}]}`

	r := (*got)[0]
	if r.method != "PUT" || r.uri != "/v1/prefetches/epoch-17" || r.contentType != "application/json" || r.auth != "Bearer x" || r.body != want {
		t.Fatalf("request: %+v", r)
	}

	if !s.Created || s.State != PrefetchPartial || s.Reason != "" || s.Generation != 2 || s.TTL != 10*time.Minute ||
		s.ExpiresAt.UnixMilli() != 1793750400000 || s.Bytes.Failed != 8 || s.Coverage.ResidentBytes != 9 ||
		len(s.Refs) != 1 || s.Refs[0].Index != 1 || s.Refs[0].ETag != "" || s.Refs[0].Error != "version_changed" || s.NextCursor != "c1" {
		t.Fatalf("status: %+v", s)
	}

	if _, err := c.Prefetch(context.Background(), "min", PrefetchSpec{Refs: []PrefetchRef{{Key: prefetchKeyA}}}); err != nil {
		t.Fatal(err)
	}

	if r := (*got)[1]; r.auth != "" || r.body != `{"generation":0,"sequence":0,"refs":[{"key":"`+prefetchKeyA+`"}]}` {
		t.Fatalf("minimal: %+v", r)
	}

	if _, err := c.PrefetchStatus(context.Background(), "e", StatusOptions{Wait: 30 * time.Second, Coverage: true, Refs: "failed", Cursor: "a/b"}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.CancelPrefetch(context.Background(), "e"); err != nil {
		t.Fatal(err)
	}

	if r := (*got)[2]; r.method != "GET" || r.uri != "/v1/prefetches/e?wait=30s&coverage=1&refs=failed&cursor=a%2Fb" || r.body != "" || r.contentType != "" {
		t.Fatalf("status request: %+v", r)
	}

	if r := (*got)[3]; r.method != "DELETE" || r.uri != "/v1/prefetches/e" || r.body != "" {
		t.Fatalf("cancel request: %+v", r)
	}
}

func TestPrefetchInvalidArguments(t *testing.T) {
	c := testClient(t, "/nonexistent/socket", 1)
	ref := []PrefetchRef{{Key: prefetchKeyA}}
	ctx := context.Background()

	for name, spec := range map[string]PrefetchSpec{
		"no refs":        {},
		"bad key":        {Refs: []PrefetchRef{{Key: "ABC"}}},
		"upper key":      {Refs: []PrefetchRef{{Key: strings.Repeat("A", 64)}}},
		"weak etag":      {Refs: []PrefetchRef{{Key: prefetchKeyA, ETag: `W/"x"`}}},
		"negative off":   {Refs: []PrefetchRef{{Key: prefetchKeyA, Offset: -1}}},
		"negative len":   {Refs: []PrefetchRef{{Key: prefetchKeyA, Length: -1}}},
		"bad metadata":   {Refs: []PrefetchRef{{Key: prefetchKeyA, Metadata: "a\nb"}}},
		"priority":       {Priority: "urgent", Refs: ref},
		"generation":     {Generation: 1 << 53, Refs: ref},
		"sequence":       {Sequence: 1 << 53, Refs: ref},
		"negative ttl":   {TTL: -1, Refs: ref},
		"negative bytes": {MaxPendingBytes: -1, Refs: ref},
		"authorization":  {Authorization: "x\r\n", Refs: ref},
	} {
		if _, err := c.Prefetch(ctx, "ok", spec); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}

	for _, id := range []string{"", ".", "..", "a/b", strings.Repeat("x", 129)} {
		if _, err := c.Prefetch(ctx, id, PrefetchSpec{Refs: ref}); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("Prefetch(%q): %v", id, err)
		}

		if _, err := c.PrefetchStatus(ctx, id, StatusOptions{}); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("PrefetchStatus(%q): %v", id, err)
		}

		if _, err := c.CancelPrefetch(ctx, id); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("CancelPrefetch(%q): %v", id, err)
		}
	}

	for _, opts := range []StatusOptions{{Wait: -1}, {Refs: "some"}} {
		if _, err := c.PrefetchStatus(ctx, "ok", opts); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%+v: %v", opts, err)
		}
	}

	// No socket: a transport failure, not a prefetch error.
	_, err := c.ListPrefetches(ctx)

	var pe *PrefetchError
	if !errors.Is(err, ErrUnavailable) || errors.As(err, &pe) {
		t.Fatal(err)
	}
}

func TestPrefetchErrorMapping(t *testing.T) {
	jsonHeader := http.Header{"Content-Type": {"application/json"}}

	for _, tt := range []struct {
		name     string
		status   int
		header   http.Header
		body     string
		code     string
		retry    time.Duration
		sentinel error
	}{
		{"conflict", 409, jsonHeader, `{"code":"stale_sequence","message":"sequence 3 is older than current 5"}`, "stale_sequence", 0, nil},
		{"bad request", 400, jsonHeader, `{"code":"invalid_request","message":"x"}`, "invalid_request", 0, ErrInvalidRequest},
		{"too large", 413, jsonHeader, `{"code":"too_large","message":"x"}`, "too_large", 0, ErrInvalidRequest},
		{"not found", 404, jsonHeader, `{"code":"not_found","message":"x"}`, "not_found", 0, ErrNotFound},
		{"ticket limit", 429, http.Header{"Content-Type": {"application/json"}, "Retry-After": {"7"}}, `{"code":"ticket_limit","message":"x"}`, "ticket_limit", 7 * time.Second, ErrUnavailable},
		{"bad retry after", 429, http.Header{"Retry-After": {"soon"}}, `{"code":"pending_limit","message":"x"}`, "pending_limit", 0, ErrUnavailable},
		{"unavailable no body", 503, nil, ``, "", 0, ErrUnavailable},
		{"html body", 500, http.Header{"Content-Type": {"text/html"}}, `<html>`, "", 0, nil},
		{"method", 405, http.Header{"Allow": {"GET"}}, `{"code":"invalid_request","message":"m"}`, "invalid_request", 0, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := scriptedClient(t, tt.status, tt.header, tt.body)

			_, err := c.Prefetch(context.Background(), "a", PrefetchSpec{Refs: []PrefetchRef{{Key: prefetchKeyA}}})

			var pe *PrefetchError
			if !errors.As(err, &pe) || pe.Status != tt.status || pe.Code != tt.code || pe.RetryAfter != tt.retry || pe.Message == "" {
				t.Fatalf("%#v", err)
			}

			for _, s := range []error{ErrInvalidRequest, ErrNotFound, ErrUnavailable} {
				if errors.Is(err, s) != (s == tt.sentinel) {
					t.Fatalf("errors.Is(%v) = %v", s, s != tt.sentinel)
				}
			}

			if !strings.Contains(err.Error(), "HTTP") {
				t.Fatal(err.Error())
			}
		})
	}
}

func TestPrefetchProtocolErrors(t *testing.T) {
	for _, body := range []string{`not json`, `{"id":1}`} {
		c, _ := scriptedClient(t, 200, nil, body)

		_, err := c.PrefetchStatus(context.Background(), "a", StatusOptions{})

		var pe *PrefetchError
		if err == nil || errors.As(err, &pe) || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("%q: %v", body, err)
		}
	}

	c, _ := scriptedClient(t, 200, nil, `{"prefetches":[{"id":"x"}]}`+strings.Repeat(" ", maxPrefetchResponse))
	if _, err := c.ListPrefetches(context.Background()); err == nil {
		t.Fatal("oversized response accepted")
	}
}

func TestPrefetchIdempotencyAgainstFake(t *testing.T) {
	c := prefetchClient(t, fakeracer.PrefetchConfig{})
	ctx := context.Background()
	spec := PrefetchSpec{Generation: 1, Sequence: 1, Refs: []PrefetchRef{{Key: prefetchKeyA}}}

	s, err := c.Prefetch(ctx, "t", spec)
	if err != nil || !s.Created || s.State != PrefetchSucceeded {
		t.Fatal(s, err)
	}

	s, err = c.Prefetch(ctx, "t", spec)
	if err != nil || s.Created {
		t.Fatal(s, err)
	}

	changed := spec
	changed.Refs = []PrefetchRef{{Key: prefetchKeyB}}

	for _, tt := range []struct {
		spec PrefetchSpec
		code string
	}{
		{changed, wire.PrefetchPayloadMismatch},
		{PrefetchSpec{Generation: 1, Sequence: 1, Authorization: "a", Refs: spec.Refs}, wire.PrefetchPayloadMismatch},
		{PrefetchSpec{Generation: 1, Sequence: 0, Refs: spec.Refs}, wire.PrefetchStaleSequence},
		{PrefetchSpec{Generation: 0, Sequence: 9, Refs: spec.Refs}, wire.PrefetchStaleGeneration},
	} {
		var pe *PrefetchError
		if _, err := c.Prefetch(ctx, "t", tt.spec); !errors.As(err, &pe) || pe.Status != 409 || pe.Code != tt.code {
			t.Fatalf("%+v: %v", tt.spec, err)
		}
	}

	s, err = c.CancelPrefetch(ctx, "t")
	if err != nil || s.State != PrefetchCanceled || s.Reason != "client" {
		t.Fatal(s, err)
	}

	if _, err := c.PrefetchStatus(ctx, "missing", StatusOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}

	list, err := c.ListPrefetches(ctx)
	if err != nil || len(list) != 1 || list[0].ID != "t" || list[0].State != PrefetchCanceled {
		t.Fatal(list, err)
	}
}

func TestPrefetchStatusWaitAndPaging(t *testing.T) {
	var release atomic.Bool

	c := prefetchClient(t, fakeracer.PrefetchConfig{RefsPage: 2, Outcome: func(_ context.Context, _ string, ref wire.PrefetchRef) fakeracer.PrefetchOutcome {
		switch {
		case ref.Key == prefetchKeyB:
			return fakeracer.PrefetchOutcome{Error: "forbidden"}
		case !release.Load():
			return fakeracer.PrefetchOutcome{Pending: true}
		default:
			return fakeracer.PrefetchOutcome{ETag: `"v"`}
		}
	}})
	ctx := context.Background()

	var refs []PrefetchRef

	for i := range 5 {
		key := prefetchKeyA
		if i%2 == 1 {
			key = prefetchKeyB
		}

		refs = append(refs, PrefetchRef{Key: key, Offset: int64(i), Length: 1})
	}

	s, err := c.Prefetch(ctx, "w", PrefetchSpec{Refs: refs})
	if err != nil || s.Settled || s.State != PrefetchRunning || s.RefsPending != 3 {
		t.Fatal(s, err)
	}

	s, err = c.PrefetchStatus(ctx, "w", StatusOptions{Wait: 20 * time.Millisecond})
	if err != nil || s.Settled {
		t.Fatal(s, err)
	}

	go func() { time.Sleep(20 * time.Millisecond); release.Store(true) }()

	s, err = c.PrefetchStatus(ctx, "w", StatusOptions{Wait: 5 * time.Second, Coverage: true, Refs: "all"})
	if err != nil || !s.Settled || s.State != PrefetchPartial || s.Coverage == nil || len(s.Refs) != 2 || s.NextCursor == "" {
		t.Fatal(s, err)
	}

	var indexes []int
	for _, r := range s.Refs {
		indexes = append(indexes, r.Index)
	}

	for s.NextCursor != "" {
		s, err = c.PrefetchStatus(ctx, "w", StatusOptions{Refs: "all", Cursor: s.NextCursor})
		if err != nil {
			t.Fatal(err)
		}

		for _, r := range s.Refs {
			indexes = append(indexes, r.Index)
		}
	}

	if len(indexes) != 5 || indexes[4] != 4 {
		t.Fatal(indexes)
	}

	s, err = c.PrefetchStatus(ctx, "w", StatusOptions{Refs: "failed"})
	if err != nil || len(s.Refs) != 2 || s.Refs[0].Error != "forbidden" || s.Refs[0].ETag != "" || s.NextCursor != "" {
		t.Fatal(s, err)
	}
}

func TestPrefetchWindow(t *testing.T) {
	var (
		mu   sync.Mutex
		seen [][2]uint64
		fail atomic.Bool
	)

	c := prefetchClient(t, fakeracer.PrefetchConfig{Admit: func(_ string, r wire.PrefetchRequest) *fakeracer.PrefetchReject {
		mu.Lock()

		seen = append(seen, [2]uint64{r.Generation, r.Sequence})
		mu.Unlock()

		if fail.Load() {
			return &fakeracer.PrefetchReject{Status: 503, Code: wire.PrefetchUnavailable}
		}

		return nil
	}})
	ctx := context.Background()
	refs := []PrefetchRef{{Key: prefetchKeyA}}

	w := c.NewPrefetchWindow("win", PrefetchSpec{Generation: 3, Sequence: 7, Priority: "low", Refs: []PrefetchRef{{Key: prefetchKeyB}}})
	if w.ID() != "win" {
		t.Fatal(w.ID())
	}

	if g, s := w.Position(); g != 3 || s != 7 {
		t.Fatal(g, s)
	}

	if s, err := w.Update(ctx, refs); err != nil || s.Generation != 3 || s.Sequence != 7 || s.Priority != "low" || !s.Created {
		t.Fatal(s, err)
	}

	if s, err := w.Update(ctx, refs); err != nil || s.Sequence != 8 {
		t.Fatal(s, err)
	}

	// Invalid refs never reach Racer and do not consume a position.
	if _, err := w.Update(ctx, nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatal(err)
	}

	fail.Store(true)

	if _, err := w.Update(ctx, refs); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}

	fail.Store(false)

	// A failed call still consumes its position, since it may have applied.
	if s, err := w.Update(ctx, []PrefetchRef{{Key: prefetchKeyB}}); err != nil || s.Sequence != 10 {
		t.Fatal(s, err)
	}

	if s, err := w.Reset(ctx, refs); err != nil || s.Generation != 4 || s.Sequence != 0 {
		t.Fatal(s, err)
	}

	if s, err := w.Update(ctx, refs); err != nil || s.Generation != 4 || s.Sequence != 1 {
		t.Fatal(s, err)
	}

	want := [][2]uint64{{3, 7}, {3, 8}, {3, 9}, {3, 10}, {4, 0}, {4, 1}}

	mu.Lock()
	defer mu.Unlock()

	if len(seen) != len(want) {
		t.Fatal(seen)
	}

	for i := range want {
		if seen[i] != want[i] {
			t.Fatal(seen)
		}
	}
}

func TestPrefetchWindowExhaustion(t *testing.T) {
	c := testClient(t, "/nonexistent/socket", 1)
	refs := []PrefetchRef{{Key: prefetchKeyA}}

	w := c.NewPrefetchWindow("x", PrefetchSpec{Generation: wire.PrefetchMaxCounter, Sequence: wire.PrefetchMaxCounter})
	w.submitted = true

	if _, err := w.Update(context.Background(), refs); !errors.Is(err, ErrInvalidRequest) {
		t.Fatal(err)
	}

	if _, err := w.Reset(context.Background(), refs); !errors.Is(err, ErrInvalidRequest) {
		t.Fatal(err)
	}
}

func TestPrefetchWindowConcurrentUpdates(t *testing.T) {
	c := prefetchClient(t, fakeracer.PrefetchConfig{})
	w := c.NewPrefetchWindow("c", PrefetchSpec{})

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := w.Update(context.Background(), []PrefetchRef{{Key: prefetchKeyA}}); err != nil {
				t.Error(err)
			}
		})
	}

	wg.Wait()

	if g, s := w.Position(); g != 0 || s != 7 {
		t.Fatal(g, s)
	}
}

func TestSplitPrefetch(t *testing.T) {
	// Offsets 1..10 all encode to the same length.
	refs := make([]PrefetchRef, 10)
	for i := range refs {
		refs[i] = PrefetchRef{Key: prefetchKeyA, Offset: int64(i + 1)}
	}

	spec := PrefetchSpec{Priority: "high", Generation: 1, Sequence: 2, Authorization: "a", Refs: refs}

	parts := SplitPrefetch("job", spec, 4, 0)
	if len(parts) != 3 || parts[0].ID != "job.0" || parts[2].ID != "job.2" || len(parts[0].Spec.Refs) != 4 || len(parts[2].Spec.Refs) != 2 {
		t.Fatal(parts)
	}

	for _, p := range parts {
		if p.Spec.Priority != "high" || p.Spec.Generation != 1 || p.Spec.Sequence != 2 || p.Spec.Authorization != "a" {
			t.Fatal(p.Spec)
		}
	}

	if whole := SplitPrefetch("job", spec, 0, 0); len(whole) != 1 || whole[0].ID != "job.0" || len(whole[0].Spec.Refs) != 10 {
		t.Fatal(whole)
	}

	// Body limit: every part body fits and parts preserve order.
	one, err := prefetchBody(PrefetchSpec{Priority: "high", Generation: 1, Sequence: 2, Refs: refs[:3]})
	if err != nil {
		t.Fatal(err)
	}

	parts = SplitPrefetch("job", spec, 100, len(one))
	if len(parts) != 4 {
		t.Fatal(len(parts))
	}

	var offsets []int64

	for _, p := range parts {
		body, err := prefetchBody(p.Spec)
		if err != nil || len(body) > len(one) {
			t.Fatal(len(body), err)
		}

		var decoded wire.PrefetchRequest
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatal(err)
		}

		for _, r := range p.Spec.Refs {
			offsets = append(offsets, r.Offset)
		}
	}

	for i, o := range offsets {
		if o != int64(i+1) {
			t.Fatal(offsets)
		}
	}

	// A body exactly at the limit is one part.
	if exact := SplitPrefetch("job", PrefetchSpec{Priority: "high", Generation: 1, Sequence: 2, Refs: refs[:3]}, 0, len(one)); len(exact) != 1 {
		t.Fatal(exact)
	}

	for name, invalid := range map[string][]PrefetchPart{
		"ref too large": SplitPrefetch("job", spec, 0, 50),
		"invalid spec":  SplitPrefetch("job", PrefetchSpec{}, 0, 0),
		"invalid ref":   SplitPrefetch("job", PrefetchSpec{Refs: []PrefetchRef{{Key: "x"}}}, 0, 0),
		"id too long":   SplitPrefetch(strings.Repeat("x", 127), spec, 0, 0),
		"invalid id":    SplitPrefetch("a/b", spec, 0, 0),
	} {
		if invalid != nil {
			t.Errorf("%s: %v", name, invalid)
		}
	}
}

func TestPrefetchSplitPartsAcceptedByFake(t *testing.T) {
	c := prefetchClient(t, fakeracer.PrefetchConfig{MaxRefs: 3})

	refs := make([]PrefetchRef, 7)
	for i := range refs {
		refs[i] = PrefetchRef{Key: prefetchKeyA, Offset: int64(i)}
	}

	for _, p := range SplitPrefetch("big", PrefetchSpec{Refs: refs}, 3, 0) {
		if _, err := c.Prefetch(context.Background(), p.ID, p.Spec); err != nil {
			t.Fatal(p.ID, err)
		}
	}

	list, err := c.ListPrefetches(context.Background())
	if err != nil || len(list) != 3 || list[2].ID != "big.2" {
		t.Fatal(list, err)
	}
}

func TestPrefetchAfterCloseAndCancel(t *testing.T) {
	block := make(chan struct{})

	c := testClient(t, rawClientPeer(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	})), 1)
	t.Cleanup(func() { close(block) })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if _, err := c.PrefetchStatus(ctx, "a", StatusOptions{Wait: time.Minute}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}

	if got := len(c.stat.slots); got != 0 {
		t.Fatal("retained admission", got)
	}

	done := make(chan error, 1)

	go func() {
		_, err := c.ListPrefetches(context.Background())
		done <- err
	}()

	time.Sleep(20 * time.Millisecond)

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	if err := <-done; !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}

	if _, err := c.ListPrefetches(context.Background()); !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
}
