// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/manifest"
)

// sourceCachemesh labels bytes served through the optional BlobFetcher.
const sourceCachemesh = "cachemesh"

// BlobFetcher is an optional content source consulted after a local-store
// miss for a digest GET (blobs and manifests), and for open-ended Range
// requests, before the peer and origin paths. FetchBlob serves the object
// from ref.Offset to its end.
//
// started == false means nothing was written and the mirror continues with
// its default path. Otherwise FetchBlob called start exactly once with the
// object size and upstream content type (the mirror writes the status line
// and headers there) and streamed the body; a non-nil error then means the
// body is truncated and the mirror aborts the response, so containerd
// resumes with a Range request.
type BlobFetcher interface {
	FetchBlob(ctx context.Context, w io.Writer, ref ifaces.OriginRef, start func(total int64, contentType string)) (started bool, written int64, err error)
}

// TagResolver is an optional source for manifest-by-tag requests, which the
// mirror otherwise answers with 503 so containerd falls back to the origin.
// It is consulted only for requests without credentials, against registries
// that do not require authentication.
type TagResolver interface {
	ResolveTag(ctx context.Context, registry, repo, tag string) (body []byte, contentType string, d digest.Digest, err error)
}

// WithBlobFetcher installs an optional BlobFetcher. Nil leaves the mirror
// unchanged.
func WithBlobFetcher(f BlobFetcher) Option {
	return func(s *Server) { s.blobFetcher = f }
}

// WithTagResolver installs an optional TagResolver. Nil leaves the mirror
// unchanged.
func WithTagResolver(r TagResolver) Option {
	return func(s *Server) { s.tagResolver = r }
}

// serveFromFetcher serves a digest GET through the BlobFetcher. It returns
// false when there is no fetcher or the fetcher declined; nothing has been
// written then.
func (s *Server) serveFromFetcher(ctx context.Context, w http.ResponseWriter, d digest.Digest, kind ifaces.OriginRefKind, upstream, repo string, offset int64, logger *slog.Logger) bool {
	if s.blobFetcher == nil {
		return false
	}

	ref := ifaces.OriginRef{Registry: upstream, Repository: repo, Digest: d, Kind: kind, Offset: offset}

	// The local content store keeps no media type, so a peer can serve a
	// manifest for a /blobs/ request (or an index under /manifests/) with
	// an empty type. Parity with the other paths: hold back the headers
	// until the first body bytes are seen and sniff them. Ranges and
	// typed responses stream straight through.
	var sw *sniffWriter

	out := io.Writer(w)
	if offset == 0 && (kind == ifaces.KindBlob || kind == ifaces.KindManifest) {
		sw = &sniffWriter{w: w}
		out = sw
	}

	started, written, err := s.blobFetcher.FetchBlob(ctx, out, ref, func(total int64, contentType string) {
		// Keep any known type whatever the requested kind: the origin can
		// serve a /blobs/ request from /manifests/ and report its media type.
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}

		if offset > 0 {
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, total-1, total))
			w.Header().Set("Content-Length", strconv.FormatInt(total-offset, 10))
			writeBlobHeaders(w, d, -1, kind)
			w.WriteHeader(http.StatusPartialContent)

			return
		}

		if sw != nil && contentType == "" {
			sw.deferHeaders(func(prefix []byte) {
				writeBlobHeadersWithPrefix(w, d, total, kind, prefix)
				w.WriteHeader(http.StatusOK)
			})

			return
		}

		writeBlobHeaders(w, d, total, kind)
		w.WriteHeader(http.StatusOK)
	})
	if started && sw != nil {
		if ferr := sw.flush(); err == nil {
			err = ferr
		}
	}

	if !started {
		return false
	}

	s.fireMirrorBytesServed(ctx, kind, sourceCachemesh, written)

	if err != nil {
		logger.Debug("mirror: cachemesh stream failed",
			slog.Int64("offset", offset),
			slog.Int64("written", written),
			slog.Any("err", err),
		)
		// The status line is out and the body is short: abort so
		// containerd sees a broken response and retries with Range.
		panic(http.ErrAbortHandler)
	}

	s.fireMirrorResponseCompleted(d, kind, sourceCachemesh)

	if s.liveStreamThrough {
		s.fireLiveStreamCompleted(d)
	}

	s.firePrefetch(ctx, kind, upstream, repo, d)

	return true
}

