// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package fakeracer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

// PrefetchOutcome is the scripted result of one ref. The zero value succeeds.
type PrefetchOutcome struct {
	// Pending keeps the ref unsettled; the hook is asked again on the next
	// status read.
	Pending bool
	// Error fails the ref with a design error code such as "not_found".
	Error string
	// ETag is the resolved version reported for refs submitted without one.
	ETag string
	// Size is the object length, used to resolve a zero Length. Zero leaves
	// the logical length at the requested Length.
	Size uint64
}

// PrefetchReject is a scripted whole-request failure.
type PrefetchReject struct {
	Status     int
	Code       string
	RetryAfter int
}

// PrefetchConfig scripts a [Prefetches]. Zero values use the server defaults.
type PrefetchConfig struct {
	// CacheUID is the live ClusterCache UID. Empty accepts any cache_uid.
	CacheUID string
	// Passthrough rejects every PUT with 409 passthrough.
	Passthrough bool
	// MaxBodyBytes, MaxRefs, MaxTickets, and RefsPage default to 4194304,
	// 16384, 1024, and 256.
	MaxBodyBytes, MaxRefs, MaxTickets, RefsPage int
	// MaxTTL, MaxWait, and Retention default to 3600s, 60s, and 300s.
	MaxTTL, MaxWait, Retention time.Duration
	// Now defaults to time.Now.
	Now func() time.Time
	// Admit may reject a PUT, for example with 429 ticket_limit.
	Admit func(id string, request wire.PrefetchRequest) *PrefetchReject
	// Outcome decides each ref. Nil succeeds every ref.
	Outcome func(ctx context.Context, ticket string, ref wire.PrefetchRef) PrefetchOutcome
}

// Prefetches is an in-memory prefetch ticket registry for one cache. It is
// not a compatibility oracle: there are no pages, owners, or scheduling.
type Prefetches struct {
	config  PrefetchConfig
	mu      sync.Mutex
	tickets map[string]*ticket
}

type refState struct {
	ref     wire.PrefetchRef
	outcome string
	err     string
	etag    *string
	length  uint64
}

type ticket struct {
	id                   string
	priority             string
	generation, sequence uint64
	hash                 [32]byte
	ttl                  time.Duration
	created, updated     time.Time
	expires              time.Time
	canceled             string
	refs                 []refState
}

// NewPrefetches returns an empty registry.
func NewPrefetches(config PrefetchConfig) *Prefetches {
	if config.MaxBodyBytes <= 0 {
		config.MaxBodyBytes = wire.PrefetchDefaultBody
	}

	if config.MaxRefs <= 0 {
		config.MaxRefs = wire.PrefetchDefaultRefs
	}

	if config.MaxTickets <= 0 {
		config.MaxTickets = 1024
	}

	if config.RefsPage <= 0 {
		config.RefsPage = wire.PrefetchRefsPage
	}

	if config.MaxTTL <= 0 {
		config.MaxTTL = time.Hour
	}

	if config.MaxWait <= 0 {
		config.MaxWait = time.Minute
	}

	if config.Retention <= 0 {
		config.Retention = 5 * time.Minute
	}

	if config.Now == nil {
		config.Now = time.Now
	}

	return &Prefetches{config: config, tickets: make(map[string]*ticket)}
}

type prefetchFailure struct {
	status     int
	code, msg  string
	retryAfter int
}

func fail(status int, code, format string, args ...any) *prefetchFailure {
	return &prefetchFailure{status: status, code: code, msg: fmt.Sprintf(format, args...)}
}

func invalidRequest(format string, args ...any) *prefetchFailure {
	return fail(http.StatusBadRequest, wire.PrefetchInvalidRequest, format, args...)
}

