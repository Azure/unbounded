// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"sync"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

// maxPrefetchResponse bounds a prefetch response body. A list holds at most
// 1024 summaries and a status at most one page of refs.
const maxPrefetchResponse = 16 << 20

// PrefetchRef is one object range to prefetch.
type PrefetchRef struct {
	// Key is the object key as 64 lowercase hexadecimal characters.
	Key string
	// ETag optionally pins a version. Empty resolves the current version once
	// at submit.
	ETag string
	// Offset is the first byte. It must be less than the object length.
	Offset int64
	// Length is the number of bytes. Zero means to the end of the object.
	Length int64
	// Metadata is passed to the origin like [Request.Metadata].
	Metadata string
}

// PrefetchSpec describes a prefetch ticket.
type PrefetchSpec struct {
	// CacheUID, if set, must equal the live ClusterCache UID.
	CacheUID string
	// Priority is "high", "normal", or "low". Empty means "normal".
	Priority string
	// Generation and Sequence order updates of one ticket; see
	// [PrefetchWindow]. Each is at most 2^53-1.
	Generation uint64
	Sequence   uint64
	// TTL is how long the ticket lives without an update. It is rounded up to
	// whole seconds. Zero means the server default (600s); Racer caps it.
	TTL time.Duration
	// MaxPendingBytes bounds queued and in-flight bytes for the ticket. Zero
	// means the server default.
	MaxPendingBytes int64
	// Authorization is passed to the origin for every ref, like
	// [Request.Authorization]. Racer keeps it in memory only.
	Authorization string
	// Refs lists the ranges to prefetch. It must not be empty.
	Refs []PrefetchRef
}

// StatusOptions selects what [Client.PrefetchStatus] returns.
type StatusOptions struct {
	// Wait blocks until the ticket settles or Wait elapses, whichever is
	// first. Racer caps it (default 60s) and rejects larger values.
	Wait time.Duration
	// Coverage probes owners for resident bytes of the desired set.
	Coverage bool
	// Refs is "", "failed", or "all" and selects per-ref outcomes.
	Refs string
	// Cursor continues a previous page of refs from
	// [PrefetchStatus.NextCursor].
	Cursor string
}

// PrefetchState is the state of a ticket.
type PrefetchState string

// Ticket states.
const (
	PrefetchQueued    PrefetchState = "QUEUED"
	PrefetchRunning   PrefetchState = "RUNNING"
	PrefetchSucceeded PrefetchState = "SUCCEEDED"
	PrefetchPartial   PrefetchState = "PARTIAL"
	PrefetchFailed    PrefetchState = "FAILED"
	PrefetchCanceled  PrefetchState = "CANCELLED"
)

// PrefetchBytes is a ticket's byte accounting. Every value except
// LogicalRequested is a multiple of [PageSize].
type PrefetchBytes struct {
	LogicalRequested  int64
	AlignedRequested  int64
	AlreadyResident   int64
	Queued            int64
	InFlight          int64
	Joined            int64
	FetchedFromOrigin int64
	Admitted          int64
	Failed            int64
}

// PrefetchCoverage reports probed residency of the desired set.
type PrefetchCoverage struct {
	ResidentBytes int64
	ObservedAt    time.Time
}

// PrefetchRefStatus is the outcome of one ref.
type PrefetchRefStatus struct {
	// Index is the ref position in the submitted spec.
	Index int
	Key   string
	// ETag is the resolved version, empty if resolution failed.
	ETag   string
	Offset int64
	Length int64
	// Outcome is "succeeded", "failed", or "pending".
	Outcome string
	// Error is set for failed refs, for example "version_changed".
	Error string
}

// PrefetchStatus mirrors Racer's ticket status. List summaries leave the
// counters, timestamps other than ExpiresAt, Coverage, and Refs zero.
type PrefetchStatus struct {
	ID       string
	CacheUID string
	State    PrefetchState
	// Reason is set only for canceled tickets: "client", "expired",
	// "superseded", "cache_removed", or "passthrough".
	Reason     string
	Priority   string
	Generation uint64
	Sequence   uint64
	// Settled reports that nothing for the current window is queued or in
	// flight. Success means pages were admitted, not that they stay resident.
	Settled       bool
	TTL           time.Duration
	ExpiresAt     time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
	RefsTotal     int64
	RefsSucceeded int64
	RefsFailed    int64
	RefsPending   int64
	Bytes         PrefetchBytes
	// Coverage is set only when requested.
	Coverage *PrefetchCoverage
	// Refs is set only when requested; continue with NextCursor.
	Refs       []PrefetchRefStatus
	NextCursor string
	// Created reports that [Client.Prefetch] created the ticket (HTTP 201).
	Created bool
}

