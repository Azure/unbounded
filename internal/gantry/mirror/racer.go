// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	gantryracer "github.com/Azure/unbounded/internal/gantry/racer"
	sdk "github.com/Azure/unbounded/pkg/racer"
)

// NewRacer builds a Racer-backend Server with an explicit registry contract.
// A nil backend sends local misses directly to the registry fallback.
func NewRacer(cfg *config.Config, store ifaces.LocalContentStore, registry gantryracer.Registry, backend *gantryracer.Backend, opts ...Option) *Server {
	limit := cfg.RacerMaxConcurrentTransfers
	if limit <= 0 {
		limit = 64
	}

	state := &racerState{
		backend:              backend,
		registry:             registry,
		admission:            make(chan struct{}, limit),
		manifestObservations: make(chan struct{}, 16),
	}
	// Install state before applying caller options, including Racer callbacks.
	opts = append([]Option{func(s *Server) { s.racer = state }}, opts...)

	return newServer(cfg, store, registry, opts...)
}

// WithRacerMetrics registers Racer forwarding and registry fallback callbacks.
func WithRacerMetrics(stream func(sdk.TransferStats, bool, error), fallback func()) Option {
	return func(s *Server) {
		if s.racer != nil {
			s.racer.onStream, s.racer.onFallback = stream, fallback
		}
	}
}

func (s *Server) serveRacer(w http.ResponseWriter, r *http.Request, ref ifaces.OriginRef, logger *slog.Logger) {
	select {
	case s.racer.admission <- struct{}{}:
		defer func() { <-s.racer.admission }()
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, "Racer transfer capacity exhausted", http.StatusServiceUnavailable)

		return
	}

	budget := s.cfg.PeerFetchTimeout
	if budget <= 0 {
		budget = 15 * time.Minute
	}

	transferCtx, transferCancel := context.WithTimeout(r.Context(), budget)
	defer transferCancel()

	r = r.WithContext(transferCtx)
	if r.ContentLength > 0 || len(r.TransferEncoding) != 0 {
		http.Error(w, "digest requests must not carry a body", http.StatusBadRequest)
		return
	}

	if r.Header.Get("Gantry-Mirrored") != "" {
		http.Error(w, "direct peer protocol is unavailable in Racer mode", http.StatusConflict)
		return
	}

	if s.racer.backend == nil {
		s.racerFallback(w, r, ref, logger)
		return
	}

	// HEAD is an availability probe. Prepare can wait for an entire cold page,
	// so it receives the complete configurable transfer budget instead.
	streamCtx, cancel := context.WithCancel(r.Context())
	defer cancel()

	metadataBudget := s.cfg.RacerMetadataTimeout
	if metadataBudget <= 0 {
		metadataBudget = 3 * time.Second
	}

	metadataCtx, metadataCancel := context.WithTimeout(streamCtx, metadataBudget)
	obj, err := s.racer.backend.Open(metadataCtx, ref)

	metadataCancel()

	if err != nil {
		if !writeRacerAuthError(w, err) {
			s.racerFallback(w, r, ref, logger)
		}

		return
	}

	meta := obj.Metadata()
	if meta.ETag != `"`+ref.Digest.Hex()+`"` {
		s.racer.backend.Quarantine(ref)
		s.racerFallback(w, r, ref, logger)

		return
	}

	if r.Method == http.MethodHead {
		racerHeaders(w.Header(), ref, meta, meta.Size)
		w.WriteHeader(http.StatusOK)

		return
	}
	// HTTP permits ignoring unsupported/multipart ranges and returning 200.
	// Single byte ranges receive exact 206 framing, including suffix ranges.
	offset, length, partial, invalid := mirrorRange(r, meta.Size, meta.ETag)
	if invalid {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", meta.Size))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)

		return
	}

	var stream *sdk.Stream
	if partial {
		stream, err = obj.ReadRange(streamCtx, offset, length)
	} else {
		stream, err = obj.Stream(streamCtx)
	}

	if err != nil {
		s.racerFallback(w, r, ref, logger)
		return
	}

	if err = stream.Prepare(); err != nil {
		_ = stream.Close() //nolint:errcheck // Preparation failed before forwarding takes ownership.

		if !writeRacerAuthError(w, err) {
			s.racerFallback(w, r, ref, logger)
		}

		return
	}

	status := http.StatusOK
	if partial {
		status = http.StatusPartialContent
	}

	// Keep forwarding headers private until hijack succeeds, so a failed
	// hijack leaves the ResponseWriter available for a clean registry fallback.
	header := w.Header().Clone()
	racerHeaders(header, ref, meta, length)
	header.Set("Connection", "close")

	if partial {
		header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, meta.Size))
	}

	result := s.racer.forward(streamCtx, cancel, w, stream, status, header)
	if result.ownership == racerFallbackAllowed {
		s.racerFallback(w, r, ref, logger)
		return
	}

	if s.racer.onStream != nil {
		s.racer.onStream(stream.Stats(), partial, result.err)
	}

	s.fireMirrorBytesServed(ref.Kind, "racer", result.written)

	if result.err != nil {
		logger.Debug("mirror: Racer stream aborted", slog.Any("err", result.err), slog.Int64("written", result.written))

		return
	}
	// Full response completion records forwarding, not a containerd commit.
	if !partial {
		s.fireMirrorResponseCompleted(ref.Digest, ref.Kind, "racer")
		s.fireLiveStreamCompleted(ref.Digest)
		s.firePrefetch(r.Context(), ref.Kind, ref.Registry, ref.Repository, ref.Digest)
	}
}

