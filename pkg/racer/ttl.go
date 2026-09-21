// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// metadataTTL follows Racer's shared-cache policy, preserving unspecified TTLs.
func metadataTTL(h http.Header) (*time.Duration, error) {
	invalid := func() (*time.Duration, error) {
		return nil, fmt.Errorf("%w: invalid metadata TTL", ErrProtocol)
	}

	var maxAge, sharedMaxAge *uint64

	disabled := false

	for _, value := range h.Values("Cache-Control") {
		// Extension values and field lists can contain quoted commas and escapes.
		start := 0
		quoted, escaped := false, false

		for i := 0; i <= len(value); i++ {
			if i < len(value) {
				b := value[i]

				if escaped {
					escaped = false
					continue
				}

				if quoted && b == '\\' {
					escaped = true
					continue
				}

				if b == '"' {
					quoted = !quoted
				}

				if b != ',' || quoted {
					continue
				}
			}

			if quoted || escaped {
				return invalid()
			}

			name, arg, hasArg := strings.Cut(strings.TrimSpace(value[start:i]), "=")

			name, arg = strings.TrimSpace(name), strings.TrimSpace(arg)
			if !cacheToken(name) || hasArg && !cacheDirectiveValue(arg) {
				return invalid()
			}

			var slot **uint64

			switch strings.ToLower(name) {
			case "no-cache", "no-store", "private":
				disabled = true
			case "max-age":
				slot = &maxAge
			case "s-maxage":
				slot = &sharedMaxAge
			}

			if slot != nil {
				if !hasArg || *slot != nil {
					return invalid()
				}

				n, ok := cacheSeconds(strings.Trim(arg, "\""))
				if !ok {
					return invalid()
				}

				*slot = &n
			}

			start = i + 1
		}
	}

	var age uint64

	if values := h.Values("Age"); len(values) != 0 {
		var ok bool

		if len(values) != 1 {
			return invalid()
		}

		age, ok = cacheSeconds(strings.TrimSpace(values[0]))
		if !ok {
			return invalid()
		}
	}

	if disabled {
		ttl := time.Duration(0)
		return &ttl, nil
	}

	seconds := sharedMaxAge
	if seconds == nil {
		seconds = maxAge
	}

	if seconds == nil {
		return nil, nil
	}

	var remaining uint64
	if *seconds > age {
		remaining = *seconds - age
	}

	if remaining > uint64(math.MaxInt64/int64(time.Second)) {
		return invalid()
	}

	ttl := time.Duration(remaining) * time.Second

	return &ttl, nil
}

func cacheSeconds(s string) (uint64, bool) {
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

func cacheToken(s string) bool {
	if s == "" {
		return false
	}

	for i := range s {
		b := s[i]
		if (b < '0' || b > '9') && (b < 'a' || b > 'z') && (b < 'A' || b > 'Z') && !strings.ContainsRune("!#$%&'*+-.^_`|~", rune(b)) {
			return false
		}
	}

	return true
}

func cacheDirectiveValue(s string) bool {
	if !strings.HasPrefix(s, "\"") {
		return cacheToken(s)
	}

	if len(s) < 2 || s[len(s)-1] != '"' {
		return false
	}

	escaped := false

	for i := 1; i < len(s)-1; i++ {
		b := s[i]
		if b < ' ' && b != '\t' || b == 127 || !escaped && b == '"' {
			return false
		}

		if escaped {
			escaped = false
		} else if b == '\\' {
			escaped = true
		}
	}

	return !escaped
}
