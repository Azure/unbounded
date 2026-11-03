// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package fakeracer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

var (
	keyA = strings.Repeat("a", 64)
	keyB = strings.Repeat("b", 64)
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)
}

type result struct {
	code   int
	header http.Header
	status wire.PrefetchStatus
	list   wire.PrefetchList
	err    wire.PrefetchErrorBody
}

func call(t *testing.T, h http.Handler, method, target, body string, header ...string) result {
	t.Helper()

	r := httptest.NewRequest(method, "http://racer"+target, strings.NewReader(body))
	if body == "" {
		r.ContentLength = 0
	} else {
		r.Header.Set("Content-Type", "application/json")
	}

	for i := 0; i+1 < len(header); i += 2 {
		if header[i+1] == "" {
			r.Header.Del(header[i])
		} else {
			r.Header.Set(header[i], header[i+1])
		}
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	out := result{code: w.Code, header: w.Header()}
	if w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%s %s: content type %q", method, target, w.Header().Get("Content-Type"))
	}

	raw := w.Body.Bytes()

	switch {
	case w.Code >= 300:
		if err := json.Unmarshal(raw, &out.err); err != nil || out.err.Code == "" {
			t.Fatalf("%s %s: bad error body %q", method, target, raw)
		}
	case target == wire.PrefetchPath:
		if err := json.Unmarshal(raw, &out.list); err != nil {
			t.Fatal(err)
		}
	default:
		if err := json.Unmarshal(raw, &out.status); err != nil {
			t.Fatal(err)
		}
	}

	return out
}

func spec(g, s uint64, keys ...string) string {
	request := wire.PrefetchRequest{Generation: g, Sequence: s}
	for _, k := range keys {
		request.Refs = append(request.Refs, wire.PrefetchRef{Key: k})
	}

	raw, err := json.Marshal(request)
	if err != nil {
		panic(err)
	}

	return string(raw)
}

func newFake(config PrefetchConfig) (*Prefetches, http.Handler, *fakeClock) {
	clock := &fakeClock{now: time.UnixMilli(1_700_000_000_000)}
	if config.Now == nil {
		config.Now = clock.Now
	}

	p := NewPrefetches(config)

	return p, NewHandlerWithPrefetch(nil, p), clock
}

func TestPrefetchIdempotencyTable(t *testing.T) {
	_, h, _ := newFake(PrefetchConfig{})
	path := wire.PrefetchPath + "/w"

	if r := call(t, h, "PUT", path, spec(1, 5, keyA)); r.code != 201 || r.status.State != "SUCCEEDED" || r.status.Generation != 1 || r.status.Sequence != 5 || !r.status.Settled {
		t.Fatalf("create: %+v", r)
	}

	for _, tt := range []struct {
		name string
		body string
		auth string
		code int
		err  string
	}{
		{"same", spec(1, 5, keyA), "", 200, ""},
		{"same pos different refs", spec(1, 5, keyB), "", 409, wire.PrefetchPayloadMismatch},
		{"same pos auth presence differs", spec(1, 5, keyA), "secret", 409, wire.PrefetchPayloadMismatch},
		{"older sequence", spec(1, 4, keyA), "", 409, wire.PrefetchStaleSequence},
		{"older generation", spec(0, 9, keyA), "", 409, wire.PrefetchStaleGeneration},
		{"newer sequence", spec(1, 6, keyB), "", 200, ""},
		{"newer generation lower sequence", spec(2, 0, keyA), "", 200, ""},
		{"stale after supersede", spec(1, 7, keyA), "", 409, wire.PrefetchStaleGeneration},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := call(t, h, "PUT", path, tt.body, "Authorization", tt.auth)
			if r.code != tt.code || r.err.Code != tt.err {
				t.Fatalf("%d %+v", r.code, r.err)
			}
		})
	}

	// Defaults are part of the canonical body, so an explicit default matches.
	if r := call(t, h, "PUT", path, `{"priority":"normal","generation":2,"sequence":0,"ttl_seconds":600,"refs":[{"key":"`+keyA+`","offset":0}]}`); r.code != 200 {
		t.Fatalf("explicit defaults: %+v", r)
	}
}

