// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package transfer is the peer-facing OCI endpoint other Gantry agents pull
// from. It binds `:5001` over HTTP/2 cleartext (h2c) because NetworkPolicy
// restricts the port to inter-node traffic and TLS termination would
// duplicate the libp2p Noise security that Gantry already does for
// coordination RPCs.
//
// Design (architecture.md the API contract):
//
// - Same OCI URL shape as the mirror: `/v2/<repo>/blobs/<digest>` and
// `/v2/<repo>/manifests/<digest>`. Tag-shaped manifest requests at this
// endpoint return **404 unconditionally** - peers must already know the
// digest (the tag-resolution path runs through the mirror, not
// here).
// - Requires `Gantry-Mirrored: 1` request header. Without the header the
// server returns 400. With the header, the response semantics are:
// **serve from the local store or return 404**. No DHT lookup, no
// HRW probe, no `please_pull`, no origin contact. The header is the
// loop-breaker that prevents two agents from recursing into each
// other's miss paths.
// - `Range: bytes=N-M` returns `206 Partial Content` with the correct
// `Content-Range`. v1 callers always fetch whole blobs, but the
// contract is preserved for v2 striping.
// - Metric `p2p_peer_serve_total` is bumped per served body - not
// `p2p_cache_hit_total`, so cluster scrapes distinguish containerd-
// facing hits from peer-facing serves.
package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c" //nolint:staticcheck // h2c deliberate

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/oci"
)

// MirroredHeader is the OCI-extension header peers MUST include on every
// transfer-endpoint request. It is the loop-breaker described in the design doc.
const MirroredHeader = "Gantry-Mirrored"

// Server serves peer-fetch requests from the local content store.
type Server struct {
	store     ifaces.LocalContentStore
	describer Describer
	logger    *slog.Logger
	metrics   metricsHooks
	// serveSem, when non-nil, caps concurrent blob-body serves. A full
	// channel means the server is at capacity and further blob GETs are
	// shed with 429 so the requester re-discovers another provider.
	serveSem chan struct{}
}

// Describer is an optional capability that callers (typically the
// containerdstore.Store) may implement to surface cached media-type
// metadata for a digest. The transfer endpoint uses it to set a
// correct manifest Content-Type instead of falling back to
// application/octet-stream - per "Descriptor and media-type
// handling": "Do not serve manifest digests with a generic content
// type unless existing containerd/client behavior proves it is safe."
//
// An empty return value means "unknown"; callers MUST fall back to
// the kind-based default rather than treating it as an error.
type Describer interface {
	LookupMediaType(d digest.Digest) string
}

type metricsHooks struct {
	onPeerServe      func()
	onPeerMiss       func()
	onPeerServeBytes func(kind string, bytes int64)
}

// Option configures a Server.
type Option func(*Server)

// WithLogger plumbs a structured logger.
func WithLogger(l *slog.Logger) Option {
	return func(s *Server) {
		if l != nil {
			s.logger = l.With(slog.String("subsystem", "transfer"))
		}
	}
}

// WithMetrics registers metric callbacks.
func WithMetrics(onPeerServe, onPeerMiss func()) Option {
	return func(s *Server) {
		s.metrics = metricsHooks{onPeerServe: onPeerServe, onPeerMiss: onPeerMiss}
	}
}

// WithByteMetrics registers a callback for bytes actually transmitted to peer
// agents. HEAD requests emit no bytes, and Range requests count only the range
// body sent on the wire.
func WithByteMetrics(onPeerServeBytes func(kind string, bytes int64)) Option {
	return func(s *Server) {
		s.metrics.onPeerServeBytes = onPeerServeBytes
	}
}

// WithDescriber registers an optional media-type lookup for cached
// digests. Wire this with the containerdstore.Store so manifest
// responses carry the correct OCI/Docker media type instead of
// application/octet-stream.
func WithDescriber(d Describer) Option {
	return func(s *Server) {
		s.describer = d
	}
}

// WithMaxConcurrentServes caps concurrent peer blob-body serves. When the cap
// is reached, further blob GETs receive 429 Too Many Requests with a
// Retry-After hint so the requester re-selects another provider instead of
// queueing behind a saturated seed. That load-shedding is what lets the first
// finishers complete early and seed the swarm (the cascade). n <= 0 means
// unlimited. HEAD requests and manifest serves are never capped: they are
// cheap and are needed for discovery and verification.
func WithMaxConcurrentServes(n int) Option {
	return func(s *Server) {
		if n > 0 {
			s.serveSem = make(chan struct{}, n)
		}
	}
}

