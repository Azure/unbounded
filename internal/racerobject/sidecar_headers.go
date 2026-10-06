// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racerobject

import (
	"net/http"
	"strconv"
	"strings"
)

func sidecarSupportedHeaders(h http.Header) bool {
	for name := range h {
		name = strings.ToLower(name)
		switch name {
		case "if-modified-since", "if-unmodified-since", "if-range":
			return false
		case "x-amz-date", "x-amz-content-sha256", "x-amz-security-token", "x-amz-region-set", "x-amz-s3session-token", "x-amz-user-agent":
			// These only carry ignored credentials, signing data, or SDK telemetry.
			continue
		}
		// Unknown S3 headers may change authorization or object-read semantics.
		// Reject even empty values rather than silently dropping those requirements.
		if strings.HasPrefix(name, "x-amz-") {
			return false
		}
	}

	return true
}

type sidecarEntityTag struct {
	value string
	weak  bool
}

type sidecarTags struct {
	present, wildcard bool
	tags              []sidecarEntityTag
}

func sidecarCondition(h http.Header, name string) (sidecarTags, bool) {
	values, present := h[http.CanonicalHeaderKey(name)]

	result := sidecarTags{present: present}
	if !present {
		return result, true
	}

	s := strings.Trim(strings.Join(values, ","), " \t")
	if s == "*" {
		result.wildcard = true
		return result, true
	}

	for s != "" {
		// Commas inside a quoted tag are literal; backslashes are not escapes.
		if s[0] == ',' {
			s = strings.TrimLeft(s[1:], " \t")
			continue
		}

		tag := sidecarEntityTag{}
		if strings.HasPrefix(s, "W/") {
			tag.weak = true
			s = s[2:]
		}

		if len(s) < 2 || s[0] != '"' {
			return result, false
		}

		end := strings.IndexByte(s[1:], '"')
		if end < 0 {
			return result, false
		}

		end += 2
		for _, c := range []byte(s[1 : end-1]) {
			if c < 0x21 || c == 0x7f {
				return result, false
			}
		}

		tag.value = s[:end]
		result.tags = append(result.tags, tag)

		s = strings.TrimLeft(s[end:], " \t")
		if s == "" {
			break
		}

		if s[0] != ',' {
			return result, false
		}

		s = strings.TrimLeft(s[1:], " \t")
	}

	return result, len(result.tags) != 0
}

func (c sidecarTags) matches(tag string, weak bool) bool {
	if c.wildcard {
		return true
	}

	for _, candidate := range c.tags {
		if candidate.value == tag && (weak || !candidate.weak) {
			return true
		}
	}

	return false
}

func sidecarRange(h http.Header, size uint64) (offset, length uint64, partial bool, status int) {
	values, present := h["Range"]
	if !present {
		return 0, size, false, 0
	}

	if len(values) != 1 {
		return 0, 0, false, http.StatusBadRequest
	}

	s, ok := strings.CutPrefix(strings.Trim(values[0], " \t"), "bytes=")
	if !ok {
		return 0, 0, false, http.StatusBadRequest
	}

	first, last, ok := strings.Cut(s, "-")
	if !ok || first == "" && last == "" {
		return 0, 0, false, http.StatusBadRequest
	}

	parse := func(s string) (uint64, bool) {
		if s == "" {
			return 0, false
		}

		for _, c := range []byte(s) {
			if c < '0' || c > '9' {
				return 0, false
			}
		}

		n, err := strconv.ParseUint(s, 10, 64)

		return n, err == nil
	}
	if first == "" {
		suffix, valid := parse(last)
		if !valid {
			return 0, 0, false, http.StatusBadRequest
		}

		if suffix == 0 || size == 0 {
			return 0, 0, false, http.StatusRequestedRangeNotSatisfiable
		}

		length = min(suffix, size)

		return size - length, length, true, 0
	}

	start, ok := parse(first)
	if !ok {
		return 0, 0, false, http.StatusBadRequest
	}

	end := uint64(0)
	if last != "" {
		end, ok = parse(last)
		if !ok || end < start {
			return 0, 0, false, http.StatusBadRequest
		}
	}

	if start >= size {
		return 0, 0, false, http.StatusRequestedRangeNotSatisfiable
	}

	if last == "" || end >= size {
		end = size - 1
	}

	return start, end - start + 1, true, 0
}
