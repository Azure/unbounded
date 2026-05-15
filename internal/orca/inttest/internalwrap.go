// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integrationtest

package inttest

import (
	"net/http"
	"sync"
	"sync/atomic"
)

// CountingInternalHandlerWrap is an http.Handler decorator factory
// that counts response status codes per receiving replica IP. Used
// by TestPeerNotCoordinatorFallback to assert a peer's
// /internal/fill handler returned 409 (proving the cluster.go 409
// fallback path actually fired on the requesting replica).
//
// One CountingInternalHandlerWrap is shared across all replicas in
// the harness; each replica's wrapped handler stamps its self IP
// onto the response writer so counts can be attributed back.
type CountingInternalHandlerWrap struct {
	mu      sync.Mutex
	counts  map[string]map[int]*atomic.Int64 // selfIP -> status -> count
	defined map[string]struct{}
}

// NewCountingInternalHandlerWrap returns an empty wrapper.
func NewCountingInternalHandlerWrap() *CountingInternalHandlerWrap {
	return &CountingInternalHandlerWrap{
		counts:  make(map[string]map[int]*atomic.Int64),
		defined: make(map[string]struct{}),
	}
}

// WrapFor returns a wrap function suitable for app.WithInternalHandlerWrap
// that attributes status-code counts back to the named selfIP.
func (w *CountingInternalHandlerWrap) WrapFor(selfIP string) func(http.Handler) http.Handler {
	w.mu.Lock()
	if _, ok := w.counts[selfIP]; !ok {
		w.counts[selfIP] = make(map[int]*atomic.Int64)
	}

	w.defined[selfIP] = struct{}{}
	w.mu.Unlock()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			cw := &countingResponseWriter{ResponseWriter: rw, status: http.StatusOK}
			next.ServeHTTP(cw, req)
			w.record(selfIP, cw.status)
		})
	}
}

// Count returns the number of responses with the given status code
// observed at the named selfIP.
func (w *CountingInternalHandlerWrap) Count(selfIP string, status int) int64 {
	w.mu.Lock()
	defer w.mu.Unlock()

	byStatus, ok := w.counts[selfIP]
	if !ok {
		return 0
	}

	c, ok := byStatus[status]
	if !ok {
		return 0
	}

	return c.Load()
}

// CountAcross returns the count summed across all known selfIPs.
func (w *CountingInternalHandlerWrap) CountAcross(status int) int64 {
	w.mu.Lock()
	defer w.mu.Unlock()

	var total int64

	for _, byStatus := range w.counts {
		if c, ok := byStatus[status]; ok {
			total += c.Load()
		}
	}

	return total
}

func (w *CountingInternalHandlerWrap) record(selfIP string, status int) {
	w.mu.Lock()

	byStatus, ok := w.counts[selfIP]
	if !ok {
		byStatus = make(map[int]*atomic.Int64)
		w.counts[selfIP] = byStatus
	}

	c, ok := byStatus[status]
	if !ok {
		c = &atomic.Int64{}
		byStatus[status] = c
	}

	w.mu.Unlock()
	c.Add(1)
}

// countingResponseWriter records the first WriteHeader status; if no
// WriteHeader is ever called, http.StatusOK is recorded (matching the
// net/http default).
type countingResponseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (c *countingResponseWriter) WriteHeader(status int) {
	if !c.wroteHeader {
		c.status = status
		c.wroteHeader = true
	}

	c.ResponseWriter.WriteHeader(status)
}

func (c *countingResponseWriter) Write(p []byte) (int, error) {
	if !c.wroteHeader {
		c.wroteHeader = true
	}

	return c.ResponseWriter.Write(p)
}

// Flush passes through to the embedded ResponseWriter when it
// implements http.Flusher. Without this method, wrapping a handler
// that streams via Flush() (e.g. the edge handler's per-chunk
// f.Flush()) would silently degrade to buffered responses.
func (c *countingResponseWriter) Flush() {
	if fl, ok := c.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}