// New builds a Server bound to the given local content store.
func New(store ifaces.LocalContentStore, opts ...Option) *Server {
	s := &Server{
		store:  store,
		logger: slog.Default().With(slog.String("subsystem", "transfer")),
	}
	for _, opt := range opts {
		opt(s)
	}

	return s
}

// Handler returns the HTTP handler. Callers wrap it with h2c via
// ListenAndServe or use it directly (e.g., httptest in unit tests, where
// HTTP/1.1 is sufficient because no client uses Range over multiplexed
// streams).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/v2/", s.handleV2)
	mux.HandleFunc("/v2", s.handleV2)

	return mux
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok") //nolint:errcheck // best-effort write
}

func (s *Server) handleV2(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")

	path := r.URL.Path
	if path == "/v2/" || path == "/v2" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`) //nolint:errcheck // best-effort write

		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	if r.Header.Get(MirroredHeader) != "1" {
		http.Error(w, "missing Gantry-Mirrored header", http.StatusBadRequest)
		return
	}

	_, kind, ref, ok := oci.ParseV2Path(path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	if !strings.HasPrefix(ref, "sha256:") {
		// Tag at peer endpoint -> 404 unconditionally (the design doc).
		http.NotFound(w, r)
		return
	}

	d, err := digest.Parse(ref)
	if err != nil {
		http.Error(w, "invalid digest", http.StatusBadRequest)
		return
	}

	s.serveDigest(w, r, d, kind)
}

func (s *Server) serveDigest(w http.ResponseWriter, r *http.Request, d digest.Digest, kind ifaces.OriginRefKind) {
	// Shed load on a saturated seed so the requester re-discovers another
	// provider (the cascade) instead of queueing behind us. Only full
	// blob-body GETs are capped: HEADs are cheap and manifests are small and
	// needed for discovery/verification.
	if r.Method == http.MethodGet && kind == ifaces.KindBlob {
		release, ok := s.tryAcquireServe()
		if !ok {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "peer serving at capacity", http.StatusTooManyRequests)

			return
		}

		defer release()
	}

	rc, size, err := s.openBlob(r.Context(), d)
	if err != nil {
		var enf *ifaces.ErrNotFound
		if errors.As(err, &enf) {
			if s.metrics.onPeerMiss != nil {
				s.metrics.onPeerMiss()
			}

			http.NotFound(w, r)

			return
		}

		var eun *ifaces.ErrUnavailable
		if errors.As(err, &eun) {
			// + containerd unavailable is
			// NOT a content-absence signal. Return 503 so the
			// peer treats this node as temporarily down (and tries
			// the next provider / falls through to cold-start)
			// instead of caching a false "this digest is missing"
			// belief in its stale-provider map.
			s.logger.Warn("transfer: backend unavailable",
				slog.String("digest", d.String()),
				slog.Any("err", err),
			)
			http.Error(w, "storage unavailable", http.StatusServiceUnavailable)

			return
		}

		s.logger.Warn("transfer: local store open error",
			slog.String("digest", d.String()),
			slog.Any("err", err),
		)
		http.Error(w, "local store error", http.StatusInternalServerError)

		return
	}

	defer func() { _ = rc.Close() }() //nolint:errcheck // best-effort close

	w.Header().Set("Docker-Content-Digest", d.String())
	w.Header().Set("Content-Type", contentTypeFor(kind, s.lookupMediaType(d)))
	w.Header().Set("Accept-Ranges", "bytes")

	rng := r.Header.Get("Range")
	if rng == "" {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)

		if r.Method == http.MethodHead {
			s.bumpServe()
			return
		}

		written, err := io.Copy(w, rc)
		s.bumpServeBytes(kind, written)

		if err != nil {
			s.logger.Debug("transfer: copy failed", slog.Any("err", err))
		}

		s.bumpServe()

		return
	}

	start, end, ok := parseSingleRange(rng, size)
	if !ok {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		http.Error(w, "invalid Range", http.StatusRequestedRangeNotSatisfiable)

		return
	}

	length := end - start + 1
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(http.StatusPartialContent)

	if r.Method == http.MethodHead {
		s.bumpServe()
		return
	}

	rs, isSeeker := rc.(io.ReadSeeker)
	if !isSeeker {
		// Fall back to discarding the unwanted prefix.
		if _, err := io.CopyN(io.Discard, rc, start); err != nil {
			s.logger.Debug("transfer: discard prefix failed", slog.Any("err", err))
			return
		}
	} else if _, err := rs.Seek(start, io.SeekStart); err != nil {
		s.logger.Debug("transfer: seek failed", slog.Any("err", err))
		return
	}

	written, err := io.CopyN(w, rc, length)
	s.bumpServeBytes(kind, written)

	if err != nil {
		s.logger.Debug("transfer: range copy failed", slog.Any("err", err))
	}

	s.bumpServe()
}

func (s *Server) bumpServe() {
	if s.metrics.onPeerServe != nil {
		s.metrics.onPeerServe()
	}
}

func (s *Server) bumpServeBytes(kind ifaces.OriginRefKind, bytes int64) {
	if bytes <= 0 || s.metrics.onPeerServeBytes == nil {
		return
	}

	s.metrics.onPeerServeBytes(kind.MetricLabel(), bytes)
}

// tryAcquireServe reserves a serve slot without blocking. The returned release
// func MUST be called when the serve completes. ok is false when the server is
// already at its configured serve cap; callers should shed the request (429)
// rather than block. When no cap is configured (serveSem nil), it always
// succeeds with a no-op release.
func (s *Server) tryAcquireServe() (release func(), ok bool) {
	if s.serveSem == nil {
		return func() {}, true
	}

	select {
	case s.serveSem <- struct{}{}:
		return func() { <-s.serveSem }, true
	default:
		return nil, false
	}
}

// lookupMediaType returns the describer's cached mediaType for d, or
// "" if no describer is wired or the index has no entry.
func (s *Server) lookupMediaType(d digest.Digest) string {
	if s.describer == nil {
		return ""
	}

	return s.describer.LookupMediaType(d)
}

// contentTypeFor selects the response Content-Type for a digest-keyed
// transfer response. The descriptor-index hint wins; otherwise we
// fall back to a kind-appropriate default. Blob digests can legitimately
// carry either layer bytes or a manifest body (origin sometimes serves
// manifests under /blobs/<digest>), but at the peer transfer
// boundary the requester drove the URL path so kind is authoritative.
func contentTypeFor(kind ifaces.OriginRefKind, hint string) string {
	if hint != "" {
		return hint
	}

	if kind == ifaces.KindManifest {
		// Safe OCI manifest default - containerd's CRI plugin
		// dispatches on the schemaVersion/mediaType inside the
		// body, not the wire Content-Type. See mirror.go
		// writeBlobHeadersWithPrefix for the same rationale on
		// the local-pull side.
		return "application/vnd.oci.image.manifest.v1+json"
	}

	return "application/octet-stream"
}

// openBlob reads d from the local content store. ErrNotFound bubbles
// up unchanged so serveDigest returns 404; ErrUnavailable bubbles up
// so serveDigest returns 503. Any other error is returned as-is.
//
// Historical note: an earlier iteration of this server consulted an
// optional SecondaryBlobSource on local-store miss. In containerd-only
// mode the primary store IS the containerd content store, so the
// secondary hop was a redundant lookup of the same backend and has
// been removed (containerdstore.Store.Has/Open is now the single
// local-availability truth).
func (s *Server) openBlob(ctx context.Context, d digest.Digest) (io.ReadCloser, int64, error) {
	return s.store.Open(ctx, d)
}

// parseSingleRange parses an RFC 7233 single-range header like
// "bytes=0-499" or "bytes=500-" against a known total size. Multi-range
// requests are rejected (v1 callers don't use them).
func parseSingleRange(h string, size int64) (start, end int64, ok bool) {
	const prefix = "bytes="
	if !strings.HasPrefix(h, prefix) {
		return 0, 0, false
	}

	spec := h[len(prefix):]
	if strings.Contains(spec, ",") {
		return 0, 0, false
	}

	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, false
	}

	startStr, endStr := spec[:dash], spec[dash+1:]
	if startStr == "" {
		// Suffix: bytes=-N
		n, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil || n <= 0 || n > size {
			return 0, 0, false
		}

		return size - n, size - 1, true
	}

	s, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil || s < 0 || s >= size {
		return 0, 0, false
	}

	if endStr == "" {
		return s, size - 1, true
	}

	e, err := strconv.ParseInt(endStr, 10, 64)
	if err != nil || e < s || e >= size {
		return 0, 0, false
	}

	return s, e, true
}

// ListenAndServe runs the transfer server with h2c support on addr.
// Returns a function that gracefully shuts the server down.
func (s *Server) ListenAndServe(addr string) (func(context.Context) error, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("transfer: listen %s: %w", addr, err)
	}

	h2s := &http2.Server{}
	handler := h2c.NewHandler(s.Handler(), h2s) //nolint:staticcheck // h2c deliberate

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	if err := http2.ConfigureServer(srv, h2s); err != nil {
		ln.Close() //nolint:errcheck // closing on config error
		return nil, fmt.Errorf("transfer: configure h2: %w", err)
	}

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Default().Error("transfer: serve error", slog.Any("err", err))
		}
	}()

	return func(ctx context.Context) error {
		return srv.Shutdown(ctx)
	}, nil
}
