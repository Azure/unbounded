// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package streaming

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/httprange"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

type LocalRangeStore interface {
	OpenRange(ctx context.Context, d digest.Digest, requested httprange.Range) (io.ReadCloser, int64, error)
}

type ProviderDiscovery interface {
	FindProviders(ctx context.Context, d digest.Digest) ([]ifaces.Provider, error)
}

type PeerRangeFetcher interface {
	FetchRangeFromPeer(ctx context.Context, peerAddr string, d digest.Digest, requested httprange.Range) (io.ReadCloser, int64, string, error)
}

type OriginRangeFetcher interface {
	FetchRange(ctx context.Context, origin OriginURL, requested httprange.Range) (io.ReadCloser, int64, string, error)
}

// Tuning values rather than operator configuration: changing one requires a
// latency measurement, so they live in code with the behavior they affect.
const (
	defaultPeerLookupTimeout = 250 * time.Millisecond
	defaultMaxPeerAttempts   = 3
)

type Options struct {
	URLPolicy URLPolicy

	// PeerLookupTimeout and MaxPeerAttempts fall back to defaults when zero.
	PeerLookupTimeout time.Duration
	MaxPeerAttempts   int

	SelfPeerID   ifaces.NodeID
	StartupGated bool
	Logger       *slog.Logger
	Metrics      MetricsHooks
}

type MetricsHooks struct {
	OnRequest   func(source, outcome string, duration time.Duration, bytes int64)
	OnFirstByte func(source string, duration time.Duration)
	OnInflight  func(source string, delta int)
	OnReject    func(reason string)
}

// Server serves OverlayBD's node-local range proxy contract.
type Server struct {
	local     LocalRangeStore
	discovery ProviderDiscovery
	peer      PeerRangeFetcher
	origin    OriginRangeFetcher
	opts      Options
	failures  *providerFailures
	draining  atomic.Bool
	ready     atomic.Bool
}

func NewServer(local LocalRangeStore, discovery ProviderDiscovery, peer PeerRangeFetcher, origin OriginRangeFetcher, opts Options) (*Server, error) {
	if local == nil || discovery == nil || peer == nil || origin == nil {
		return nil, fmt.Errorf("streaming server: all dependencies are required")
	}

	if opts.PeerLookupTimeout <= 0 {
		opts.PeerLookupTimeout = defaultPeerLookupTimeout
	}

	if opts.MaxPeerAttempts < 1 {
		opts.MaxPeerAttempts = defaultMaxPeerAttempts
	}

	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	opts.Logger = opts.Logger.With(slog.String("subsystem", "streaming"))

	s := &Server{local: local, discovery: discovery, peer: peer, origin: origin, opts: opts, failures: newProviderFailures()}
	if !opts.StartupGated {
		s.ready.Store(true)
	}

	return s, nil
}

// Drain prevents new reads while allowing the shared HTTP server to finish
// active handlers.
func (s *Server) Drain() { s.draining.Store(true) }