func racerHeaders(h http.Header, ref ifaces.OriginRef, meta sdk.Metadata, length int64) {
	h.Set("Docker-Content-Digest", ref.Digest.String())
	h.Set("Content-Length", strconv.FormatInt(length, 10))
	h.Set("ETag", meta.ETag)
	h.Set("Accept-Ranges", "bytes")

	h["Content-Type"] = nil
	if meta.ContentType != "" {
		h.Set("Content-Type", meta.ContentType)
	}
}

func writeRacerAuthError(w http.ResponseWriter, err error) bool {
	var status *sdk.HTTPError
	if !errors.As(err, &status) || (status.StatusCode != 401 && status.StatusCode != 403) {
		return false
	}

	if status.WWWAuthenticate != "" {
		w.Header().Set("WWW-Authenticate", status.WWWAuthenticate)
	}

	if status.RetryAfter != "" {
		w.Header().Set("Retry-After", status.RetryAfter)
	}

	http.Error(w, "registry authorization rejected", status.StatusCode)

	return true
}

func mirrorRange(r *http.Request, size int64, etag string) (offset, length int64, partial, invalid bool) {
	value := r.Header.Get("Range")
	if r.Header.Get("If-Range") != "" && r.Header.Get("If-Range") != etag {
		return 0, size, false, false
	}

	if !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") {
		return 0, size, false, false
	}

	left, right, ok := strings.Cut(strings.TrimPrefix(value, "bytes="), "-")
	if !ok || size == 0 {
		return 0, 0, false, true
	}

	decimal := func(value string) (int64, error) {
		if value == "" || strings.IndexFunc(value, func(c rune) bool { return c < '0' || c > '9' }) >= 0 {
			return 0, errors.New("invalid range")
		}

		return strconv.ParseInt(value, 10, 64)
	}
	if left == "" {
		n, err := decimal(right)
		if err != nil || n == 0 {
			return 0, 0, false, true
		}

		n = min(n, size)

		return size - n, n, true, false
	}

	start, err := decimal(left)
	if err != nil || start >= size {
		return 0, 0, false, true
	}

	end := size - 1
	if right != "" {
		end, err = decimal(right)
		if err != nil || end < start {
			return 0, 0, false, true
		}

		end = min(end, size-1)
	}

	return start, end - start + 1, true, false
}

