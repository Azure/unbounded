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

	// Like Racer, the hash covers the body as sent: whitespace and field
	// order do not matter, but an explicit default differs from an omitted
	// field.
	if r := call(t, h, "PUT", path, ` { "refs" : [ {"key":"`+keyA+`"} ], "sequence":0, "generation":2 } `); r.code != 200 {
		t.Fatalf("reordered: %+v", r)
	}

	if r := call(t, h, "PUT", path, `{"priority":"normal","generation":2,"sequence":0,"ttl_seconds":600,"refs":[{"key":"`+keyA+`","offset":0}]}`); r.code != 409 || r.err.Code != wire.PrefetchPayloadMismatch {
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

	// A settled ticket keeps its outcome at expiry.
	r = call(t, h, "GET", path, "")
	if r.status.State != "SUCCEEDED" || r.status.Reason != nil || !r.status.Settled {
		t.Fatalf("expired settled: %+v", r.status)
	}

	// The replay no longer renews a released ticket.
	if r2 := call(t, h, "PUT", path, `{"ttl_seconds":3600,"refs":[{"key":"`+keyA+`"}]}`); r2.code != 200 || r2.status.ExpiresAtMs != r.status.ExpiresAtMs {
		t.Fatalf("replay after expiry: %+v", r2)
	}

	// Retention counts from expiry.
	clock.Add(time.Minute - time.Second)

	if r := call(t, h, "GET", path, ""); r.code != 200 {
		t.Fatalf("retained: %+v", r)
	}

	clock.Add(time.Second)

	if r := call(t, h, "GET", path, ""); r.code != 404 {
		t.Fatalf("retention: %+v", r)
	}
}

func TestPrefetchTTLExpiryCancelsUnsettled(t *testing.T) {
	_, h, clock := newFake(PrefetchConfig{Retention: time.Minute, Outcome: func(context.Context, string, wire.PrefetchRef) PrefetchOutcome {
		return PrefetchOutcome{Pending: true}
	}})
	path := wire.PrefetchPath + "/u"

	call(t, h, "PUT", path, `{"ttl_seconds":5,"refs":[{"key":"`+keyA+`"}]}`)
	clock.Add(5 * time.Second)

	r := call(t, h, "GET", path, "")
	if r.status.State != "CANCELLED" || r.status.Reason == nil || *r.status.Reason != "expired" || !r.status.Settled || r.status.Bytes.InFlight != 0 {
		t.Fatalf("expired: %+v", r.status)
	}

	// DELETE keeps the original reason.
	if r := call(t, h, "DELETE", path, ""); r.code != 200 || *r.status.Reason != "expired" {
		t.Fatalf("delete: %+v", r)
	}
}

func TestPrefetchTicketLimitEvictsRetained(t *testing.T) {
	_, h, clock := newFake(PrefetchConfig{MaxTickets: 2})

	call(t, h, "PUT", wire.PrefetchPath+"/a", spec(0, 0, keyA))
	call(t, h, "PUT", wire.PrefetchPath+"/b", spec(0, 0, keyA))
	call(t, h, "DELETE", wire.PrefetchPath+"/b", "")
	clock.Add(time.Second)
	call(t, h, "DELETE", wire.PrefetchPath+"/a", "")

	// The oldest retained ticket (b) makes room.
	if r := call(t, h, "PUT", wire.PrefetchPath+"/c", spec(0, 0, keyA)); r.code != 201 {
		t.Fatalf("evict: %+v", r)
	}

	if r := call(t, h, "GET", wire.PrefetchPath+"/b", ""); r.code != 404 {
		t.Fatalf("b: %+v", r)
	}

	if r := call(t, h, "PUT", wire.PrefetchPath+"/d", spec(0, 0, keyA)); r.code != 201 {
		t.Fatalf("evict a: %+v", r)
	}

	if r := call(t, h, "PUT", wire.PrefetchPath+"/e", spec(0, 0, keyA)); r.code != 429 || r.header.Get("Retry-After") != "5" {
		t.Fatalf("limit: %+v", r)
	}
}

func TestPrefetchEmptyObjectAndWireShapes(t *testing.T) {
	_, h, _ := newFake(PrefetchConfig{Outcome: func(context.Context, string, wire.PrefetchRef) PrefetchOutcome {
		return PrefetchOutcome{Empty: true, ETag: `"e"`}
	}})
	path := wire.PrefetchPath + "/e"

	r := call(t, h, "PUT", path, `{"refs":[{"key":"`+keyA+`"},{"key":"`+keyA+`","offset":1},{"key":"`+keyA+`","length":1}]}`)
	if r.status.State != "PARTIAL" || r.status.RefsSucceeded != 1 || r.status.RefsFailed != 2 || r.status.Bytes.AlignedRequested != 0 {
		t.Fatalf("empty: %+v", r.status)
	}

	raw := func(target string) map[string]json.RawMessage {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://racer"+target, nil))

		var m map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}

		return m
	}

	m := raw(path)
	for _, field := range []string{"cache_uid", "reason", "ttl_seconds", "created_at_ms", "updated_at_ms", "refs_total", "refs_succeeded", "refs_failed", "refs_pending", "bytes"} {
		if _, ok := m[field]; !ok {
			t.Errorf("missing %s", field)
		}
	}

	for _, field := range []string{"refs", "next_cursor", "coverage"} {
		if _, ok := m[field]; ok {
			t.Errorf("unrequested %s", field)
		}
	}

	m = raw(path + "?refs=all")
	if string(m["next_cursor"]) != "null" || len(m["refs"]) == 0 {
		t.Fatalf("refs: %s %s", m["refs"], m["next_cursor"])
	}

	if r := call(t, h, "POST", path, ""); r.err.Code != wire.PrefetchMethodNotAllowed {
		t.Fatalf("405: %+v", r)
	}

	for _, body := range []string{`{"cache_uid":"","refs":[{"key":"` + keyA + `"}]}`, `{"cache_uid":"` + strings.Repeat("u", 65) + `","refs":[{"key":"` + keyA + `"}]}`} {
		if r := call(t, h, "PUT", path, body); r.code != 400 {
			t.Fatalf("cache uid: %+v", r)
		}
	}
}

func TestPrefetchLimitsAndRejections(t *testing.T) {
	_, h, _ := newFake(PrefetchConfig{MaxTickets: 1, MaxRefs: 2, MaxBodyBytes: 4096})

	call(t, h, "PUT", wire.PrefetchPath+"/a", spec(0, 0, keyA))

	r := call(t, h, "PUT", wire.PrefetchPath+"/b", spec(0, 0, keyA))
	if r.code != 429 || r.err.Code != wire.PrefetchTicketLimit || r.header.Get("Retry-After") != "5" {
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

	generation, _, _ := strings.Cut(cursor, ".")

	if r := call(t, h, "GET", path+"?refs=all&cursor="+generation+".1.0", ""); r.code != 200 {
		t.Fatalf("current cursor: %+v", r)
	}

	for _, offset := range []string{"2147483648", "18446744073709551616"} {
		if r := call(t, h, "GET", path+"?refs=all&cursor="+generation+".1."+offset, ""); r.code != 400 {
			t.Fatalf("cursor offset %s: %+v", offset, r)
		}
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