// MarkReady releases the sticky startup gate.
func (s *Server) MarkReady() { s.ready.Store(true) }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.URL.Path == ReadinessPath {
		if s.draining.Load() || !s.ready.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)

			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok") //nolint:errcheck // best-effort readiness body

		return
	}

	if s.draining.Load() {
		http.Error(w, "agent shutting down", http.StatusServiceUnavailable)

		return
	}

	if !s.ready.Load() {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "agent starting up", http.StatusServiceUnavailable)

		return
	}

	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	requested, err := httprange.ParseExact(r.Header.Get("Range"))
	if err != nil {
		s.reject("range")
		http.Error(w, "invalid Range", http.StatusBadRequest)

		return
	}

	origin, err := OriginURLFromRequest(r, s.opts.URLPolicy)
	if err != nil {
		s.reject("origin_url")
		http.Error(w, "invalid origin URL", http.StatusBadRequest)

		return
	}

	body, total, err := s.local.OpenRange(r.Context(), origin.Digest, requested)
	if err == nil {
		s.writeResponse(w, body, total, "application/octet-stream", requested, "local", started)

		return
	}

	if errors.Is(err, httprange.ErrUnsatisfiable) && total >= 0 {
		w.Header().Set("Content-Range", httprange.UnsatisfiedContentRange(total))
		http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)

		return
	}

	var (
		notFound    *ifaces.ErrNotFound
		unavailable *ifaces.ErrUnavailable
	)
	if !errors.As(err, &notFound) && !errors.As(err, &unavailable) {
		s.opts.Logger.Warn("local range open failed",
			slog.String("digest", origin.Digest.String()),
			slog.Any("err", err),
		)
	}

	lookupCtx, cancel := context.WithTimeout(r.Context(), s.opts.PeerLookupTimeout)
	providers, lookupErr := s.discovery.FindProviders(lookupCtx, origin.Digest)

	cancel()

	if lookupErr == nil {
		attempts := 0
		for _, provider := range s.failures.filter(origin.Digest, providers, s.opts.SelfPeerID) {
			if attempts >= s.opts.MaxPeerAttempts {
				break
			}

			attempts++

			peerBody, peerTotal, contentType, peerErr := s.peer.FetchRangeFromPeer(r.Context(), provider.Addr, origin.Digest, requested)
			if peerErr != nil {
				s.failures.record(origin.Digest, provider, peerErr)
				continue
			}

			s.writeResponse(w, peerBody, peerTotal, contentType, requested, "peer", started)

			return
		}
	}

	body, total, contentType, err := s.origin.FetchRange(r.Context(), origin, requested)
	if err != nil {
		s.writeOriginError(w, err)
		s.observe("origin", "error", time.Since(started), 0)

		return
	}

	s.writeResponse(w, body, total, contentType, requested, "origin", started)
}

func (s *Server) writeResponse(w http.ResponseWriter, body io.ReadCloser, total int64, contentType string, requested httprange.Range, source string, started time.Time) {
	defer func() { _ = body.Close() }() //nolint:errcheck // best-effort close

	if s.opts.Metrics.OnInflight != nil {
		s.opts.Metrics.OnInflight(source, 1)
		defer s.opts.Metrics.OnInflight(source, -1)
	}

	if contentType == "" {
		contentType = "application/octet-stream"
	}

	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", requested.Start, requested.End, total))
	w.Header().Set("Content-Length", strconv.FormatInt(requested.Length(), 10))

	if s.opts.Metrics.OnFirstByte != nil {
		s.opts.Metrics.OnFirstByte(source, time.Since(started))
	}

	w.WriteHeader(http.StatusPartialContent)

	written, err := io.CopyN(w, body, requested.Length())
	if err != nil {
		s.opts.Logger.Debug("range response interrupted",
			slog.String("source", source),
			slog.String("digest", "redacted"),
			slog.Int64("written", written),
			slog.Any("err", err),
		)
		s.observe(source, "error", time.Since(started), written)

		return
	}

	s.observe(source, "success", time.Since(started), written)
}

func (s *Server) writeOriginError(w http.ResponseWriter, err error) {
	var statusErr *OriginStatusError
	if errors.As(err, &statusErr) {
		if statusErr.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.FormatInt(int64(statusErr.RetryAfter/time.Second), 10))
		}

		switch statusErr.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			http.Error(w, "origin authorization failed", statusErr.StatusCode)
		case http.StatusNotFound:
			http.Error(w, "origin content not found", http.StatusNotFound)
		case http.StatusTooManyRequests:
			http.Error(w, "origin rate limited", http.StatusTooManyRequests)
		default:
			http.Error(w, "origin unavailable", http.StatusBadGateway)
		}

		return
	}

	http.Error(w, "origin unavailable", http.StatusServiceUnavailable)
}

func (s *Server) reject(reason string) {
	if s.opts.Metrics.OnReject != nil {
		s.opts.Metrics.OnReject(reason)
	}
}

func (s *Server) observe(source, outcome string, duration time.Duration, bytes int64) {
	if s.opts.Metrics.OnRequest != nil {
		s.opts.Metrics.OnRequest(source, outcome, duration, bytes)
	}
}