// ServeHTTP serves /v1/prefetches and /v1/prefetches/{id}.
func (p *Prefetches) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	status, body, failure := p.serve(r)
	if failure != nil {
		if failure.status == http.StatusMethodNotAllowed {
			allow := "GET, PUT, DELETE"
			if r.URL.Path == wire.PrefetchPath {
				allow = "GET"
			}

			w.Header().Set("Allow", allow)
		}

		if failure.retryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(failure.retryAfter))
		}

		status, body = failure.status, wire.PrefetchErrorBody{Code: failure.code, Message: failure.msg}
		if failure.code == "" {
			body = wire.PrefetchErrorBody{Code: wire.PrefetchInvalidRequest, Message: "method not allowed"}
		}
	}

	raw, err := json.Marshal(body)
	if err != nil {
		writeError(w, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.WriteHeader(status)

	if _, err := w.Write(raw); err != nil {
		return
	}
}

func (p *Prefetches) serve(r *http.Request) (int, any, *prefetchFailure) {
	if r.URL.Path == wire.PrefetchPath {
		if r.Method != http.MethodGet {
			return 0, nil, fail(http.StatusMethodNotAllowed, "", "")
		}

		if f := noBody(r); f != nil {
			return 0, nil, f
		}

		if r.URL.RawQuery != "" {
			return 0, nil, invalidRequest("list takes no parameters")
		}

		return http.StatusOK, p.list(), nil
	}

	id := strings.TrimPrefix(r.URL.Path, wire.PrefetchPath+"/")

	switch r.Method {
	case http.MethodGet, http.MethodPut, http.MethodDelete:
	default:
		return 0, nil, fail(http.StatusMethodNotAllowed, "", "")
	}

	if wire.ValidatePrefetchID(id) != nil {
		return 0, nil, invalidRequest("invalid id")
	}

	switch r.Method {
	case http.MethodPut:
		return p.put(r, id)
	case http.MethodDelete:
		if f := noBody(r); f != nil {
			return 0, nil, f
		}

		if r.URL.RawQuery != "" {
			return 0, nil, invalidRequest("cancel takes no parameters")
		}

		return p.cancel(r.Context(), id)
	default:
		if f := noBody(r); f != nil {
			return 0, nil, f
		}

		return p.status(r, id)
	}
}

func noBody(r *http.Request) *prefetchFailure {
	if r.ContentLength > 0 || len(r.TransferEncoding) != 0 {
		return invalidRequest("unexpected body")
	}

	if r.Header.Get("Authorization") != "" {
		return invalidRequest("authorization is accepted on PUT only")
	}

	return nil
}

func (p *Prefetches) put(r *http.Request, id string) (int, any, *prefetchFailure) {
	if len(r.TransferEncoding) != 0 || r.Header.Get("Expect") != "" || r.Header.Get("Content-Encoding") != "" || r.ContentLength <= 0 {
		return 0, nil, invalidRequest("unsupported body framing")
	}

	if r.URL.RawQuery != "" {
		return 0, nil, invalidRequest("PUT takes no parameters")
	}

	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
		return 0, nil, invalidRequest("content type must be application/json")
	}

	if r.ContentLength > int64(p.config.MaxBodyBytes) {
		return 0, nil, fail(http.StatusRequestEntityTooLarge, wire.PrefetchTooLarge, "body over %d bytes", p.config.MaxBodyBytes)
	}

	auth := r.Header.Get("Authorization")
	if auth != "" && wire.ValidateOpaque(auth) != nil {
		return 0, nil, invalidRequest("invalid authorization")
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, r.ContentLength+1))
	if err != nil || int64(len(raw)) != r.ContentLength {
		return 0, nil, invalidRequest("short body")
	}

	request, f := p.decode(raw)
	if f != nil {
		return 0, nil, f
	}

	if p.config.Passthrough {
		return 0, nil, fail(http.StatusConflict, wire.PrefetchPassthrough, "cache is passthrough")
	}

	if p.config.CacheUID != "" && request.CacheUID != "" && request.CacheUID != p.config.CacheUID {
		return 0, nil, fail(http.StatusConflict, wire.PrefetchCacheUIDMismatch, "cache uid mismatch")
	}

	if p.config.Admit != nil {
		if reject := p.config.Admit(id, request); reject != nil {
			return 0, nil, &prefetchFailure{status: reject.Status, code: reject.Code, msg: "rejected", retryAfter: reject.RetryAfter}
		}
	}

	// The hash covers the canonical body with defaults applied and the
	// presence, not the value, of Authorization.
	canonical, err := json.Marshal(request)
	if err != nil {
		return 0, nil, invalidRequest("encode")
	}

	hash := sha256.Sum256(append(canonical, boolByte(auth != "")))

	ctx := r.Context()

	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.config.Now()
	p.gc(now)

	t, ok := p.tickets[id]
	code := http.StatusOK

	switch {
	case !ok:
		if len(p.tickets) >= p.config.MaxTickets {
			return 0, nil, &prefetchFailure{status: http.StatusTooManyRequests, code: wire.PrefetchTicketLimit, msg: "ticket limit", retryAfter: 1}
		}

		t = &ticket{id: id, created: now}
		p.tickets[id] = t
		code = http.StatusCreated
	case request.Generation == t.generation && request.Sequence == t.sequence:
		if hash != t.hash {
			return 0, nil, fail(http.StatusConflict, wire.PrefetchPayloadMismatch, "payload differs at generation %d sequence %d", t.generation, t.sequence)
		}

		if t.canceled == "" {
			t.expires, t.updated = now.Add(t.ttl), now
		}

		return code, p.render(ctx, t, wire.PrefetchQuery{}), nil
	case request.Generation < t.generation:
		return 0, nil, fail(http.StatusConflict, wire.PrefetchStaleGeneration, "generation %d is older than current %d", request.Generation, t.generation)
	case request.Generation == t.generation && request.Sequence < t.sequence:
		return 0, nil, fail(http.StatusConflict, wire.PrefetchStaleSequence, "sequence %d is older than current %d", request.Sequence, t.sequence)
	}

	t.priority, t.generation, t.sequence, t.hash = request.Priority, request.Generation, request.Sequence, hash
	t.ttl = time.Duration(request.TTLSeconds) * time.Second
	t.updated, t.expires, t.canceled = now, now.Add(t.ttl), ""
	t.refs = make([]refState, len(request.Refs))

	for i, ref := range request.Refs {
		t.refs[i] = refState{ref: ref, outcome: "pending", length: ref.Length}
		if ref.ETag != "" {
			etag := ref.ETag
			t.refs[i].etag = &etag
		}
	}

	return code, p.render(ctx, t, wire.PrefetchQuery{}), nil
}

