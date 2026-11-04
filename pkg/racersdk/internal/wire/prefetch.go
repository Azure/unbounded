// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// PrefetchPath is the collection path of the client-socket prefetch API.
const PrefetchPath = "/v1/prefetches"

// Prefetch limits shared by the SDK and the fake. Server limits are configurable;
// these are the documented defaults.
const (
	PrefetchMaxID          = 128
	PrefetchMaxCounter     = 1<<53 - 1
	PrefetchDefaultRefs    = 16384
	PrefetchDefaultBody    = 4194304
	PrefetchRefsPage       = 256
	PrefetchMaxRetryAfter  = 60
	PrefetchDefaultTTLSecs = 600
)

// Prefetch error codes carried in JSON error bodies.
const (
	PrefetchInvalidRequest   = "invalid_request"
	PrefetchNotFound         = "not_found"
	PrefetchMethodNotAllowed = "method_not_allowed"
	PrefetchCacheUIDMismatch = "cache_uid_mismatch"
	PrefetchPayloadMismatch  = "payload_mismatch"
	PrefetchStaleSequence    = "stale_sequence"
	PrefetchStaleGeneration  = "stale_generation"
	PrefetchPassthrough      = "passthrough"
	PrefetchTooLarge         = "too_large"
	PrefetchTicketLimit      = "ticket_limit"
	PrefetchPendingLimit     = "pending_limit"
	PrefetchUnavailable      = "unavailable"
)

// PrefetchRequest is the PUT body. Field order is the canonical encoding order.
type PrefetchRequest struct {
	CacheUID        string        `json:"cache_uid,omitempty"`
	Priority        string        `json:"priority,omitempty"`
	Generation      uint64        `json:"generation"`
	Sequence        uint64        `json:"sequence"`
	TTLSeconds      uint64        `json:"ttl_seconds,omitempty"`
	MaxPendingBytes uint64        `json:"max_pending_bytes,omitempty"`
	Refs            []PrefetchRef `json:"refs"`
}

// PrefetchRef is one requested object range.
type PrefetchRef struct {
	Key      string `json:"key"`
	ETag     string `json:"etag,omitempty"`
	Offset   uint64 `json:"offset,omitempty"`
	Length   uint64 `json:"length,omitempty"`
	Metadata string `json:"metadata,omitempty"`
}

// PrefetchBytes is the byte accounting of a ticket.
type PrefetchBytes struct {
	LogicalRequested  uint64 `json:"logical_requested"`
	AlignedRequested  uint64 `json:"aligned_requested"`
	AlreadyResident   uint64 `json:"already_resident"`
	Queued            uint64 `json:"queued"`
	InFlight          uint64 `json:"in_flight"`
	Joined            uint64 `json:"joined"`
	FetchedFromOrigin uint64 `json:"fetched_from_origin"`
	Admitted          uint64 `json:"admitted"`
	Failed            uint64 `json:"failed"`
}

// PrefetchCoverage is the probed residency of the current desired set.
type PrefetchCoverage struct {
	ResidentBytes uint64 `json:"resident_bytes"`
	ObservedAtMs  int64  `json:"observed_at_ms"`
}

// PrefetchRefStatus is the outcome of one ref.
type PrefetchRefStatus struct {
	Index   uint64  `json:"index"`
	Key     string  `json:"key"`
	ETag    *string `json:"etag"`
	Offset  uint64  `json:"offset"`
	Length  uint64  `json:"length"`
	Outcome string  `json:"outcome"`
	Error   *string `json:"error,omitempty"`
}

// PrefetchStatus is a ticket status or list summary.
type PrefetchStatus struct {
	ID            string              `json:"id"`
	CacheUID      string              `json:"cache_uid,omitempty"`
	State         string              `json:"state"`
	Reason        *string             `json:"reason"`
	Priority      string              `json:"priority"`
	Generation    uint64              `json:"generation"`
	Sequence      uint64              `json:"sequence"`
	Settled       bool                `json:"settled"`
	TTLSeconds    uint64              `json:"ttl_seconds,omitempty"`
	ExpiresAtMs   int64               `json:"expires_at_ms"`
	CreatedAtMs   int64               `json:"created_at_ms,omitempty"`
	UpdatedAtMs   int64               `json:"updated_at_ms,omitempty"`
	RefsTotal     uint64              `json:"refs_total,omitempty"`
	RefsSucceeded uint64              `json:"refs_succeeded,omitempty"`
	RefsFailed    uint64              `json:"refs_failed,omitempty"`
	RefsPending   uint64              `json:"refs_pending,omitempty"`
	Bytes         PrefetchBytes       `json:"bytes"`
	Coverage      *PrefetchCoverage   `json:"coverage,omitempty"`
	Refs          []PrefetchRefStatus `json:"refs,omitempty"`
	NextCursor    *string             `json:"next_cursor,omitempty"`
}

