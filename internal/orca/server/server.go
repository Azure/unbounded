// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package server holds the HTTP handlers for the client edge and the
// internal-listener.
//
// Client edge (8443): GET /{bucket}/{key} (with optional Range), HEAD,
// LIST. No auth in dev (server.auth.enabled=false).
//
// Internal listener (8444): GET /internal/fill?<chunk-key>. No mTLS in
// dev (cluster.internal_tls.enabled=false).
package server

import (
	"bufio"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/Azure/unbounded/internal/orca/chunk"
	"github.com/Azure/unbounded/internal/orca/cluster"
	"github.com/Azure/unbounded/internal/orca/config"
	"github.com/Azure/unbounded/internal/orca/origin"
)

// EdgeHandler implements the client-edge S3 surface.
type EdgeHandler struct {
	fc  edgeFetchAPI
	cfg *config.Config
	log *slog.Logger
}

// edgeFetchAPI is the surface area EdgeHandler depends on. The real
// *fetch.Coordinator satisfies it; tests substitute small fakes for
// deterministic unit-level coverage.
type edgeFetchAPI interface {
	HeadObject(ctx context.Context, bucket, key string) (origin.ObjectInfo, error)
	GetChunk(ctx context.Context, k chunk.Key, objectSize int64) (io.ReadCloser, error)
	Origin() origin.Origin
}

// NewEdgeHandler wires the edge handler.
func NewEdgeHandler(fc edgeFetchAPI, cfg *config.Config, log *slog.Logger) *EdgeHandler {
	return &EdgeHandler{fc: fc, cfg: cfg, log: log}
}

