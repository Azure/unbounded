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

// RangeCache retains ranges fetched from peers or origin so the next reader -
// local or remote - does not have to leave the node for them.
type RangeCache interface {
	OpenRange(ctx context.Context, d digest.Digest, requested httprange.Range) (io.ReadCloser, int64, error)
	Put(d digest.Digest, total, start int64, r io.Reader) (int64, error)
}

// Tuning values rather than operator configuration: changing one requires a
// latency measurement, so they live in code with the behavior they affect.
const (
	defaultPeerLookupTimeout  = 250 * time.Millisecond
	defaultPeerAttemptTimeout = 10 * time.Second
	defaultMaxPeerAttempts    = 3
)

type Options struct {
	URLPolicy              URLPolicy
	ResolveCanonicalOrigin func(OriginURL) (OriginURL, bool)

	// PeerLookupTimeout, PeerAttemptTimeout, and MaxPeerAttempts use defaults when zero.
	PeerLookupTimeout  time.Duration
	PeerAttemptTimeout time.Duration
	MaxPeerAttempts    int

	SelfPeerID   ifaces.NodeID
	StartupGated bool
	Logger       *slog.Logger
	Metrics      MetricsHooks

	// Cache, when set, absorbs reads no larger than MaxCachedRangeBytes.
	// OverlayBD index reads are tiny and every node wants the same ones, so
	// caching them is what lets peers answer instead of origin.
	Cache               RangeCache
	MaxCachedRangeBytes int64
	// OnCached announces that this node can now serve ranges of d. Peers
	// discover range sources the same way they discover complete blobs, so
	// without this a cached range stays invisible to the swarm.
	OnCached func(d digest.Digest)
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

	if opts.PeerAttemptTimeout <= 0 {
		opts.PeerAttemptTimeout = defaultPeerAttemptTimeout
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

	if r.Context().Err() != nil {
		return
	}

	body, total, err := s.local.OpenRange(r.Context(), origin.Digest, requested)
	if err == nil {
		s.writeResponse(w, body, total, "application/octet-stream", requested, "local", started)

		return
	}

	if s.opts.Cache != nil {
		if cached, cachedTotal, cacheErr := s.opts.Cache.OpenRange(r.Context(), origin.Digest, requested); cacheErr == nil {
			s.writeResponse(w, cached, cachedTotal, "application/octet-stream", requested, "cache", started)

			return
		}
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

	if r.Context().Err() != nil {
		return
	}

	if lookupErr == nil {
		attempts := 0

		for _, provider := range s.failures.filter(origin.Digest, requested, providers, s.opts.SelfPeerID) {
			if r.Context().Err() != nil {
				return
			}

			if attempts >= s.opts.MaxPeerAttempts {
				break
			}

			attempts++

			peerCtx, peerCancel := s.peerAttemptContext(r.Context())

			peerBody, peerTotal, contentType, peerErr := s.peer.FetchRangeFromPeer(peerCtx, provider.Addr, origin.Digest, requested)
			if peerErr != nil {
				peerCancel()

				if r.Context().Err() != nil {
					return
				}

				s.failures.record(origin.Digest, requested, provider, peerErr)

				continue
			}
			defer peerCancel()

			s.serveMaybeCaching(w, r, origin.Digest, peerBody, peerTotal, contentType, requested, "peer", started)

			return
		}
	}

	if r.Context().Err() != nil {
		return
	}

	if origin.Canonical {
		resolved, ok := OriginURL{}, false
		if s.opts.ResolveCanonicalOrigin != nil {
			resolved, ok = s.opts.ResolveCanonicalOrigin(origin)
		}

		if !ok {
			http.Error(w, "canonical origin unavailable", http.StatusServiceUnavailable)

			return
		}

		origin = resolved
	}

	body, total, contentType, err := s.origin.FetchRange(r.Context(), origin, requested)
	if err != nil {
		s.writeOriginError(w, err)
		s.observe("origin", "error", time.Since(started), 0)

		return
	}

	s.serveMaybeCaching(w, r, origin.Digest, body, total, contentType, requested, "origin", started)
}

// peerAttemptContext bounds each peer read independently of bulk transfers and
// reserves at least half of the remaining caller deadline for subsequent sources.
func (s *Server) peerAttemptContext(parent context.Context) (context.Context, context.CancelFunc) {
	timeout := s.opts.PeerAttemptTimeout
	if deadline, ok := parent.Deadline(); ok {
		timeout = min(timeout, time.Until(deadline)/2)
	}

	return context.WithTimeout(parent, timeout)
}

// serveMaybeCaching stores a small remote read before serving it. The bytes go
// to disk first and are then read back, so a 1 GiB range never has to be held
// in memory to be cached.
func (s *Server) serveMaybeCaching(w http.ResponseWriter, r *http.Request, d digest.Digest, body io.ReadCloser, total int64, contentType string, requested httprange.Range, source string, started time.Time) {
	if s.opts.Cache == nil || s.opts.MaxCachedRangeBytes <= 0 || requested.Length() > s.opts.MaxCachedRangeBytes {
		s.writeResponse(w, body, total, contentType, requested, source, started)

		return
	}

	stored, err := s.opts.Cache.Put(d, total, requested.Start, io.LimitReader(body, requested.Length()))
	_ = body.Close() //nolint:errcheck // best-effort close

	if err != nil || stored != requested.Length() {
		s.opts.Logger.Debug("range cache store incomplete",
			slog.String("digest", d.String()),
			slog.Int64("stored", stored),
			slog.Any("err", err),
		)
		s.observe(source, "error", time.Since(started), stored)
		http.Error(w, "range cache store failed", http.StatusBadGateway)

		return
	}

	cached, cachedTotal, err := s.opts.Cache.OpenRange(r.Context(), d, requested)
	if err != nil {
		s.observe(source, "error", time.Since(started), 0)
		http.Error(w, "range cache read failed", http.StatusBadGateway)

		return
	}

	if s.opts.OnCached != nil {
		s.opts.OnCached(d)
	}

	s.writeResponse(w, cached, cachedTotal, contentType, requested, source, started)
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