func TestPrefetchTerminalRestartAndCancel(t *testing.T) {
	_, h, _ := newFake(PrefetchConfig{})
	path := wire.PrefetchPath + "/t"

	if r := call(t, h, "DELETE", path, ""); r.code != 404 || r.err.Code != wire.PrefetchNotFound {
		t.Fatal(r)
	}

	call(t, h, "PUT", path, spec(0, 0, keyA))

	r := call(t, h, "DELETE", path, "")
	if r.code != 200 || r.status.State != "CANCELLED" || r.status.Reason == nil || *r.status.Reason != "client" || !r.status.Settled {
		t.Fatalf("cancel: %+v", r)
	}

	// The same request returns the retained canceled status.
	if r := call(t, h, "PUT", path, spec(0, 0, keyA)); r.code != 200 || r.status.State != "CANCELLED" {
		t.Fatalf("retained: %+v", r)
	}

	if r := call(t, h, "PUT", path, spec(0, 1, keyA)); r.code != 200 || r.status.State != "SUCCEEDED" || r.status.Reason != nil {
		t.Fatalf("restart: %+v", r)
	}
}

func TestPrefetchTTLExpiryAndRetention(t *testing.T) {
	_, h, clock := newFake(PrefetchConfig{MaxTTL: 10 * time.Second, Retention: time.Minute})
	path := wire.PrefetchPath + "/x"

	r := call(t, h, "PUT", path, `{"ttl_seconds":3600,"refs":[{"key":"`+keyA+`"}]}`)
	if r.status.TTLSeconds != 10 || r.status.ExpiresAtMs != clock.Now().Add(10*time.Second).UnixMilli() {
		t.Fatalf("ttl cap: %+v", r.status)
	}

	clock.Add(5 * time.Second)

	if r2 := call(t, h, "PUT", path, `{"ttl_seconds":3600,"refs":[{"key":"`+keyA+`"}]}`); r2.status.ExpiresAtMs != clock.Now().Add(10*time.Second).UnixMilli() {
		t.Fatalf("renew: %+v", r2.status)
	}

	clock.Add(10 * time.Second)

	r = call(t, h, "GET", path, "")
	if r.status.State != "CANCELLED" || *r.status.Reason != "expired" {
		t.Fatalf("expired: %+v", r.status)
	}

	clock.Add(time.Minute)

	if r := call(t, h, "GET", path, ""); r.code != 404 {
		t.Fatalf("retention: %+v", r)
	}
}

func TestPrefetchLimitsAndRejections(t *testing.T) {
	_, h, _ := newFake(PrefetchConfig{MaxTickets: 1, MaxRefs: 2, MaxBodyBytes: 4096})

	call(t, h, "PUT", wire.PrefetchPath+"/a", spec(0, 0, keyA))

	r := call(t, h, "PUT", wire.PrefetchPath+"/b", spec(0, 0, keyA))
	if r.code != 429 || r.err.Code != wire.PrefetchTicketLimit || r.header.Get("Retry-After") != "1" {
		t.Fatalf("ticket limit: %+v", r)
	}

	if r := call(t, h, "PUT", wire.PrefetchPath+"/a", spec(0, 1, keyA, keyA, keyB)); r.code != 413 || r.err.Code != wire.PrefetchTooLarge {
		t.Fatalf("refs: %+v", r)
	}

	if r := call(t, h, "PUT", wire.PrefetchPath+"/a", `{"refs":[{"key":"`+keyA+`","metadata":"`+strings.Repeat("m", 5000)+`"}]}`); r.code != 413 {
		t.Fatalf("body: %+v", r)
	}

	_, h, _ = newFake(PrefetchConfig{Passthrough: true})
	if r := call(t, h, "PUT", wire.PrefetchPath+"/a", spec(0, 0, keyA)); r.code != 409 || r.err.Code != wire.PrefetchPassthrough {
		t.Fatalf("passthrough: %+v", r)
	}

	_, h, _ = newFake(PrefetchConfig{CacheUID: "uid"})
	if r := call(t, h, "PUT", wire.PrefetchPath+"/a", `{"cache_uid":"other","refs":[{"key":"`+keyA+`"}]}`); r.code != 409 || r.err.Code != wire.PrefetchCacheUIDMismatch {
		t.Fatalf("uid: %+v", r)
	}

	if r := call(t, h, "PUT", wire.PrefetchPath+"/a", `{"cache_uid":"uid","refs":[{"key":"`+keyA+`"}]}`); r.code != 201 || r.status.CacheUID != "uid" {
		t.Fatalf("uid match: %+v", r)
	}

	_, h, _ = newFake(PrefetchConfig{Admit: func(string, wire.PrefetchRequest) *PrefetchReject {
		return &PrefetchReject{Status: 429, Code: wire.PrefetchPendingLimit, RetryAfter: 7}
	}})
	if r := call(t, h, "PUT", wire.PrefetchPath+"/a", spec(0, 0, keyA)); r.code != 429 || r.err.Code != wire.PrefetchPendingLimit || r.header.Get("Retry-After") != "7" {
		t.Fatalf("admit: %+v", r)
	}
}