// ServeHTTP routes incoming client requests.
//
// Routing (path-style only, since LocalStack and most dev clients
// use path-style):
//
//	GET  /                                  -> ListBuckets (not supported; 405)
//	GET  /{bucket}/?list-type=2&prefix=...  -> ListObjectsV2
//	GET  /{bucket}/                         -> ListObjectsV2 (default)
//	GET  /{bucket}/{key}                    -> GetObject (with optional Range)
//	HEAD /{bucket}/{key}                    -> HeadObject
//	HEAD /{bucket}/                         -> HeadBucket (not supported; 405)
func (h *EdgeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Server.Auth.Enabled {
		// Stub: production would dispatch to bearer/mTLS validation.
		// In dev (auth.enabled=false) we skip entirely.
		http.Error(w, "auth required (server.auth.enabled=true) but not implemented in MVP",
			http.StatusUnauthorized)

		return
	}

	bucket, key := splitPath(r.URL.Path)

	h.log.LogAttrs(r.Context(), slog.LevelDebug, "edge_request",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.String("bucket", bucket),
		slog.String("key", key),
		slog.String("range", r.Header.Get("Range")),
		slog.String("remote", r.RemoteAddr),
	)

	switch r.Method {
	case http.MethodHead:
		if key == "" {
			h.notImplemented(w, "HeadBucket")
			return
		}

		h.handleHead(w, r, bucket, key)
	case http.MethodGet:
		if key == "" {
			h.handleList(w, r, bucket)
			return
		}

		h.handleGet(w, r, bucket, key)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *EdgeHandler) handleHead(w http.ResponseWriter, r *http.Request, bucket, key string) {
	info, err := h.fc.HeadObject(r.Context(), bucket, key)
	if err != nil {
		h.writeOriginError(w, err)
		return
	}

	h.log.LogAttrs(r.Context(), slog.LevelDebug, "edge_head_response",
		slog.String("bucket", bucket),
		slog.String("key", key),
		slog.Int64("size", info.Size),
		slog.String("etag", origin.ETagShort(info.ETag)),
	)

	setObjectHeaders(w, info)
	// HEAD must report the Content-Length the GET response would carry.
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.WriteHeader(http.StatusOK)
}

func (h *EdgeHandler) handleGet(w http.ResponseWriter, r *http.Request, bucket, key string) {
	info, err := h.fc.HeadObject(r.Context(), bucket, key)
	if err != nil {
		h.writeOriginError(w, err)
		return
	}

	// Zero-byte objects short-circuit to 200 + empty body. The normal
	// flow below would compute rangeEnd = info.Size - 1 = -1 and fall
	// into the rangeStart > rangeEnd guard, returning a spurious 416
	// for what should be a successful empty-body fetch. Any Range
	// request against a zero-byte object is genuinely unsatisfiable
	// and remains a 416 (RFC 7233).
	if info.Size == 0 {
		if r.Header.Get("Range") != "" {
			http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
			return
		}

		setObjectHeaders(w, info)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)

		h.log.LogAttrs(r.Context(), slog.LevelDebug, "edge_get_empty_object",
			slog.String("bucket", bucket),
			slog.String("key", key),
		)

		return
	}

	// Determine byte range.
	var (
		rangeStart int64
		rangeEnd   = info.Size - 1
		hasRange   bool
		statusCode = http.StatusOK
	)
	if rh := r.Header.Get("Range"); rh != "" {
		s, e, ok := parseSimpleByteRange(rh, info.Size)
		if !ok {
			http.Error(w, "invalid Range", http.StatusRequestedRangeNotSatisfiable)
			return
		}

		rangeStart, rangeEnd = s, e
		hasRange = true
		statusCode = http.StatusPartialContent
	}

	if rangeStart > rangeEnd {
		http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return
	}

	chunkSize := h.cfg.Chunking.Size
	firstChunk, lastChunk := chunk.IndexRange(rangeStart, rangeEnd, chunkSize, info.Size)

	h.log.LogAttrs(r.Context(), slog.LevelDebug, "edge_get_plan",
		slog.String("bucket", bucket),
		slog.String("key", key),
		slog.Int64("range_start", rangeStart),
		slog.Int64("range_end", rangeEnd),
		slog.Int64("first_chunk", firstChunk),
		slog.Int64("last_chunk", lastChunk),
		slog.Bool("has_range", hasRange),
	)

	// Fetch the first chunk before committing any response headers
	// so that origin errors (404, auth, timeout, mid-stream blob
	// fault) surface as a clean S3-style error response instead of
	// a half-written 200 followed by a dropped connection. Once the
	// first byte is in hand we know the rest of the stream is
	// "tentatively" healthy; subsequent chunk failures remain
	// mid-stream aborts.
	firstKey := chunk.Key{
		OriginID:  h.cfg.Origin.ID,
		Bucket:    bucket,
		ObjectKey: key,
		ETag:      info.ETag,
		ChunkSize: chunkSize,
		Index:     firstChunk,
	}

	firstBody, err := h.fc.GetChunk(r.Context(), firstKey, info.Size)
	if err != nil {
		h.writeOriginError(w, err)
		return
	}
	// Peek a single byte to drain any first-read errors from the
	// underlying body (e.g. cachestore-backed bodies can fail on the
	// first network read). io.EOF on peek is acceptable for the
	// degenerate empty-chunk case.
	firstReader := bufio.NewReader(firstBody)
	if _, err := firstReader.Peek(1); err != nil && !errors.Is(err, io.EOF) {
		firstBody.Close() //nolint:errcheck // closing on error path
		h.writeOriginError(w, err)

		return
	}

	// Set headers eagerly. The response headers are committed below
	// once the first chunk has been confirmed readable; thereafter
	// any failure becomes a mid-stream abort.
	setObjectHeaders(w, info)
	w.Header().Set("Content-Length", strconv.FormatInt(rangeEnd-rangeStart+1, 10))

	if hasRange {
		w.Header().Set("Content-Range",
			fmt.Sprintf("bytes %d-%d/%d", rangeStart, rangeEnd, info.Size))
	}
	// Write status now; subsequent failures become mid-stream aborts.
	w.WriteHeader(statusCode)

	// Stream the first chunk's slice. Any failure here is now a
	// mid-stream abort (headers are committed).
	off, length := chunk.ChunkSlice(firstChunk, chunkSize, rangeStart, rangeEnd, info.Size)
	if err := streamSlice(w, firstReader, off, length); err != nil {
		firstBody.Close() //nolint:errcheck // body close best-effort, response already streaming
		h.log.LogAttrs(r.Context(), slog.LevelWarn, "mid-stream copy failed",
			slog.String("bucket", bucket),
			slog.String("key", key),
			slog.Int64("chunk", firstChunk),
			slog.Any("err", err),
		)

		return
	}

	firstBody.Close() //nolint:errcheck // body close best-effort, response already streaming

	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	for ci := firstChunk + 1; ci <= lastChunk; ci++ {
		ckey := chunk.Key{
			OriginID:  h.cfg.Origin.ID,
			Bucket:    bucket,
			ObjectKey: key,
			ETag:      info.ETag,
			ChunkSize: chunkSize,
			Index:     ci,
		}

		h.log.LogAttrs(r.Context(), slog.LevelDebug, "edge_get_chunk_next",
			slog.String("bucket", bucket),
			slog.String("key", key),
			slog.Int64("chunk", ci),
		)

		body, err := h.fc.GetChunk(r.Context(), ckey, info.Size)
		if err != nil {
			// We've already sent headers; abort the response.
			h.log.LogAttrs(r.Context(), slog.LevelWarn, "mid-stream chunk fetch failed",
				slog.String("bucket", bucket),
				slog.String("key", key),
				slog.Int64("chunk", ci),
				slog.Any("err", err),
			)

			return
		}

		off, length := chunk.ChunkSlice(ci, chunkSize, rangeStart, rangeEnd, info.Size)
		if err := streamSlice(w, body, off, length); err != nil {
			body.Close() //nolint:errcheck // chunk body close best-effort, response already streaming
			h.log.LogAttrs(r.Context(), slog.LevelWarn, "mid-stream copy failed",
				slog.String("bucket", bucket),
				slog.String("key", key),
				slog.Int64("chunk", ci),
				slog.Any("err", err),
			)

			return
		}

		body.Close() //nolint:errcheck // chunk body close best-effort, response already streaming

		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	h.log.LogAttrs(r.Context(), slog.LevelDebug, "edge_get_complete",
		slog.String("bucket", bucket),
		slog.String("key", key),
		slog.Int64("bytes", rangeEnd-rangeStart+1),
	)
}

// streamSlice copies length bytes starting at off from src to dst.
func streamSlice(dst io.Writer, src io.Reader, off, length int64) error {
	if off > 0 {
		if _, err := io.CopyN(io.Discard, src, off); err != nil {
			return err
		}
	}

	if length > 0 {
		if _, err := io.CopyN(dst, src, length); err != nil {
			return err
		}
	}

	return nil
}

// handleList is a thin pass-through to Origin.List for v1 prototype.
func (h *EdgeHandler) handleList(w http.ResponseWriter, r *http.Request, bucket string) {
	// Pass-through; very minimal S3 ListObjectsV2 shape. Reviewers can
	// curl this for sanity but full S3 list semantics are not in MVP.
	prefix := r.URL.Query().Get("prefix")
	marker := r.URL.Query().Get("continuation-token")
	maxStr := r.URL.Query().Get("max-keys")
	maxKeys := 1000

	if maxStr != "" {
		if v, err := strconv.Atoi(maxStr); err == nil && v > 0 {
			maxKeys = v
		}
	}

	type listEntry struct {
		Key  string `xml:"Key"`
		Size int64  `xml:"Size"`
		ETag string `xml:"ETag"`
	}

	type listResult struct {
		XMLName     xml.Name    `xml:"ListBucketResult"`
		Name        string      `xml:"Name"`
		Prefix      string      `xml:"Prefix"`
		KeyCount    int         `xml:"KeyCount"`
		MaxKeys     int         `xml:"MaxKeys"`
		IsTruncated bool        `xml:"IsTruncated"`
		NextMarker  string      `xml:"NextContinuationToken,omitempty"`
		Contents    []listEntry `xml:"Contents"`
	}

	or := h.fc.Origin()

	res, err := or.List(r.Context(), bucket, prefix, marker, maxKeys)
	if err != nil {
		h.writeOriginError(w, err)
		return
	}

	body := listResult{
		Name:        bucket,
		Prefix:      prefix,
		KeyCount:    len(res.Entries),
		MaxKeys:     maxKeys,
		IsTruncated: res.IsTruncated,
		NextMarker:  res.NextMarker,
	}
	for _, e := range res.Entries {
		body.Contents = append(body.Contents, listEntry{Key: e.Key, Size: e.Size, ETag: e.ETag})
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	enc := xml.NewEncoder(w)

	if err := enc.Encode(body); err != nil {
		// Headers already sent; we cannot change the status. Log so
		// truncated / malformed LIST responses are visible, matching
		// the mid-stream warn-level treatment in the GET path.
		h.log.LogAttrs(r.Context(), slog.LevelWarn, "list xml encode failed",
			slog.String("bucket", bucket),
			slog.String("prefix", prefix),
			slog.Any("err", err),
		)
	}
}

func (h *EdgeHandler) notImplemented(w http.ResponseWriter, op string) {
	http.Error(w, op+" not implemented in MVP", http.StatusNotImplemented)
}

func (h *EdgeHandler) writeOriginError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, origin.ErrNotFound):
		http.Error(w, "NoSuchKey", http.StatusNotFound)
	case errors.Is(err, origin.ErrAuth):
		http.Error(w, "Unauthorized origin", http.StatusBadGateway)
	default:
		var (
			ube *origin.UnsupportedBlobTypeError
			ec  *origin.OriginETagChangedError
			mte *origin.MissingETagError
		)

		switch {
		case errors.As(err, &ube):
			http.Error(w, "OriginUnsupported: "+ube.Error(), http.StatusBadGateway)
		case errors.As(err, &ec):
			http.Error(w, "OriginETagChanged", http.StatusBadGateway)
		case errors.As(err, &mte):
			http.Error(w, "OriginMissingETag: "+mte.Error(), http.StatusBadGateway)
		default:
			h.log.LogAttrs(context.Background(), slog.LevelWarn, "origin error",
				slog.Any("err", err),
			)
			http.Error(w, "OriginUnreachable", http.StatusBadGateway)
		}
	}
}

