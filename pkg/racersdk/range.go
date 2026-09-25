// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"net/http"
	"strconv"
	"strings"
)

// RangeDecision selects the response interval for a GET of an immutable object.
type RangeDecision struct {
	Offset int64
	Length int64
	// StatusCode is 200 (full object), 206 (single range), or 416 (unsatisfiable).
	// Offset and Length are zero for 416; callers must not open a payload.
	StatusCode int
}

// DecideRange applies Racer's Range and If-Range semantics to GET headers.
// Metadata must have a nonnegative Size. A present If-Range permits a range only
// when it is one matching strong ETag. Malformed, reversed, multipart, repeated,
// or unsigned-64-bit-overflow ranges are ignored, yielding the full object.
// Valid endpoints are clipped to Size; unsatisfiable ranges yield 416.
// Callers evaluate other preconditions first and handle HEAD separately, ignoring
// Range and If-Range. This function neither changes headers nor opens payloads.
func DecideRange(h http.Header, m Metadata) RangeDecision {
	offset, length, status := objectRange(h, m)

	return RangeDecision{Offset: offset, Length: length, StatusCode: status}
}

func objectRange(h http.Header, m Metadata) (start, length int64, status int) {
	full := func() (int64, int64, int) { return 0, m.Size, http.StatusOK }
	if values := h.Values("If-Range"); len(values) != 0 && (len(values) != 1 || !strongETag(m.ETag) || strings.Trim(values[0], " \t") != m.ETag) {
		return full()
	}

	values := h.Values("Range")
	if len(values) != 1 {
		return full()
	}

	unit, spec, ok := strings.Cut(strings.Trim(values[0], " \t"), "=")
	if !ok || !strings.EqualFold(unit, "bytes") {
		return full()
	}

	a, b, ok := strings.Cut(spec, "-")
	if !ok {
		return full()
	}
	// Rust parses unsigned 64-bit endpoints, clipping before conversion to size.
	parse := func(s string) (uint64, bool) {
		if s == "" {
			return 0, false
		}

		for i := range s {
			if s[i] < '0' || s[i] > '9' {
				return 0, false
			}
		}

		n, err := strconv.ParseUint(s, 10, 64)

		return n, err == nil
	}
	size := uint64(m.Size)

	if a == "" {
		n, valid := parse(b)
		if !valid {
			return full()
		}

		if n == 0 || size == 0 {
			return 0, 0, http.StatusRequestedRangeNotSatisfiable
		}

		length = int64(min(n, size))

		return m.Size - length, length, http.StatusPartialContent
	}

	first, valid := parse(a)
	if !valid {
		return full()
	}

	end := size

	if b != "" {
		last, valid := parse(b)
		if !valid || last < first {
			return full()
		}

		if last < size {
			end = last + 1
		}
	}

	if first >= size {
		return 0, 0, http.StatusRequestedRangeNotSatisfiable
	}

	return int64(first), int64(end - first), http.StatusPartialContent
}
