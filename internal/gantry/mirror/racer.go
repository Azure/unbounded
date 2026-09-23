// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/unbounded/internal/gantry/ifaces"
	gantryracer "github.com/Azure/unbounded/internal/gantry/racer"
	sdk "github.com/Azure/unbounded/pkg/racer"
)

// WithRacer installs the explicit post-local-miss backend. Partial streams are
// version pinned but cannot establish the complete OCI SHA-256 digest.
func WithRacer(backend *gantryracer.Backend, stream func(sdk.TransferStats, bool, error), fallback func()) Option {
	return func(s *Server) {
		s.racer, s.onRacerStream, s.onRacerFallback = backend, stream, fallback
		s.manifestObservations = make(chan struct{}, 16)
	}
}

func (s *Server) serveRacer(w http.ResponseWriter, r *http.Request, ref ifaces.OriginRef, logger *slog.Logger) {
	if r.ContentLength > 0 || len(r.TransferEncoding) != 0 {
		http.Error(w, "digest requests must not carry a body", http.StatusBadRequest)
		return
	}

	if r.Header.Get("Gantry-Mirrored") != "" {
		http.Error(w, "direct peer protocol is unavailable in Racer mode", http.StatusConflict)
		return
	}

	if s.racer == nil {
		s.racerFallback(w, r, ref, logger)
		return
	}

	// Bound availability/header probes independently of the full transfer
	// budget. Stop the timer after Prepare without canceling the stream context.
	streamCtx, cancel := context.WithCancel(r.Context())
	defer cancel()

	probeTimer := time.AfterFunc(3*time.Second, cancel)
	defer probeTimer.Stop()

	obj, err := s.racer.Open(streamCtx, ref)
	if err != nil {
		if !writeRacerAuthError(w, err) {
			s.racerFallback(w, r, ref, logger)
		}

		return
	}

	meta := obj.Metadata()
	if meta.ETag != `"`+ref.Digest.Hex()+`"` {
		s.racer.Quarantine(ref)
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
		var expected [sha256.Size]byte

		_, err = hex.Decode(expected[:], []byte(ref.Digest.Hex()))
		if err == nil {
			stream, err = obj.StreamVerified(streamCtx, expected)
		}
	}

	if err != nil {
		s.racerFallback(w, r, ref, logger)
		return
	}

	defer stream.Close() //nolint:errcheck // Abandoned streams must release sockets.

	if err = stream.Prepare(); err != nil {
		if errors.Is(err, sdk.ErrDigestMismatch) {
			s.racer.Quarantine(ref)
		}

		if !writeRacerAuthError(w, err) {
			s.racerFallback(w, r, ref, logger)
		}

		return
	}

	if !probeTimer.Stop() || streamCtx.Err() != nil {
		s.racerFallback(w, r, ref, logger)
		return
	}
	// Hijacking is essential: passing ResponseWriter or the buffered writer to
	// WriteTo hides the TCP socket and turns forwarding into a userspace copy.
	// One response per connection is deliberate; Connection: close gives exact
	// framing without maintaining a second HTTP keep-alive request parser.
	conn, buffered, err := http.NewResponseController(w).Hijack()
	if err != nil {
		s.racerFallback(w, r, ref, logger)
		return
	}
	defer conn.Close() //nolint:errcheck // One response per hijacked connection.

	s.racerMu.Lock()
	if s.draining.Load() {
		s.racerMu.Unlock()
		return
	}

	if s.racerConnections == nil {
		s.racerConnections = make(map[net.Conn]context.CancelFunc)
	}

	s.racerConnections[conn] = cancel
	s.racerMu.Unlock()

	defer func() { s.racerMu.Lock(); delete(s.racerConnections, conn); s.racerMu.Unlock() }()

	deadline := time.Now().Add(s.cfg.PeerFetchTimeout)
	if s.cfg.PeerFetchTimeout <= 0 {
		deadline = time.Now().Add(15 * time.Minute)
	}

	if err := conn.SetDeadline(deadline); err != nil {
		return
	}

	status := http.StatusOK
	if partial {
		status = http.StatusPartialContent
	}

	racerHeaders(w.Header(), ref, meta, length)
	w.Header().Set("Connection", "close")

	if partial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, meta.Size))
	}

	_, err = fmt.Fprintf(buffered, "HTTP/1.1 %d %s\r\n", status, http.StatusText(status))
	if err == nil {
		err = w.Header().Write(buffered)
	}

	if err == nil {
		_, err = buffered.WriteString("\r\n")
	}

	if err == nil {
		err = buffered.Flush()
	}

	var written int64
	if err == nil {
		written, err = stream.WriteTo(conn)
	}

	if s.onRacerStream != nil {
		s.onRacerStream(stream.Stats(), partial, err)
	}

	s.fireMirrorBytesServed(ref.Kind, "racer", written)

	if err != nil {
		if errors.Is(err, sdk.ErrDigestMismatch) {
			s.racer.Quarantine(ref)
		}

		logger.Debug("mirror: Racer stream aborted", slog.Any("err", err), slog.Int64("written", written))

		return
	}
	// A partial response is not a verified object or a containerd commit.
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
// ignored with a full 200 response, which remains fully SHA-256 verified even
// when the upstream cannot serve ranges or reports an unknown size.
func (s *Server) racerFallback(w http.ResponseWriter, r *http.Request, ref ifaces.OriginRef, logger *slog.Logger) {
	if s.onRacerFallback != nil {
		s.onRacerFallback()
	}

	if r.Method == http.MethodHead {
		size, ct, err := s.origin.Head(r.Context(), ref)
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

	s.fireOriginStreamStarted(ref.Kind)

	body, size, err := s.origin.Pull(r.Context(), ref)
	if err != nil {
		s.fireOriginStreamFailed(ref.Kind)
		writeOriginError(w, err, logger)

		return
	}
	defer body.Close() //nolint:errcheck // Upstream response cleanup.

	br := bufio.NewReader(body)
	prefix, _ := br.Peek(512) //nolint:errcheck // Short prefixes are valid; copy checks stream errors.
	// Empty objects must be verified before committing headers.
	if len(prefix) == 0 && ref.Digest.Hex() != fmt.Sprintf("%x", sha256.Sum256(nil)) {
		s.fireOriginStreamFailed(ref.Kind)
		http.Error(w, "origin digest mismatch", http.StatusBadGateway)

		return
	}

	writeBlobHeadersWithPrefix(w, ref.Digest, size, ref.Kind, prefix)

	hash := sha256.New()
	hold := &lastByteWriter{dst: w}

	written, err := io.Copy(io.MultiWriter(hold, hash), br)
	if err == nil && size >= 0 && written != size {
		err = io.ErrUnexpectedEOF
	}

	if err == nil && hex.EncodeToString(hash.Sum(nil)) != ref.Digest.Hex() {
		err = sdk.ErrDigestMismatch
	}

	if err == nil {
		err = hold.finish()
	}

	s.fireMirrorBytesServed(ref.Kind, "origin", hold.written)

	if err != nil {
		s.fireOriginStreamFailed(ref.Kind)
		// Abort chunked framing too: net/http must not emit the final chunk.
		panic(http.ErrAbortHandler)
	}

	s.fireOriginStreamCompleted(ref.Kind)
	s.fireMirrorResponseCompleted(ref.Digest, ref.Kind, "origin")
	s.fireLiveStreamCompleted(ref.Digest)
	s.firePrefetch(r.Context(), ref.Kind, ref.Registry, ref.Repository, ref.Digest)
}

type lastByteWriter struct {
	dst     io.Writer
	last    byte
	has     bool
	written int64
}

func (w *lastByteWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	if w.has {
		n, err := w.dst.Write([]byte{w.last})
		w.written += int64(n)

		if err != nil {
			return 0, err
		}
	}

	n, err := w.dst.Write(p[:len(p)-1])

	w.written += int64(n)
	if err != nil {
		return n, err
	}

	if n != len(p)-1 {
		return n, io.ErrShortWrite
	}

	w.last, w.has = p[len(p)-1], true

	return len(p), nil
}

func (w *lastByteWriter) finish() error {
	if !w.has {
		return nil
	}

	n, err := w.dst.Write([]byte{w.last})

	w.written += int64(n)
	if err == nil && n != 1 {
		return io.ErrShortWrite
	}

	return err
}