// serveTag answers a manifest-by-tag request through the TagResolver. It
// returns false, having written nothing, when there is no resolver, the
// request carries credentials, the registry requires authentication, or
// the resolver fails. The challenge check is only a cheap pre-filter: it
// probes the registry root, not the repository. The resolver's owner fetches
// the tag anonymously, which is what keeps private repositories out.
func (s *Server) serveTag(w http.ResponseWriter, r *http.Request, upstream, repo, tag, authorization string) bool {
	if s.tagResolver == nil || authorization != "" {
		return false
	}

	ctx := r.Context()

	if s.auth != nil {
		challengeCtx, cancel := context.WithTimeout(ctx, authenticationChallengeTimeout)
		_, required, err := s.auth.AuthenticationChallenge(challengeCtx, upstream)

		cancel()

		if err != nil || required {
			return false
		}
	}

	body, contentType, d, err := s.tagResolver.ResolveTag(ctx, upstream, repo, tag)
	if err != nil {
		s.logger.Debug("mirror: cachemesh tag resolution declined",
			slog.String("registry", upstream),
			slog.String("repo", repo),
			slog.String("tag", tag),
			slog.Any("err", err),
		)

		return false
	}

	if contentType == "" {
		contentType = manifest.DetectContentType(body)
	}

	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}

	writeBlobHeaders(w, d, int64(len(body)), ifaces.KindManifest)
	w.WriteHeader(http.StatusOK)

	if r.Method == http.MethodHead {
		return true
	}

	n, _ := w.Write(body) //nolint:errcheck // containerd detects a short body
	s.fireMirrorBytesServed(ctx, ifaces.KindManifest, sourceCachemesh, int64(n))

	return true
}

// sniffWriter passes writes through unless headers have been deferred, in
// which case it buffers the body until it can be classified, then writes the
// headers from that prefix and flushes. A body whose first non-whitespace
// byte is not '{' cannot be a manifest and is released at once, so layers
// keep streaming. A JSON object is buffered up to the detector's own limit
// (or the end of the stream): an index may carry large annotations before
// its top-level mediaType or manifests field, and a shorter prefix would be
// misclassified as a single manifest.
type sniffWriter struct {
	w       io.Writer
	headers func(prefix []byte)
	buf     []byte
	lead    int // leading whitespace bytes of buf already scanned
}

func (s *sniffWriter) deferHeaders(headers func(prefix []byte)) {
	s.headers = headers
}

func (s *sniffWriter) Write(p []byte) (int, error) {
	if s.headers == nil {
		return s.w.Write(p)
	}

	s.buf = append(s.buf, p...)

	// Leading whitespace is scanned once, and the buffer never grows past
	// the detector's limit, whitespace or not.
	if int64(len(s.buf)) < manifest.MaxContentTypeDetectionBytes {
		s.lead += len(s.buf[s.lead:]) - len(bytes.TrimLeft(s.buf[s.lead:], " \t\r\n"))
		if s.lead == len(s.buf) || s.buf[s.lead] == '{' {
			return len(p), nil
		}
	}

	if err := s.flush(); err != nil {
		return 0, err
	}

	return len(p), nil
}

// flush writes deferred headers and any buffered bytes. It is a no-op once
// the headers are out.
func (s *sniffWriter) flush() error {
	if s.headers == nil {
		return nil
	}

	headers := s.headers
	s.headers = nil
	headers(s.buf[:min(int64(len(s.buf)), manifest.MaxContentTypeDetectionBytes)])

	buf := s.buf
	s.buf = nil

	if len(buf) == 0 {
		return nil
	}

	_, err := s.w.Write(buf)

	return err
}

// Flush forwards to the response once the headers are out, so the
// fetcher's early flush still reaches the client on typed streams.
func (s *sniffWriter) Flush() {
	if s.headers != nil {
		return
	}

	if fl, ok := s.w.(http.Flusher); ok {
		fl.Flush()
	}
}