// racerFallback uses the original delegated context. Range is deliberately
// ignored with a full 200 response. Like raw Racer forwarding, it leaves OCI
// digest verification to containerd while checking transport and declared size.
func (s *Server) racerFallback(w http.ResponseWriter, r *http.Request, ref ifaces.OriginRef, logger *slog.Logger) {
	// net/http's header timeout does not bound response writes. Cancellation
	// must also interrupt a Write blocked on a downstream that stopped reading.
	controller := http.NewResponseController(w)

	deadline, _ := r.Context().Deadline()
	if err := controller.SetWriteDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		panic(http.ErrAbortHandler)
	}

	interrupted := make(chan struct{})
	stop := context.AfterFunc(r.Context(), func() {
		_ = controller.SetWriteDeadline(time.Now()) //nolint:errcheck // Best-effort interruption during cancellation.

		close(interrupted)
	})

	defer func() {
		if !stop() {
			<-interrupted
		}

		// Leave the deadline in place for net/http's final header/chunk flush.
		// The server resets it after finishing this response, before keep-alive.
	}()

	if s.racer.onFallback != nil {
		s.racer.onFallback()
	}

	if r.Method == http.MethodHead {
		size, ct, err := s.racer.registry.Head(r.Context(), ref)
		if err != nil {
			writeOriginError(w, err, logger)
			return
		}

		h := w.Header()
		h.Set("Docker-Content-Digest", ref.Digest.String())

		h["Content-Type"] = nil
		if ct != "" {
			h.Set("Content-Type", ct)
		}

		if size >= 0 {
			h.Set("Content-Length", strconv.FormatInt(size, 10))
		}

		w.WriteHeader(http.StatusOK)

		return
	}

	// HTTP/1.0 close-delimited bodies cannot distinguish an abort from success.
	if !r.ProtoAtLeast(1, 1) {
		http.Error(w, "registry streaming requires HTTP/1.1 or later", http.StatusHTTPVersionNotSupported)
		return
	}

	s.fireOriginStreamStarted(ref.Kind)

	body, size, contentType, err := s.racer.registry.PullWithMetadata(r.Context(), ref)
	if err != nil {
		s.fireOriginStreamFailed(ref.Kind)
		writeOriginError(w, err, logger)

		return
	}

	defer body.Close() //nolint:errcheck // Upstream response cleanup.

	w.Header().Set("Docker-Content-Digest", ref.Digest.String())

	w.Header()["Content-Type"] = nil
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}

	// Do not expose the declared length as downstream framing: reaching it
	// cannot signal success before we check EOF for overruns or late errors.
	// Flushing headers also prevents net/http from synthesizing Content-Length
	// for small/empty bodies. HTTP/1.1 uses chunks; HTTP/2 uses stream termination.
	w.Header().Del("Content-Length")
	w.WriteHeader(http.StatusOK)

	err = controller.Flush()

	var written int64
	if err == nil {
		written, err = io.Copy(w, body)
	}

	if err == nil && size >= 0 && written != size {
		err = fmt.Errorf("origin size mismatch: declared %d, forwarded %d", size, written)
	}

	if err == nil {
		err = r.Context().Err()
	}

	if err == nil {
		err = controller.Flush()
	}

	s.fireMirrorBytesServed(ref.Kind, "origin", written)

	if err != nil {
		s.fireOriginStreamFailed(ref.Kind)
		// Abort chunked framing too: net/http must not emit the final chunk.
		panic(http.ErrAbortHandler)
	}

	// Completion reports forwarding only, including same-length corrupt bytes.
	// Containerd's later digest-checked commit is observed separately.
	s.fireOriginStreamCompleted(ref.Kind)
	s.fireMirrorResponseCompleted(ref.Digest, ref.Kind, "origin")
	s.fireLiveStreamCompleted(ref.Digest)
	s.firePrefetch(r.Context(), ref.Kind, ref.Registry, ref.Repository, ref.Digest)
}