// PrefetchError is a non-2xx prefetch response. It matches [ErrInvalidRequest]
// for 400 and 413, [ErrNotFound] for 404, and [ErrUnavailable] for 429 and 503.
type PrefetchError struct {
	// Status is the HTTP status code.
	Status int
	// Code is the machine-readable code, for example "stale_sequence". It
	// is empty if the response had no valid error body.
	Code string
	// Message is human-readable text with no stable contract.
	Message string
	// RetryAfter is the server's Retry-After, if any.
	RetryAfter time.Duration
}

func (e *PrefetchError) Error() string {
	msg := "racersdk: prefetch: HTTP " + strconv.Itoa(e.Status)
	if e.Code != "" {
		msg += " " + e.Code
	}

	if e.Message != "" {
		msg += ": " + e.Message
	}

	return msg
}

// Is maps the status onto the SDK sentinels.
func (e *PrefetchError) Is(target error) bool {
	switch e.Status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		return target == ErrInvalidRequest
	case http.StatusNotFound:
		return target == ErrNotFound
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return target == ErrUnavailable
	default:
		return false
	}
}

// Prefetch creates or updates ticket id. Racer schedules the missing pages and
// fills them in the background; nothing is pinned, so pages may be evicted
// later. Resubmitting the same spec is idempotent. Update a ticket with a
// higher Sequence, or supersede it with a higher Generation; see
// [PrefetchWindow]. Prefetch shares Stat's lane.
func (c *Client) Prefetch(ctx context.Context, id string, spec PrefetchSpec) (PrefetchStatus, error) {
	const op = "prefetch"

	if err := wire.ValidatePrefetchID(id); err != nil {
		return PrefetchStatus{}, invalid(op, err)
	}

	body, err := prefetchBody(spec)
	if err != nil {
		return PrefetchStatus{}, invalid(op, err)
	}

	header := http.Header{"Content-Type": {"application/json"}}

	if spec.Authorization != "" {
		if err := wire.ValidateOpaque(spec.Authorization); err != nil {
			return PrefetchStatus{}, invalid(op, err)
		}

		header.Set("Authorization", spec.Authorization)
	}

	var out wire.PrefetchStatus

	status, err := c.prefetchCall(ctx, op, http.MethodPut, wire.PrefetchPath+"/"+id, header, body, 0, &out)
	if err != nil {
		return PrefetchStatus{}, err
	}

	s := fromWirePrefetch(out)
	s.Created = status == http.StatusCreated

	return s, nil
}

// PrefetchStatus returns the status of ticket id.
func (c *Client) PrefetchStatus(ctx context.Context, id string, opts StatusOptions) (PrefetchStatus, error) {
	const op = "prefetch status"

	if err := wire.ValidatePrefetchID(id); err != nil {
		return PrefetchStatus{}, invalid(op, err)
	}

	if opts.Wait < 0 || opts.Refs != "" && opts.Refs != "failed" && opts.Refs != "all" {
		return PrefetchStatus{}, invalid(op, errors.New("invalid status options"))
	}

	path := wire.PrefetchPath + "/" + id
	if q := (wire.PrefetchQuery{Wait: opts.Wait, Coverage: opts.Coverage, Refs: opts.Refs, Cursor: opts.Cursor}).Encode(); q != "" {
		path += "?" + q
	}

	var out wire.PrefetchStatus
	if _, err := c.prefetchCall(ctx, op, http.MethodGet, path, nil, nil, opts.Wait, &out); err != nil {
		return PrefetchStatus{}, err
	}

	return fromWirePrefetch(out), nil
}

// CancelPrefetch cancels ticket id and returns its canceled status. Racer
// stops scheduling its pages but does not evict anything already loaded.
func (c *Client) CancelPrefetch(ctx context.Context, id string) (PrefetchStatus, error) {
	const op = "cancel prefetch"

	if err := wire.ValidatePrefetchID(id); err != nil {
		return PrefetchStatus{}, invalid(op, err)
	}

	var out wire.PrefetchStatus
	if _, err := c.prefetchCall(ctx, op, http.MethodDelete, wire.PrefetchPath+"/"+id, nil, nil, 0, &out); err != nil {
		return PrefetchStatus{}, err
	}

	return fromWirePrefetch(out), nil
}