// PrefetchList is the collection response.
type PrefetchList struct {
	Prefetches []PrefetchStatus `json:"prefetches"`
}

// PrefetchErrorBody is the body of every non-2xx prefetch response.
type PrefetchErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// PrefetchQuery holds GET status parameters.
type PrefetchQuery struct {
	Wait     time.Duration
	Coverage bool
	Refs     string
	Cursor   string
}

// ValidatePrefetchID checks [A-Za-z0-9._-]{1,128}, excluding "." and "..".
func ValidatePrefetchID(id string) error {
	bad := failure(ErrorInvalidArgument, "prefetch id", errors.New("invalid prefetch id"))
	if id == "" || len(id) > PrefetchMaxID || id == "." || id == ".." {
		return bad
	}

	for i := range len(id) {
		c := id[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '.' && c != '_' && c != '-' {
			return bad
		}
	}

	return nil
}

// ValidPriority reports whether p is a non-empty priority name.
func ValidPriority(p string) bool {
	return p == "high" || p == "normal" || p == "low"
}

// FormatWait encodes a wait as whole seconds ("30s") when exact, else as
// milliseconds rounded up ("1500ms"). Zero encodes as "0s".
func FormatWait(d time.Duration) string {
	if d%time.Second == 0 {
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	}

	ms := d / time.Millisecond
	if d%time.Millisecond != 0 {
		ms++
	}

	return strconv.FormatInt(int64(ms), 10) + "ms"
}

// Encode returns the raw query string, with parameters in a fixed order.
func (q PrefetchQuery) Encode() string {
	var parts []string
	if q.Wait > 0 {
		parts = append(parts, "wait="+url.QueryEscape(FormatWait(q.Wait)))
	}

	if q.Coverage {
		parts = append(parts, "coverage=1")
	}

	if q.Refs != "" {
		parts = append(parts, "refs="+url.QueryEscape(q.Refs))
	}

	if q.Cursor != "" {
		parts = append(parts, "cursor="+url.QueryEscape(q.Cursor))
	}

	return strings.Join(parts, "&")
}

// ParsePrefetchQuery strictly parses a status query: unknown or duplicate
// parameters, an unknown refs filter, or a wait above maxWait fail.
func ParsePrefetchQuery(raw string, maxWait time.Duration) (PrefetchQuery, error) {
	var q PrefetchQuery

	bad := failure(ErrorInvalidArgument, "prefetch query", nil)

	values, err := url.ParseQuery(raw)
	if err != nil {
		return q, bad
	}

	for name, v := range values {
		if len(v) != 1 {
			return q, bad
		}

		value := v[0]

		switch name {
		case "wait":
			// Racer rejects signs that Go accepts.
			d, err := time.ParseDuration(value)
			if err != nil || strings.HasPrefix(value, "+") || strings.HasPrefix(value, "-") || d > maxWait {
				return q, bad
			}

			q.Wait = d
		case "coverage":
			switch value {
			case "1", "true":
				q.Coverage = true
			case "0", "false":
			default:
				return q, bad
			}
		case "refs":
			if value != "failed" && value != "all" {
				return q, bad
			}

			q.Refs = value
		case "cursor":
			if !validCursor(value) {
				return q, bad
			}

			q.Cursor = value
		default:
			return q, bad
		}
	}

	if q.Cursor != "" && q.Refs == "" {
		return q, bad
	}

	return q, nil
}

// validCursor matches Racer's cursor syntax: [A-Za-z0-9._-]{1,256}.
func validCursor(s string) bool {
	if s == "" || len(s) > 256 {
		return false
	}

	for i := range len(s) {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '.' && c != '_' && c != '-' {
			return false
		}
	}

	return true
}

// ParseRetryAfter parses an integer Retry-After in seconds. Invalid values are zero.
func ParseRetryAfter(value string) time.Duration {
	n, err := strconv.ParseUint(strings.TrimSpace(value), 10, 32)
	if err != nil {
		return 0
	}

	return time.Duration(n) * time.Second
}
