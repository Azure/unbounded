// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package metrics

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// NewHTTPMiddleware returns counters and histogram for HTTP handler
// instrumentation with the given metric namespace prefix. The returned
// InstrumentHandler wraps an http.Handler to record request count and
// duration.
//
// This function is safe to call multiple times with the same namespace;
// subsequent calls return a middleware backed by the same collectors.
func NewHTTPMiddleware(namespace string) *HTTPMiddleware {
	httpMiddlewareMu.Lock()
	defer httpMiddlewareMu.Unlock()

	if m, ok := httpMiddlewares[namespace]; ok {
		return m
	}

	m := &HTTPMiddleware{
		requests: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "http_requests_total",
			Help:      "Total number of HTTP requests by method, path, and status code.",
		}, []string{"method", "path", "code"}),
		duration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "http_request_duration_seconds",
			Help:      "HTTP request duration in seconds by method and path.",
			Buckets:   DefaultDurationBuckets,
		}, []string{"method", "path"}),
	}
	httpMiddlewares[namespace] = m

	return m
}

var (
	httpMiddlewareMu sync.Mutex
	httpMiddlewares  = make(map[string]*HTTPMiddleware)
)

// HTTPMiddleware holds the Prometheus metrics for HTTP handler instrumentation.
type HTTPMiddleware struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

// Wrap returns an http.Handler that records metrics and delegates to next.
func (m *HTTPMiddleware) Wrap(path string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rw, r)

		elapsed := time.Since(start).Seconds()

		m.requests.WithLabelValues(r.Method, path, strconv.Itoa(rw.statusCode)).Inc()
		m.duration.WithLabelValues(r.Method, path).Observe(elapsed)
	})
}

// WrapFunc is a convenience method that wraps an http.HandlerFunc.
func (m *HTTPMiddleware) WrapFunc(path string, next http.HandlerFunc) http.Handler {
	return m.Wrap(path, next)
}

type responseWriter struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
}

func (rw *responseWriter) WriteHeader(code int) {
	if !rw.wroteHeader {
		rw.statusCode = code
		rw.wroteHeader = true
	}

	rw.ResponseWriter.WriteHeader(code)
}

// Unwrap returns the underlying ResponseWriter, allowing http.ResponseController
// and other callers to access optional interfaces (Hijacker, Flusher, etc.).
func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}