// ListPrefetches returns summaries of this cache's tickets on the local node,
// sorted by id.
func (c *Client) ListPrefetches(ctx context.Context) ([]PrefetchStatus, error) {
	const op = "list prefetches"

	var out wire.PrefetchList
	if _, err := c.prefetchCall(ctx, op, http.MethodGet, wire.PrefetchPath, nil, nil, 0, &out); err != nil {
		return nil, err
	}

	list := make([]PrefetchStatus, 0, len(out.Prefetches))
	for _, s := range out.Prefetches {
		list = append(list, fromWirePrefetch(s))
	}

	return list, nil
}

func prefetchBody(spec PrefetchSpec) ([]byte, error) {
	r, err := spec.wire()
	if err != nil {
		return nil, err
	}

	return json.Marshal(r)
}

func (spec PrefetchSpec) wire() (wire.PrefetchRequest, error) {
	r := wire.PrefetchRequest{
		CacheUID:   spec.CacheUID,
		Priority:   spec.Priority,
		Generation: spec.Generation,
		Sequence:   spec.Sequence,
	}

	switch {
	case spec.Priority != "" && !wire.ValidPriority(spec.Priority):
		return r, errors.New("invalid priority")
	case spec.Generation > wire.PrefetchMaxCounter || spec.Sequence > wire.PrefetchMaxCounter:
		return r, errors.New("generation and sequence must be at most 2^53-1")
	case spec.TTL < 0 || spec.MaxPendingBytes < 0:
		return r, errors.New("negative TTL or MaxPendingBytes")
	case len(spec.Refs) == 0:
		return r, errors.New("no refs")
	}

	r.TTLSeconds = uint64(spec.TTL / time.Second)
	if spec.TTL%time.Second != 0 {
		r.TTLSeconds++
	}

	r.MaxPendingBytes = uint64(spec.MaxPendingBytes)
	r.Refs = make([]wire.PrefetchRef, len(spec.Refs))

	for i, ref := range spec.Refs {
		w, err := ref.wire()
		if err != nil {
			return r, err
		}

		r.Refs[i] = w
	}

	return r, nil
}

func (ref PrefetchRef) wire() (wire.PrefetchRef, error) {
	if _, err := wire.ParseKey(ref.Key); err != nil {
		return wire.PrefetchRef{}, err
	}

	if ref.ETag != "" {
		if err := wire.ValidateETag(ref.ETag); err != nil {
			return wire.PrefetchRef{}, err
		}
	}

	if ref.Metadata != "" {
		if err := wire.ValidateOpaque(ref.Metadata); err != nil {
			return wire.PrefetchRef{}, err
		}
	}

	if ref.Offset < 0 || ref.Length < 0 || ref.Length > math.MaxInt64-ref.Offset {
		return wire.PrefetchRef{}, errors.New("invalid range")
	}

	return wire.PrefetchRef{Key: ref.Key, ETag: ref.ETag, Offset: uint64(ref.Offset), Length: uint64(ref.Length), Metadata: ref.Metadata}, nil
}

func fromWirePrefetch(w wire.PrefetchStatus) PrefetchStatus {
	s := PrefetchStatus{
		ID:            w.ID,
		CacheUID:      w.CacheUID,
		State:         PrefetchState(w.State),
		Priority:      w.Priority,
		Generation:    w.Generation,
		Sequence:      w.Sequence,
		Settled:       w.Settled,
		TTL:           seconds(w.TTLSeconds),
		ExpiresAt:     millis(w.ExpiresAtMs),
		CreatedAt:     millis(w.CreatedAtMs),
		UpdatedAt:     millis(w.UpdatedAtMs),
		RefsTotal:     clamp(w.RefsTotal),
		RefsSucceeded: clamp(w.RefsSucceeded),
		RefsFailed:    clamp(w.RefsFailed),
		RefsPending:   clamp(w.RefsPending),
		Bytes: PrefetchBytes{
			LogicalRequested:  clamp(w.Bytes.LogicalRequested),
			AlignedRequested:  clamp(w.Bytes.AlignedRequested),
			AlreadyResident:   clamp(w.Bytes.AlreadyResident),
			Queued:            clamp(w.Bytes.Queued),
			InFlight:          clamp(w.Bytes.InFlight),
			Joined:            clamp(w.Bytes.Joined),
			FetchedFromOrigin: clamp(w.Bytes.FetchedFromOrigin),
			Admitted:          clamp(w.Bytes.Admitted),
			Failed:            clamp(w.Bytes.Failed),
		},
	}

	if w.Reason != nil {
		s.Reason = *w.Reason
	}

	if w.Coverage != nil {
		s.Coverage = &PrefetchCoverage{ResidentBytes: clamp(w.Coverage.ResidentBytes), ObservedAt: millis(w.Coverage.ObservedAtMs)}
	}

	if w.NextCursor != nil {
		s.NextCursor = *w.NextCursor
	}

	for _, r := range w.Refs {
		ref := PrefetchRefStatus{
			Index:   int(min(r.Index, math.MaxInt32)),
			Key:     r.Key,
			Offset:  clamp(r.Offset),
			Length:  clamp(r.Length),
			Outcome: r.Outcome,
		}
		if r.ETag != nil {
			ref.ETag = *r.ETag
		}

		if r.Error != nil {
			ref.Error = *r.Error
		}

		s.Refs = append(s.Refs, ref)
	}

	return s
}