func TestPrefetchBadRequests(t *testing.T) {
	_, h, _ := newFake(PrefetchConfig{})
	item := wire.PrefetchPath + "/a"
	ref := `{"key":"` + keyA + `"}`

	for _, tt := range []struct {
		name, method, target, body string
		header                     []string
	}{
		{"bad id", "PUT", wire.PrefetchPath + "/..", spec(0, 0, keyA), nil},
		{"bad id char", "GET", wire.PrefetchPath + "/a%20b", "", nil},
		{"no content type", "PUT", item, spec(0, 0, keyA), []string{"Content-Type", ""}},
		{"wrong content type", "PUT", item, spec(0, 0, keyA), []string{"Content-Type", "text/plain"}},
		{"expect", "PUT", item, spec(0, 0, keyA), []string{"Expect", "100-continue"}},
		{"content encoding", "PUT", item, spec(0, 0, keyA), []string{"Content-Encoding", "gzip"}},
		{"query on put", "PUT", item + "?wait=1s", spec(0, 0, keyA), nil},
		{"unknown field", "PUT", item, `{"refs":[` + ref + `],"extra":1}`, nil},
		{"unknown ref field", "PUT", item, `{"refs":[{"key":"` + keyA + `","x":1}]}`, nil},
		{"empty refs", "PUT", item, `{"refs":[]}`, nil},
		{"missing refs", "PUT", item, `{}`, nil},
		{"bad key", "PUT", item, `{"refs":[{"key":"ABC"}]}`, nil},
		{"weak etag", "PUT", item, `{"refs":[{"key":"` + keyA + `","etag":"W/\"x\""}]}`, nil},
		{"zero length", "PUT", item, `{"refs":[{"key":"` + keyA + `","length":0}]}`, nil},
		{"zero ttl", "PUT", item, `{"ttl_seconds":0,"refs":[` + ref + `]}`, nil},
		{"priority", "PUT", item, `{"priority":"urgent","refs":[` + ref + `]}`, nil},
		{"generation range", "PUT", item, `{"generation":9007199254740992,"refs":[` + ref + `]}`, nil},
		{"trailing", "PUT", item, `{"refs":[` + ref + `]} {}`, nil},
		{"not json", "PUT", item, `nope`, nil},
		{"unknown query", "GET", item + "?foo=1", "", nil},
		{"list query", "GET", wire.PrefetchPath + "?wait=1s", "", nil},
		{"auth on get", "GET", item, "", []string{"Authorization", "x"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if r := call(t, h, tt.method, tt.target, tt.body, tt.header...); r.code != 400 || r.err.Code != wire.PrefetchInvalidRequest {
				t.Fatalf("%+v", r)
			}
		})
	}

	for _, tt := range []struct{ method, target, allow string }{
		{"POST", item, "GET, PUT, DELETE"},
		{"PATCH", item, "GET, PUT, DELETE"},
		{"PUT", wire.PrefetchPath, "GET"},
		{"DELETE", wire.PrefetchPath, "GET"},
	} {
		if r := call(t, h, tt.method, tt.target, ""); r.code != 405 || r.header.Get("Allow") != tt.allow {
			t.Fatalf("%s %s: %+v", tt.method, tt.target, r)
		}
	}
}