func boolByte(b bool) byte {
	if b {
		return 1
	}

	return 0
}

// decode parses strictly and applies defaults.
func (p *Prefetches) decode(raw []byte) (wire.PrefetchRequest, *prefetchFailure) {
	var request wire.PrefetchRequest

	var present struct {
		TTLSeconds *uint64 `json:"ttl_seconds"`
		Length     []struct {
			Length *uint64 `json:"length"`
		} `json:"refs"`
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&request); err != nil || decoder.More() {
		return request, invalidRequest("invalid JSON: %v", err)
	}

	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return request, invalidRequest("trailing data")
	}

	if err := json.Unmarshal(raw, &present); err != nil {
		return request, invalidRequest("invalid JSON")
	}

	if request.Priority == "" {
		request.Priority = "normal"
	}

	switch {
	case !wire.ValidPriority(request.Priority):
		return request, invalidRequest("invalid priority")
	case request.Generation > wire.PrefetchMaxCounter || request.Sequence > wire.PrefetchMaxCounter:
		return request, invalidRequest("counter out of range")
	case present.TTLSeconds != nil && *present.TTLSeconds == 0:
		return request, invalidRequest("ttl_seconds must be positive")
	case len(request.Refs) == 0:
		return request, invalidRequest("refs must not be empty")
	case len(request.Refs) > p.config.MaxRefs:
		return request, fail(http.StatusRequestEntityTooLarge, wire.PrefetchTooLarge, "more than %d refs", p.config.MaxRefs)
	}

	if request.TTLSeconds == 0 {
		request.TTLSeconds = wire.PrefetchDefaultTTLSecs
	}

	request.TTLSeconds = min(request.TTLSeconds, uint64(p.config.MaxTTL/time.Second))

	for i, ref := range request.Refs {
		if _, err := wire.ParseKey(ref.Key); err != nil {
			return request, invalidRequest("ref %d: invalid key", i)
		}

		if ref.ETag != "" && wire.ValidateETag(ref.ETag) != nil {
			return request, invalidRequest("ref %d: invalid etag", i)
		}

		if ref.Metadata != "" && wire.ValidateOpaque(ref.Metadata) != nil {
			return request, invalidRequest("ref %d: invalid metadata", i)
		}

		if present.Length[i].Length != nil && ref.Length == 0 {
			return request, invalidRequest("ref %d: length must be positive", i)
		}
	}

	return request, nil
}