func clamp(v uint64) int64 { return int64(min(v, math.MaxInt64)) }

func seconds(v uint64) time.Duration {
	return time.Duration(min(v, uint64(math.MaxInt64/int64(time.Second)))) * time.Second
}

func millis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}

	return time.UnixMilli(ms).UTC()
}

// prefetchTransport returns the shared JSON transport, creating it lazily.
func (c *Client) prefetchTransport() *http.Transport {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.prefetch == nil {
		dialer := net.Dialer{Timeout: c.limits.dialTimeout}
		c.prefetch = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", c.path)
			},
			MaxIdleConns:        c.maxIdle,
			MaxIdleConnsPerHost: c.maxIdle,
			IdleConnTimeout:     c.limits.idleTimeout,
			DisableCompression:  true,
			ForceAttemptHTTP2:   false,
		}
	}

	return c.prefetch
}

// prefetchCall runs one JSON exchange on the Stat lane. PUT and DELETE are
// idempotent by protocol, so every method is retried once on a reused
// connection that failed before any response arrived.
func (c *Client) prefetchCall(ctx context.Context, op, method, path string, header http.Header, body []byte, wait time.Duration, out any) (int, error) {
	release, err := c.admit(ctx, &c.stat, op)
	if err != nil {
		return 0, err
	}
	defer release()

	ctx, done := c.bind(ctx)
	defer done()

	transport := c.prefetchTransport()

	for attempt := 0; ; attempt++ {
		status, reused, err := c.prefetchOnce(ctx, transport, method, path, header, body, wait, out)
		if err == nil {
			return status, nil
		}

		var pe *PrefetchError
		if errors.As(err, &pe) {
			return 0, err
		}

		if ctx.Err() != nil {
			return 0, contextError(op, ctx)
		}

		if attempt == 0 && reused && status == 0 && staleConnectionError(err) {
			continue
		}

		if status == 0 {
			return 0, failure(wire.ErrorUnavailable, op, err)
		}

		return 0, failure(wire.ErrorProtocol, op, err)
	}
}

func (c *Client) prefetchOnce(ctx context.Context, transport *http.Transport, method, path string, header http.Header, body []byte, wait time.Duration, out any) (int, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, c.limits.headerTimeout+wait)
	defer cancel()

	var reused bool

	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }})

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, "http://racer"+path, reader)
	if err != nil {
		return 0, false, err
	}

	for name, values := range header {
		req.Header[name] = values
	}

	req.Header["User-Agent"] = nil

	res, err := transport.RoundTrip(req)
	if err != nil {
		return 0, reused, err
	}
	defer closeQuietly(res.Body)

	raw, err := io.ReadAll(io.LimitReader(res.Body, maxPrefetchResponse+1))
	if err != nil {
		return res.StatusCode, reused, err
	}

	if len(raw) > maxPrefetchResponse {
		return res.StatusCode, reused, errors.New("response too large")
	}

	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusCreated {
		return res.StatusCode, reused, prefetchError(res, raw)
	}

	if err := json.Unmarshal(raw, out); err != nil {
		return res.StatusCode, reused, err
	}

	return res.StatusCode, reused, nil
}

func prefetchError(res *http.Response, raw []byte) error {
	e := &PrefetchError{Status: res.StatusCode, RetryAfter: wire.ParseRetryAfter(res.Header.Get("Retry-After"))}

	var body wire.PrefetchErrorBody
	if json.Unmarshal(raw, &body) == nil && body.Code != "" {
		e.Code, e.Message = body.Code, body.Message
	} else {
		e.Message = http.StatusText(res.StatusCode)
	}

	return e
}

// PrefetchPart is one ticket produced by [SplitPrefetch].
type PrefetchPart struct {
	ID   string
	Spec PrefetchSpec
}