func TestPrefetchOutcomesWaitAndPaging(t *testing.T) {
	var release atomic.Bool

	_, h, _ := newFake(PrefetchConfig{RefsPage: 2, Outcome: func(_ context.Context, _ string, ref wire.PrefetchRef) PrefetchOutcome {
		switch {
		case ref.Key == keyB:
			return PrefetchOutcome{Error: "not_found"}
		case ref.Offset == 1 && !release.Load():
			return PrefetchOutcome{Pending: true}
		default:
			return PrefetchOutcome{ETag: `"v"`, Size: 3 * wire.PageSize}
		}
	}})
	path := wire.PrefetchPath + "/p"

	body := `{"refs":[{"key":"` + keyA + `"},{"key":"` + keyB + `"},{"key":"` + keyA + `","offset":1,"length":10},{"key":"` + keyB + `","etag":"\"e\""},{"key":"` + keyA + `","offset":3221225472}]}`

	r := call(t, h, "PUT", path, body)
	if r.status.State != "RUNNING" || r.status.Settled || r.status.RefsPending != 1 || r.status.RefsFailed != 3 || r.status.RefsSucceeded != 1 {
		t.Fatalf("running: %+v", r.status)
	}

	if r.status.Bytes.LogicalRequested != 3*wire.PageSize+10 || r.status.Bytes.AlignedRequested != 3*wire.PageSize {
		t.Fatalf("bytes: %+v", r.status.Bytes)
	}

	start := time.Now()
	if r2 := call(t, h, "GET", path+"?wait=50ms", ""); r2.status.Settled || time.Since(start) < 50*time.Millisecond {
		t.Fatalf("wait timeout: %+v", r2.status)
	}

	go func() { time.Sleep(20 * time.Millisecond); release.Store(true) }()

	r = call(t, h, "GET", path+"?wait=5s&coverage=1&refs=all", "")
	if !r.status.Settled || r.status.State != "PARTIAL" || r.status.Coverage == nil || r.status.Coverage.ResidentBytes != r.status.Bytes.Admitted {
		t.Fatalf("settled: %+v", r.status)
	}

	if len(r.status.Refs) != 2 || r.status.NextCursor == nil || r.status.Refs[0].ETag == nil || *r.status.Refs[0].ETag != `"v"` || r.status.Refs[1].ETag != nil || *r.status.Refs[1].Error != "not_found" {
		t.Fatalf("page 1: %+v", r.status.Refs)
	}

	var all []wire.PrefetchRefStatus

	all = append(all, r.status.Refs...)
	for r.status.NextCursor != nil {
		r = call(t, h, "GET", path+"?refs=all&cursor="+*r.status.NextCursor, "")
		all = append(all, r.status.Refs...)
	}

	if len(all) != 5 || all[4].Index != 4 || *all[4].Error != "invalid_range" || all[2].Length != 10 {
		t.Fatalf("all refs: %+v", all)
	}

	r = call(t, h, "GET", path+"?refs=failed", "")
	if len(r.status.Refs) != 2 || r.status.Refs[0].Index != 1 || r.status.Refs[1].Index != 3 || r.status.NextCursor == nil {
		t.Fatalf("failed page: %+v", r.status.Refs)
	}

	cursor := *r.status.NextCursor

	call(t, h, "PUT", path, `{"sequence":1,"refs":[{"key":"`+keyA+`"}]}`)

	if r := call(t, h, "GET", path+"?refs=failed&cursor="+cursor, ""); r.code != 400 {
		t.Fatalf("stale cursor: %+v", r)
	}

	if r := call(t, h, "GET", path+"?cursor="+cursor, ""); r.code != 400 {
		t.Fatalf("cursor without refs: %+v", r)
	}
}

func TestPrefetchList(t *testing.T) {
	_, h, _ := newFake(PrefetchConfig{})

	if r := call(t, h, "GET", wire.PrefetchPath, ""); r.code != 200 || r.list.Prefetches == nil || len(r.list.Prefetches) != 0 {
		t.Fatalf("empty: %+v", r)
	}

	for _, id := range []string{"c", "a", "b"} {
		call(t, h, "PUT", wire.PrefetchPath+"/"+id, spec(0, 0, keyA))
	}

	r := call(t, h, "GET", wire.PrefetchPath, "")

	var ids []string
	for _, s := range r.list.Prefetches {
		ids = append(ids, s.ID)

		if s.Refs != nil || s.Coverage != nil || s.State != "SUCCEEDED" {
			t.Fatalf("summary: %+v", s)
		}
	}

	if strings.Join(ids, ",") != "a,b,c" {
		t.Fatal(ids)
	}
}

func TestPrefetchConcurrentPuts(t *testing.T) {
	_, h, _ := newFake(PrefetchConfig{})

	var (
		wg      sync.WaitGroup
		created atomic.Int32
	)

	for range 16 {
		wg.Go(func() {
			if call(t, h, "PUT", wire.PrefetchPath+"/same", spec(0, 0, keyA)).code == 201 {
				created.Add(1)
			}
		})
	}

	wg.Wait()

	if created.Load() != 1 {
		t.Fatal("created", strconv.Itoa(int(created.Load())))
	}
}
