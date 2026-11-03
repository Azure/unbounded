// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package fakeracer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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
	// the logical length at the requested Length unless Empty is set.
	Size uint64
	// Empty reports a zero-length object. Like Racer, a ref with offset 0
	// and no length then succeeds with no pages; any other ref fails
	// invalid_range.
	Empty bool
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

// Prefetches is an in-memory prefetch ticket registry for one cache. It
// follows the Racer server's client-socket contract (validation, error codes,
// idempotency, expiry, retention, and JSON shapes) but has no pages, owners,
// or scheduling: refs move from pending to an outcome as the Outcome hook
// decides, an unsettled ticket reports RUNNING (never QUEUED), and the only
// byte counters it fills are the requested, admitted, fetched, in-flight, and
// failed bytes.
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
	// released is set once the ticket is canceled or expires: its outcomes
	// are frozen and it is removed at removeAt.
	released bool
	removeAt time.Time
	refs     []refState
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
	}

	raw, err := json.Marshal(body)
	if err != nil {
		writeError(w, http.StatusInternalServerError)
		return
	}

	// Racer closes the connection after every prefetch response.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.Header().Set("Connection", "close")
	w.WriteHeader(status)

	if _, err := w.Write(raw); err != nil {
		return
	}
}

func (p *Prefetches) serve(r *http.Request) (int, any, *prefetchFailure) {
	if r.URL.Path == wire.PrefetchPath {
		if r.Method != http.MethodGet {
			return 0, nil, fail(http.StatusMethodNotAllowed, wire.PrefetchMethodNotAllowed, "method not allowed")
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
		return 0, nil, fail(http.StatusMethodNotAllowed, wire.PrefetchMethodNotAllowed, "method not allowed")
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

	// Racer rejects a passthrough cache before it parses the body.
	if p.config.Passthrough {
		return 0, nil, fail(http.StatusConflict, wire.PrefetchPassthrough, "cache is passthrough")
	}

	request, canonical, f := p.decode(raw)
	if f != nil {
		return 0, nil, f
	}

	if p.config.CacheUID != "" && request.CacheUID != "" && request.CacheUID != p.config.CacheUID {
		return 0, nil, fail(http.StatusConflict, wire.PrefetchCacheUIDMismatch, "cache uid mismatch")
	}

	// Like Racer, the hash covers the body as sent (re-encoded without
	// whitespace, absent fields omitted, defaults not applied) and the
	// presence, not the value, of Authorization.
	hash := sha256.Sum256(append(canonical, boolByte(auth != "")))

	ctx := r.Context()

	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.config.Now()
	p.gc(ctx, now)

	t, ok := p.tickets[id]
	if ok {
		switch {
		case request.Generation < t.generation:
			return 0, nil, fail(http.StatusConflict, wire.PrefetchStaleGeneration, "generation %d is older than current %d", request.Generation, t.generation)
		case request.Generation == t.generation && request.Sequence < t.sequence:
			return 0, nil, fail(http.StatusConflict, wire.PrefetchStaleSequence, "sequence %d is older than current %d", request.Sequence, t.sequence)
		case request.Generation == t.generation && request.Sequence == t.sequence:
			if hash != t.hash {
				return 0, nil, fail(http.StatusConflict, wire.PrefetchPayloadMismatch, "generation %d sequence %d was submitted with another payload", t.generation, t.sequence)
			}

			// A replay renews the TTL of a live ticket only.
			if !t.released {
				t.expires = now.Add(t.ttl)
			}

			return http.StatusOK, p.render(ctx, t, wire.PrefetchQuery{}), nil
		}
	}

	if p.config.Admit != nil {
		if reject := p.config.Admit(id, request); reject != nil {
			return 0, nil, &prefetchFailure{status: reject.Status, code: reject.Code, msg: "rejected", retryAfter: reject.RetryAfter}
		}
	}

	code := http.StatusOK

	if !ok {
		if len(p.tickets) >= p.config.MaxTickets && !p.evictOldestReleased() {
			return 0, nil, &prefetchFailure{status: http.StatusTooManyRequests, code: wire.PrefetchTicketLimit, msg: "node prefetch ticket limit reached", retryAfter: 5}
		}

		t = &ticket{id: id, created: now}
		p.tickets[id] = t
		code = http.StatusCreated
	}

	t.priority, t.generation, t.sequence, t.hash = request.Priority, request.Generation, request.Sequence, hash
	t.ttl = time.Duration(request.TTLSeconds) * time.Second
	t.updated, t.expires, t.canceled = now, now.Add(t.ttl), ""
	t.released, t.removeAt = false, time.Time{}
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

// evictOldestReleased drops the released ticket that is due for removal
// first, as Racer does before answering 429 ticket_limit. Callers hold p.mu.
func (p *Prefetches) evictOldestReleased() bool {
	var oldest *ticket

	for _, t := range p.tickets {
		if t.released && (oldest == nil || t.removeAt.Before(oldest.removeAt)) {
			oldest = t
		}
	}

	if oldest == nil {
		return false
	}

	delete(p.tickets, oldest.id)

	return true
}

func boolByte(b bool) byte {
	if b {
		return 1
	}

	return 0
}

// rawRequest mirrors Racer's raw PUT body: absent fields stay absent so the
// canonical encoding distinguishes an explicit default from an omitted field.
type rawRequest struct {
	CacheUID        *string  `json:"cache_uid,omitempty"`
	Priority        *string  `json:"priority,omitempty"`
	Generation      *uint64  `json:"generation,omitempty"`
	Sequence        *uint64  `json:"sequence,omitempty"`
	TTLSeconds      *uint64  `json:"ttl_seconds,omitempty"`
	MaxPendingBytes *uint64  `json:"max_pending_bytes,omitempty"`
	Refs            []rawRef `json:"refs"`
}

type rawRef struct {
	Key      string  `json:"key"`
	ETag     *string `json:"etag,omitempty"`
	Offset   *uint64 `json:"offset,omitempty"`
	Length   *uint64 `json:"length,omitempty"`
	Metadata *string `json:"metadata,omitempty"`
}

func value[T any](v *T) T {
	var zero T
	if v == nil {
		return zero
	}

	return *v
}

// decode parses strictly, applies defaults and caps, and returns the
// canonical encoding of the body as sent.
func (p *Prefetches) decode(raw []byte) (wire.PrefetchRequest, []byte, *prefetchFailure) {
	var (
		body    rawRequest
		request wire.PrefetchRequest
	)

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&body); err != nil {
		return request, nil, invalidRequest("invalid JSON body: %v", err)
	}

	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return request, nil, invalidRequest("invalid JSON body: trailing data")
	}

	if body.Refs == nil {
		return request, nil, invalidRequest("invalid JSON body: missing refs")
	}

	request = wire.PrefetchRequest{
		CacheUID:        value(body.CacheUID),
		Priority:        value(body.Priority),
		Generation:      value(body.Generation),
		Sequence:        value(body.Sequence),
		TTLSeconds:      wire.PrefetchDefaultTTLSecs,
		MaxPendingBytes: value(body.MaxPendingBytes),
	}

	if body.Priority == nil {
		request.Priority = "normal"
	}

	switch {
	case len(body.Refs) == 0:
		return request, nil, invalidRequest("refs must not be empty")
	case len(body.Refs) > p.config.MaxRefs:
		return request, nil, fail(http.StatusRequestEntityTooLarge, wire.PrefetchTooLarge, "%d refs exceed %d", len(body.Refs), p.config.MaxRefs)
	case !wire.ValidPriority(request.Priority):
		return request, nil, invalidRequest("priority must be high, normal, or low")
	case request.Generation > wire.PrefetchMaxCounter || request.Sequence > wire.PrefetchMaxCounter:
		return request, nil, invalidRequest("counter exceeds 2^53-1")
	case body.TTLSeconds != nil && *body.TTLSeconds == 0:
		return request, nil, invalidRequest("ttl_seconds must be at least 1")
	case body.CacheUID != nil && (*body.CacheUID == "" || len(*body.CacheUID) > 64):
		return request, nil, invalidRequest("invalid cache_uid")
	}

	if body.TTLSeconds != nil {
		request.TTLSeconds = *body.TTLSeconds
	}

	request.TTLSeconds = min(request.TTLSeconds, uint64(p.config.MaxTTL/time.Second))

	request.Refs = make([]wire.PrefetchRef, len(body.Refs))

	for i, ref := range body.Refs {
		if _, err := wire.ParseKey(ref.Key); err != nil {
			return request, nil, invalidRequest("refs[%d].key is not 64 hex", i)
		}

		if ref.ETag != nil && wire.ValidateETag(*ref.ETag) != nil {
			return request, nil, invalidRequest("refs[%d].etag is not strong", i)
		}

		if ref.Length != nil && *ref.Length == 0 {
			return request, nil, invalidRequest("refs[%d].length must be at least 1", i)
		}

		if ref.Metadata != nil && wire.ValidateOpaque(*ref.Metadata) != nil {
			return request, nil, invalidRequest("refs[%d].metadata is invalid", i)
		}

		request.Refs[i] = wire.PrefetchRef{Key: ref.Key, ETag: value(ref.ETag), Offset: value(ref.Offset), Length: value(ref.Length), Metadata: value(ref.Metadata)}
	}

	canonical, err := json.Marshal(body)
	if err != nil {
		return request, nil, invalidRequest("body cannot be canonicalized")
	}

	return request, canonical, nil
}

// gc applies TTL expiry and retention like Racer: an unsettled ticket
// becomes CANCELLED (expired), a settled one keeps its outcome, and either is
// removed once retention passes. Callers hold p.mu.
func (p *Prefetches) gc(ctx context.Context, now time.Time) {
	for id, t := range p.tickets {
		if !t.released && !now.Before(t.expires) {
			p.evaluate(ctx, t)

			if !t.settled() {
				t.canceled, t.updated = "expired", t.expires
			}

			p.release(t, t.expires)
		}

		if t.released && !now.Before(t.removeAt) {
			delete(p.tickets, id)
		}
	}
}

// release freezes a ticket's outcomes and starts its retention.
func (p *Prefetches) release(t *ticket, at time.Time) {
	if t.released {
		return
	}

	t.released, t.removeAt = true, at.Add(p.config.Retention)
}

// evaluate asks the hook about pending refs. Callers hold p.mu.
func (p *Prefetches) evaluate(ctx context.Context, t *ticket) {
	if t.released {
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

		if outcome.Empty {
			ref.length = 0
			if ref.ref.Offset != 0 || ref.ref.Length != 0 {
				outcome.Error = "invalid_range"
			}
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
	if t.released {
		return true
	}

	for _, ref := range t.refs {
		if ref.outcome == "pending" {
			return false
		}
	}

	return true
}

// statusJSON is the status body exactly as Racer sends it: every field is
// present, reason is null unless canceled, and refs and next_cursor (null on
// the last page) appear only when refs were requested.
type statusJSON struct {
	ID            string                    `json:"id"`
	CacheUID      string                    `json:"cache_uid"`
	State         string                    `json:"state"`
	Reason        *string                   `json:"reason"`
	Priority      string                    `json:"priority"`
	Generation    uint64                    `json:"generation"`
	Sequence      uint64                    `json:"sequence"`
	Settled       bool                      `json:"settled"`
	TTLSeconds    uint64                    `json:"ttl_seconds"`
	ExpiresAtMs   int64                     `json:"expires_at_ms"`
	CreatedAtMs   int64                     `json:"created_at_ms"`
	UpdatedAtMs   int64                     `json:"updated_at_ms"`
	RefsTotal     uint64                    `json:"refs_total"`
	RefsSucceeded uint64                    `json:"refs_succeeded"`
	RefsFailed    uint64                    `json:"refs_failed"`
	RefsPending   uint64                    `json:"refs_pending"`
	Bytes         wire.PrefetchBytes        `json:"bytes"`
	Coverage      *wire.PrefetchCoverage    `json:"coverage,omitempty"`
	Refs          *[]wire.PrefetchRefStatus `json:"refs,omitempty"`
	NextCursor    **string                  `json:"next_cursor,omitempty"`
}

// summaryJSON is one entry of the list body.
type summaryJSON struct {
	ID          string             `json:"id"`
	State       string             `json:"state"`
	Reason      *string            `json:"reason"`
	Priority    string             `json:"priority"`
	Generation  uint64             `json:"generation"`
	Sequence    uint64             `json:"sequence"`
	Settled     bool               `json:"settled"`
	ExpiresAtMs int64              `json:"expires_at_ms"`
	Bytes       wire.PrefetchBytes `json:"bytes"`
}

type listJSON struct {
	Prefetches []summaryJSON `json:"prefetches"`
}

// render builds a status. Callers hold p.mu and have run gc. The cursor, if
// any, has been validated by decodeCursor.
func (p *Prefetches) render(ctx context.Context, t *ticket, q wire.PrefetchQuery) statusJSON {
	p.evaluate(ctx, t)

	s := statusJSON{
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

			if !t.released {
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
	case s.RefsFailed == s.RefsTotal:
		s.State = "FAILED"
	default:
		s.State = "PARTIAL"
	}

	if q.Coverage {
		s.Coverage = &wire.PrefetchCoverage{ResidentBytes: s.Bytes.Admitted, ObservedAtMs: p.config.Now().UnixMilli()}
	}

	if q.Refs != "" {
		// Like Racer, the cursor is an offset into the filtered refs.
		start, _ := decodeCursor(q.Cursor, t) //nolint:errcheck // Validated by status.

		refs := []wire.PrefetchRefStatus{}

		var next *string

		matched := 0

		for i, ref := range t.refs {
			if q.Refs == "failed" && ref.outcome != "failed" {
				continue
			}

			matched++
			if matched <= start {
				continue
			}

			if len(refs) == p.config.RefsPage {
				cursor := encodeCursor(t, start+len(refs))
				next = &cursor

				break
			}

			status := wire.PrefetchRefStatus{Index: uint64(i), Key: ref.ref.Key, ETag: ref.etag, Offset: ref.ref.Offset, Length: ref.length, Outcome: ref.outcome}
			if ref.err != "" {
				e := ref.err
				status.Error = &e
			}

			refs = append(refs, status)
		}

		s.Refs, s.NextCursor = &refs, &next
	}

	return s
}

// encodeCursor uses Racer's "<generation>.<sequence>.<offset>" format.
func encodeCursor(t *ticket, offset int) string {
	return fmt.Sprintf("%d.%d.%d", t.generation, t.sequence, offset)
}

// decodeCursor returns the offset of a cursor for t's current window. An
// empty cursor is offset 0.
func decodeCursor(cursor string, t *ticket) (int, *prefetchFailure) {
	if cursor == "" {
		return 0, nil
	}

	parts := strings.Split(cursor, ".")
	if len(parts) < 3 {
		return 0, invalidRequest("invalid cursor")
	}

	var numbers [3]uint64

	for i := range numbers {
		n, err := strconv.ParseUint(parts[i], 10, 64)
		if err != nil {
			return 0, invalidRequest("invalid cursor")
		}

		numbers[i] = n
	}

	if numbers[0] != t.generation || numbers[1] != t.sequence {
		return 0, invalidRequest("cursor belongs to another generation or sequence")
	}

	if numbers[2] > math.MaxInt32 {
		return 0, invalidRequest("invalid cursor")
	}

	return int(numbers[2]), nil
}

func (p *Prefetches) status(r *http.Request, id string) (int, any, *prefetchFailure) {
	q, err := wire.ParsePrefetchQuery(r.URL.RawQuery, p.config.MaxWait)
	if err != nil {
		return 0, nil, invalidRequest("invalid query")
	}

	ctx := r.Context()
	deadline := time.Now().Add(q.Wait)

	for first := true; ; first = false {
		p.mu.Lock()
		p.gc(ctx, p.config.Now())

		t, ok := p.tickets[id]
		if !ok {
			p.mu.Unlock()
			return 0, nil, fail(http.StatusNotFound, wire.PrefetchNotFound, "prefetch %s not found", id)
		}

		// Like Racer, validate the cursor before waiting and again after.
		if _, f := decodeCursor(q.Cursor, t); f != nil {
			p.mu.Unlock()
			return 0, nil, f
		}

		p.evaluate(ctx, t)

		if t.settled() || (!first && !time.Now().Before(deadline)) || q.Wait == 0 {
			s := p.render(ctx, t, q)
			p.mu.Unlock()

			return http.StatusOK, s, nil
		}

		p.mu.Unlock()

		select {
		case <-ctx.Done():
			return 0, nil, fail(http.StatusServiceUnavailable, wire.PrefetchUnavailable, "client went away")
		case <-time.After(max(0, min(5*time.Millisecond, time.Until(deadline)))):
		}
	}
}

func (p *Prefetches) cancel(ctx context.Context, id string) (int, any, *prefetchFailure) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.config.Now()
	p.gc(ctx, now)

	t, ok := p.tickets[id]
	if !ok {
		return 0, nil, fail(http.StatusNotFound, wire.PrefetchNotFound, "prefetch %s not found", id)
	}

	// An already canceled ticket keeps its reason; a settled ticket that
	// expired becomes CANCELLED (client) but keeps its retention.
	if t.canceled == "" {
		p.evaluate(ctx, t)
		t.canceled, t.updated = "client", now
		p.release(t, now)
	}

	return http.StatusOK, p.render(ctx, t, wire.PrefetchQuery{}), nil
}

func (p *Prefetches) list() listJSON {
	p.mu.Lock()
	defer p.mu.Unlock()

	ctx := context.Background()
	p.gc(ctx, p.config.Now())

	ids := make([]string, 0, len(p.tickets))
	for id := range p.tickets {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	if len(ids) > 1024 {
		ids = ids[:1024]
	}

	list := listJSON{Prefetches: []summaryJSON{}}

	for _, id := range ids {
		s := p.render(ctx, p.tickets[id], wire.PrefetchQuery{})
		list.Prefetches = append(list.Prefetches, summaryJSON{
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