// SplitPrefetch splits spec into tickets that each fit maxRefs refs and a
// maxBodyBytes JSON body. Zero or negative limits use Racer's defaults (16384
// refs, 4 MiB). Part ids are "<id>.<n>" counting from 0, even for a single
// part, so they stay stable as the ref list grows. It returns nil if spec is
// invalid, a part id would be invalid, or a single ref cannot fit.
func SplitPrefetch(id string, spec PrefetchSpec, maxRefs, maxBodyBytes int) []PrefetchPart {
	if maxRefs <= 0 {
		maxRefs = wire.PrefetchDefaultRefs
	}

	if maxBodyBytes <= 0 {
		maxBodyBytes = wire.PrefetchDefaultBody
	}

	r, err := spec.wire()
	if err != nil {
		return nil
	}

	r.Refs = []wire.PrefetchRef{}

	base, err := json.Marshal(r)
	if err != nil {
		return nil
	}

	var parts []PrefetchPart

	start, size := 0, len(base)
	flush := func(end int) bool {
		part := spec
		part.Refs = spec.Refs[start:end]
		partID := id + "." + strconv.Itoa(len(parts))

		if wire.ValidatePrefetchID(partID) != nil {
			return false
		}

		parts = append(parts, PrefetchPart{ID: partID, Spec: part})
		start, size = end, len(base)

		return true
	}

	for i, ref := range spec.Refs {
		w, _ := ref.wire() //nolint:errcheck // Validated by prefetchBody above.

		encoded, err := json.Marshal(w)
		if err != nil {
			return nil
		}

		cost := len(encoded)
		if i > start {
			cost++ // comma
		}

		if i > start && (i-start >= maxRefs || size+cost > maxBodyBytes) {
			if !flush(i) {
				return nil
			}

			cost = len(encoded)
		}

		if size+cost > maxBodyBytes {
			return nil
		}

		size += cost
	}

	if !flush(len(spec.Refs)) {
		return nil
	}

	return parts
}

// PrefetchWindow submits a rolling window as one ticket whose desired set is
// replaced by each update. It is safe for concurrent use.
//
// Counters only move forward, even when a call fails: the server may have
// applied a request whose response was lost, and resubmitting different refs
// at the same position would be rejected as a payload mismatch.
type PrefetchWindow struct {
	client    *Client
	id        string
	mu        sync.Mutex
	spec      PrefetchSpec
	submitted bool
}

// NewPrefetchWindow returns a window for ticket id. spec supplies every field
// except Refs; its Generation and Sequence are used for the first submission.
func (c *Client) NewPrefetchWindow(id string, spec PrefetchSpec) *PrefetchWindow {
	spec.Refs = nil
	return &PrefetchWindow{client: c, id: id, spec: spec}
}

// ID returns the ticket id.
func (w *PrefetchWindow) ID() string { return w.id }

// Position returns the generation and sequence of the last submission, or of
// the next one before any submission.
func (w *PrefetchWindow) Position() (generation, sequence uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.spec.Generation, w.spec.Sequence
}

// Update replaces the desired set with refs at the next sequence number. The
// first submission uses the position given to [Client.NewPrefetchWindow].
func (w *PrefetchWindow) Update(ctx context.Context, refs []PrefetchRef) (PrefetchStatus, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	next := w.spec
	if w.submitted {
		if next.Sequence >= wire.PrefetchMaxCounter {
			return PrefetchStatus{}, invalid("prefetch window", errors.New("sequence exhausted; call Reset"))
		}

		next.Sequence++
	}

	return w.submit(ctx, next, refs)
}

// Reset supersedes the window with refs at the next generation and sequence 0.
func (w *PrefetchWindow) Reset(ctx context.Context, refs []PrefetchRef) (PrefetchStatus, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.spec.Generation >= wire.PrefetchMaxCounter {
		return PrefetchStatus{}, invalid("prefetch window", errors.New("generation exhausted"))
	}

	next := w.spec
	next.Generation++
	next.Sequence = 0

	return w.submit(ctx, next, refs)
}

// submit advances to next unless refs are invalid, which never reach Racer and
// so do not consume a position.
func (w *PrefetchWindow) submit(ctx context.Context, next PrefetchSpec, refs []PrefetchRef) (PrefetchStatus, error) {
	spec := next
	spec.Refs = refs

	if _, err := prefetchBody(spec); err != nil {
		return PrefetchStatus{}, invalid("prefetch window", err)
	}

	w.spec, w.submitted = next, true

	return w.client.Prefetch(ctx, w.id, spec)
}