// gc expires live tickets and drops terminal tickets after retention. Callers
// hold p.mu.
func (p *Prefetches) gc(now time.Time) {
	for id, t := range p.tickets {
		if t.canceled == "" && !now.Before(t.expires) {
			t.canceled, t.updated = "expired", t.expires
		}

		if t.canceled != "" && !now.Before(t.updated.Add(p.config.Retention)) {
			delete(p.tickets, id)
		}
	}
}

// evaluate asks the hook about pending refs. Callers hold p.mu.
func (p *Prefetches) evaluate(ctx context.Context, t *ticket) {
	if t.canceled != "" {
		return
	}

	for i := range t.refs {
		ref := &t.refs[i]
		if ref.outcome != "pending" {
			continue
		}

		var outcome PrefetchOutcome
		if p.config.Outcome != nil {
			outcome = p.config.Outcome(ctx, t.id, ref.ref)
		}

		if outcome.Pending {
			continue
		}

		if ref.etag == nil && outcome.ETag != "" {
			etag := outcome.ETag
			ref.etag = &etag
		}

		if ref.length == 0 && outcome.Size > ref.ref.Offset {
			ref.length = outcome.Size - ref.ref.Offset
		}

		if outcome.Size != 0 && ref.ref.Offset >= outcome.Size {
			outcome.Error = "invalid_range"
		}

		if outcome.Size != 0 && ref.length > outcome.Size-min(ref.ref.Offset, outcome.Size) {
			ref.length = outcome.Size - min(ref.ref.Offset, outcome.Size)
		}

		ref.outcome, ref.err = "succeeded", outcome.Error
		if outcome.Error != "" {
			ref.outcome = "failed"
		}
	}
}

func (t *ticket) settled() bool {
	if t.canceled != "" {
		return true
	}

	for _, ref := range t.refs {
		if ref.outcome == "pending" {
			return false
		}
	}

	return true
}

type cursor struct {
	Generation uint64 `json:"g"`
	Sequence   uint64 `json:"s"`
	Next       int    `json:"n"`
}

// render builds a status. Callers hold p.mu and have run gc and evaluate.
func (p *Prefetches) render(ctx context.Context, t *ticket, q wire.PrefetchQuery) wire.PrefetchStatus {
	p.evaluate(ctx, t)

	s := wire.PrefetchStatus{
		ID:          t.id,
		CacheUID:    p.config.CacheUID,
		Priority:    t.priority,
		Generation:  t.generation,
		Sequence:    t.sequence,
		Settled:     t.settled(),
		TTLSeconds:  uint64(t.ttl / time.Second),
		ExpiresAtMs: t.expires.UnixMilli(),
		CreatedAtMs: t.created.UnixMilli(),
		UpdatedAtMs: t.updated.UnixMilli(),
		RefsTotal:   uint64(len(t.refs)),
	}

	pages := map[string]struct{}{}

	for _, ref := range t.refs {
		s.Bytes.LogicalRequested += ref.length

		aligned := uint64(0)

		if ref.length != 0 {
			for page := ref.ref.Offset / wire.PageSize; page <= (ref.ref.Offset+ref.length-1)/wire.PageSize; page++ {
				name := ref.ref.Key + "/" + strconv.FormatUint(page, 10)
				if _, ok := pages[name]; !ok {
					pages[name] = struct{}{}
					aligned += wire.PageSize
				}
			}
		}

		s.Bytes.AlignedRequested += aligned

		switch ref.outcome {
		case "succeeded":
			s.RefsSucceeded++
			s.Bytes.Admitted += aligned
			s.Bytes.FetchedFromOrigin += aligned
		case "failed":
			s.RefsFailed++
			s.Bytes.Failed += aligned
		default:
			s.RefsPending++

			if t.canceled == "" {
				s.Bytes.InFlight += aligned
			}
		}
	}

	switch {
	case t.canceled != "":
		s.State = "CANCELLED"
		reason := t.canceled
		s.Reason = &reason
	case !s.Settled:
		s.State = "RUNNING"
	case s.RefsFailed == 0:
		s.State = "SUCCEEDED"
	case s.RefsSucceeded == 0:
		s.State = "FAILED"
	default:
		s.State = "PARTIAL"
	}

	if q.Coverage {
		s.Coverage = &wire.PrefetchCoverage{ResidentBytes: s.Bytes.Admitted, ObservedAtMs: p.config.Now().UnixMilli()}
	}

	if q.Refs != "" {
		start := 0

		if q.Cursor != "" {
			raw, _ := base64.RawURLEncoding.DecodeString(q.Cursor) //nolint:errcheck // Validated by status.

			var c cursor

			_ = json.Unmarshal(raw, &c) //nolint:errcheck // Validated by status.
			start = c.Next
		}

		s.Refs = []wire.PrefetchRefStatus{}

		for i := start; i < len(t.refs); i++ {
			ref := t.refs[i]
			if q.Refs == "failed" && ref.outcome != "failed" {
				continue
			}

			if len(s.Refs) == p.config.RefsPage {
				next := encodeCursor(cursor{Generation: t.generation, Sequence: t.sequence, Next: i})
				s.NextCursor = &next

				break
			}

			status := wire.PrefetchRefStatus{Index: uint64(i), Key: ref.ref.Key, ETag: ref.etag, Offset: ref.ref.Offset, Length: ref.length, Outcome: ref.outcome}
			if ref.err != "" {
				e := ref.err
				status.Error = &e
			}

			s.Refs = append(s.Refs, status)
		}
	}

	return s
}