func setObjectHeaders(w http.ResponseWriter, info origin.ObjectInfo) {
	if info.ContentType != "" {
		w.Header().Set("Content-Type", info.ContentType)
	}

	if info.ETag != "" {
		w.Header().Set("ETag", "\""+info.ETag+"\"")
	}

	w.Header().Set("Accept-Ranges", "bytes")
}

func splitPath(p string) (bucket, key string) {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return "", ""
	}

	idx := strings.IndexByte(p, '/')
	if idx < 0 {
		return p, ""
	}

	return p[:idx], p[idx+1:]
}

func parseSimpleByteRange(h string, size int64) (start, end int64, ok bool) {
	if !strings.HasPrefix(h, "bytes=") {
		return 0, 0, false
	}

	spec := strings.TrimPrefix(h, "bytes=")

	parts := strings.Split(spec, "-")
	if len(parts) != 2 {
		return 0, 0, false
	}

	if parts[0] == "" {
		// Suffix: -N (last N bytes)
		n, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || n <= 0 || n > size {
			return 0, 0, false
		}

		return size - n, size - 1, true
	}

	s, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || s < 0 {
		return 0, 0, false
	}

	if parts[1] == "" {
		return s, size - 1, true
	}

	e, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || e < s {
		return 0, 0, false
	}

	if e >= size {
		e = size - 1
	}

	return s, e, true
}

