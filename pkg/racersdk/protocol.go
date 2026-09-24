// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racersdk provides a concurrent HTTP client and an origin handler for Racer.
// Clients can address either a volume listener or an origin using the same API.
package racersdk

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"
)

// PageSize is Racer's fixed, aligned payload page size.
const PageSize int64 = 64 << 20

var (
	ErrVersionChanged = errors.New("racer: object version changed")
	ErrNoValidator    = errors.New("racer: a canonical checksum ETag is required")
	ErrProtocol       = errors.New("racer: invalid HTTP response")
)

// Metadata describes a representation. ETag is mandatory: a quoted, 64-character
// lowercase hexadecimal representation ID. It may be a content checksum or an
// opaque version identity, but must never identify different bytes at one target.
type Metadata struct {
	Size int64
	ETag string
	// ContentType is optional and bounded to 256 HTTP field-value bytes.
	ContentType string
	// TTL governs metadata freshness, not the lifetime of versioned cached pages.
	// Nil leaves freshness unspecified; a pointer to zero requests immediate
	// revalidation. Origins require nonnegative values and round down to whole
	// seconds on the wire. Clients report remaining freshness after HTTP Age.
	TTL *time.Duration
}

// HTTPError is an unexpected status. Use errors.As to inspect it, or errors.Is
// with ErrVersionChanged, fs.ErrNotExist, or fs.ErrPermission.
type HTTPError struct {
	Method          string
	Target          string
	StatusCode      int
	WWWAuthenticate string
	RetryAfter      string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("racer: %s %s: HTTP %d", e.Method, e.Target, e.StatusCode)
}

func (e *HTTPError) Is(err error) bool {
	return e.StatusCode == http.StatusPreconditionFailed && err == ErrVersionChanged ||
		e.StatusCode == http.StatusNotFound && err == fs.ErrNotExist ||
		(e.StatusCode == http.StatusForbidden || e.StatusCode == http.StatusUnauthorized) && err == fs.ErrPermission
}

func validTarget(s string) bool {
	if s == "" || s[0] != '/' {
		return false
	}

	for i := range s {
		if s[i] <= ' ' || s[i] >= 127 || s[i] == '#' {
			return false
		}
	}

	return true
}

func validETag(s string) bool {
	s = strings.TrimPrefix(s, "W/")
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return false
	}

	for i := 1; i < len(s)-1; i++ {
		if s[i] < 0x21 || s[i] == '"' || s[i] == 0x7f {
			return false
		}
	}

	return true
}

func strongETag(s string) bool { return validETag(s) && !strings.HasPrefix(s, "W/") }

// Representation validators are stricter than client conditional-header grammar.
func checksumETag(s string) bool {
	if len(s) != 66 || s[0] != '"' || s[65] != '"' {
		return false
	}

	for _, c := range s[1:65] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}

	return true
}

func contentRange(start, end, size int64) string {
	return fmt.Sprintf("bytes %d-%d/%d", start, end, size)
}

// Small scratch buffers bound streaming memory independently of object/page size.
var copyBuffers = sync.Pool{New: func() any { b := make([]byte, 32<<10); return &b }}