func encodeCursor(c cursor) string {
	raw, err := json.Marshal(c)
	if err != nil {
		return ""
	}

	return base64.RawURLEncoding.EncodeToString(raw)
}

func (p *Prefetches) status(r *http.Request, id string) (int, any, *prefetchFailure) {
	q, err := wire.ParsePrefetchQuery(r.URL.RawQuery, p.config.MaxWait)
	if err != nil {
		return 0, nil, invalidRequest("invalid query")
	}

	var c cursor

	if q.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(q.Cursor)
		if q.Refs == "" || err != nil || json.Unmarshal(raw, &c) != nil || c.Next < 0 {
			return 0, nil, invalidRequest("invalid cursor")
		}
	}

	deadline := time.Now().Add(q.Wait)

	for {
		p.mu.Lock()
		p.gc(p.config.Now())

		t, ok := p.tickets[id]
		if !ok {
			p.mu.Unlock()
			return 0, nil, fail(http.StatusNotFound, wire.PrefetchNotFound, "no prefetch %q", id)
		}

		if q.Cursor != "" && (c.Generation != t.generation || c.Sequence != t.sequence) {
			p.mu.Unlock()
			return 0, nil, invalidRequest("cursor from another window")
		}

		p.evaluate(r.Context(), t)

		if t.settled() || !time.Now().Before(deadline) {
			s := p.render(r.Context(), t, q)
			p.mu.Unlock()

			return http.StatusOK, s, nil
		}

		p.mu.Unlock()

		select {
		case <-r.Context().Done():
			return 0, nil, fail(http.StatusServiceUnavailable, wire.PrefetchUnavailable, "client went away")
		case <-time.After(min(5*time.Millisecond, time.Until(deadline))):
		}
	}
}

func (p *Prefetches) cancel(ctx context.Context, id string) (int, any, *prefetchFailure) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.config.Now()
	p.gc(now)

	t, ok := p.tickets[id]
	if !ok {
		return 0, nil, fail(http.StatusNotFound, wire.PrefetchNotFound, "no prefetch %q", id)
	}

	if t.canceled == "" {
		p.evaluate(ctx, t)
		t.canceled, t.updated = "client", now
	}

	return http.StatusOK, p.render(ctx, t, wire.PrefetchQuery{}), nil
}

func (p *Prefetches) list() wire.PrefetchList {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.gc(p.config.Now())

	ids := make([]string, 0, len(p.tickets))
	for id := range p.tickets {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	list := wire.PrefetchList{Prefetches: []wire.PrefetchStatus{}}

	for _, id := range ids {
		s := p.render(context.Background(), p.tickets[id], wire.PrefetchQuery{})
		list.Prefetches = append(list.Prefetches, wire.PrefetchStatus{
			ID: s.ID, State: s.State, Reason: s.Reason, Priority: s.Priority, Generation: s.Generation,
			Sequence: s.Sequence, Settled: s.Settled, ExpiresAtMs: s.ExpiresAtMs, Bytes: s.Bytes,
		})
	}

	return list
}

// Ticket reports whether id exists, for tests.
func (p *Prefetches) Ticket(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	_, ok := p.tickets[id]

	return ok
}