// InternalHandler implements GET /internal/fill on the internal
// listener. Plain HTTP/2 (no mTLS) in dev.
type InternalHandler struct {
	fc  internalFetchAPI
	cl  *cluster.Cluster
	log *slog.Logger
}

// internalFetchAPI is the surface area InternalHandler depends on. The
// real *fetch.Coordinator satisfies it; tests substitute small fakes.
type internalFetchAPI interface {
	FillForPeer(ctx context.Context, k chunk.Key, objectSize int64) (io.ReadCloser, error)
}

// NewInternalHandler wires the internal handler.
func NewInternalHandler(fc internalFetchAPI, cl *cluster.Cluster, log *slog.Logger) *InternalHandler {
	return &InternalHandler{fc: fc, cl: cl, log: log}
}

// ServeHTTP handles GET /internal/fill?<chunk-key-params>.
func (h *InternalHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/internal/fill" {
		http.NotFound(w, r)
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if r.Header.Get("X-Orca-Internal") != "1" {
		http.Error(w, "missing X-Orca-Internal header", http.StatusBadRequest)
		return
	}

	k, objectSize, err := cluster.DecodeChunkKey(r.URL.Query())
	if err != nil {
		http.Error(w, "invalid chunk key: "+err.Error(), http.StatusBadRequest)
		return
	}

	h.log.LogAttrs(r.Context(), slog.LevelDebug, "internal_fill_request",
		intChunkAttrs(k),
		slog.Int64("object_size", objectSize),
		slog.String("remote", r.RemoteAddr),
	)

	if !h.cl.IsCoordinator(k) {
		h.log.LogAttrs(r.Context(), slog.LevelDebug, "internal_fill_not_coordinator",
			intChunkAttrs(k),
			slog.String("remote", r.RemoteAddr),
		)
		http.Error(w, `{"reason":"not_coordinator"}`, http.StatusConflict)

		return
	}

	body, err := h.fc.FillForPeer(r.Context(), k, objectSize)
	if err != nil {
		h.log.LogAttrs(r.Context(), slog.LevelWarn, "internal fill failed",
			intChunkAttrs(k),
			slog.Any("err", err),
		)
		http.Error(w, "fill failed", http.StatusBadGateway)

		return
	}
	defer body.Close() //nolint:errcheck // internal-fill body close best-effort

	// Set Content-Length so the requesting peer can validate the
	// streamed body length and detect mid-stream truncation. If the
	// expected length is zero (unknown objectSize or empty chunk) we
	// omit Content-Length; the requester then falls back to
	// connection-close framing without length validation.
	expectedLen := k.ExpectedLen(objectSize)
	if expectedLen > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(expectedLen, 10))
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)

	if _, copyErr := io.Copy(w, body); copyErr != nil {
		h.log.LogAttrs(r.Context(), slog.LevelWarn, "internal fill copy failed",
			intChunkAttrs(k),
			slog.Any("err", copyErr),
		)

		return
	}

	h.log.LogAttrs(r.Context(), slog.LevelDebug, "internal_fill_complete",
		intChunkAttrs(k),
		slog.Int64("bytes", expectedLen),
	)
}

// intChunkAttrs renders the chunk's identifying tuple as a slog
// group attribute matching the cross-package 'chunk' taxonomy.
func intChunkAttrs(k chunk.Key) slog.Attr {
	return slog.Group("chunk",
		slog.String("origin_id", k.OriginID),
		slog.String("bucket", k.Bucket),
		slog.String("key", k.ObjectKey),
		slog.Int64("index", k.Index),
	)
}
