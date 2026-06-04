// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package server holds the HTTP handlers for the client edge and the
// internal-listener.
//
// Client edge (8443): GET /{bucket}/{key} (with optional Range) and
// HEAD /{bucket}/{key}. No auth in dev (server.auth.enabled=false).
// Errors are returned as S3-compatible XML <Error> envelopes so that
// AWS S3 SDKs surface a typed error code to the caller; HEAD errors
// carry status + headers only (no body), matching real S3 behavior.
//
// Internal listener (8444): GET /internal/fill?<chunk-key>. No mTLS in
// dev (cluster.internal_tls.enabled=false). Internal-listener errors
// are plain text or JSON; the internal API is peer-to-peer between
// orca replicas and is never consumed by an S3 SDK.
package server

import (
	"bufio"
	"context"
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
}

// NewEdgeHandler wires the edge handler.
func NewEdgeHandler(fc edgeFetchAPI, cfg *config.Config, log *slog.Logger) *EdgeHandler {
	return &EdgeHandler{fc: fc, cfg: cfg, log: log}
}

// ServeHTTP routes incoming client requests.
//
// Routing (path-style only, since the S3-compatible dev backend and
// most dev clients use path-style):
//
//	GET  /                                  -> ListBuckets (not supported; 501)
//	GET  /{bucket}/                         -> ListObjectsV2 (not supported; 501)
//	GET  /{bucket}/{key}                    -> GetObject (with optional Range)
//	HEAD /                                  -> (not supported; 501)
//	HEAD /{bucket}/                         -> HeadBucket (not supported; 501)
//	HEAD /{bucket}/{key}                    -> HeadObject
func (h *EdgeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Server.Auth.Enabled {
		// Stub: production would dispatch to bearer/mTLS validation.
		// In dev (auth.enabled=false) we skip entirely.
		writeS3Error(w, r, http.StatusUnauthorized, s3ErrAccessDenied,
			"Server auth is enabled but no auth handler is implemented in this build.")

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
			// Both HEAD / and HEAD /{bucket}/ are reported as
			// HeadBucket. HEAD / is not a real S3 operation, but
			// labelling it HeadBucket keeps the surface uniform and
			// makes the 501 self-explanatory.
			h.notImplemented(w, r, "HeadBucket")
			return
		}

		h.handleHead(w, r, bucket, key)
	case http.MethodGet:
		switch {
		case bucket == "":
			h.notImplemented(w, r, "ListBuckets")
		case key == "":
			h.notImplemented(w, r, "ListObjectsV2")
		default:
			h.handleGet(w, r, bucket, key)
		}
	default:
		writeS3Error(w, r, http.StatusMethodNotAllowed, s3ErrMethodNotAllowed,
			"The specified method is not allowed against this resource.")
	}
}

func (h *EdgeHandler) handleHead(w http.ResponseWriter, r *http.Request, bucket, key string) {
	info, err := h.fc.HeadObject(r.Context(), bucket, key)
	if err != nil {
		h.writeOriginError(w, r, err)
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
		h.writeOriginError(w, r, err)
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
			writeS3Error(w, r, http.StatusRequestedRangeNotSatisfiable, s3ErrInvalidRange,
				"The requested range is not satisfiable.",
				withBucketKey(bucket, key))

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
			writeS3Error(w, r, http.StatusRequestedRangeNotSatisfiable, s3ErrInvalidRange,
				"The requested range is not valid for the resource.",
				withBucketKey(bucket, key))

			return
		}

		rangeStart, rangeEnd = s, e
		hasRange = true
		statusCode = http.StatusPartialContent
	}

	if rangeStart > rangeEnd {
		writeS3Error(w, r, http.StatusRequestedRangeNotSatisfiable, s3ErrInvalidRange,
			"The requested range is not satisfiable.",
			withBucketKey(bucket, key))

		return
	}

	chunkSize := chunk.SizeFor(info.Size, h.cfg.Chunking.Size.Int64(), h.cfg.Chunking.AsChunkTiers())
	firstChunk, lastChunk := chunk.IndexRange(rangeStart, rangeEnd, chunkSize, info.Size)

	h.log.LogAttrs(r.Context(), slog.LevelDebug, "edge_get_plan",
		slog.String("bucket", bucket),
		slog.String("key", key),
		slog.Int64("range_start", rangeStart),
		slog.Int64("range_end", rangeEnd),
		slog.Int64("first_chunk", firstChunk),
		slog.Int64("last_chunk", lastChunk),
		slog.Int64("chunk_size", chunkSize),
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
		h.writeOriginError(w, r, err)
		return
	}
	// Peek a single byte to drain any first-read errors from the
	// underlying body (e.g. cachestore-backed bodies can fail on the
	// first network read). io.EOF on peek is acceptable for the
	// degenerate empty-chunk case.
	firstReader := bufio.NewReader(firstBody)
	if _, err := firstReader.Peek(1); err != nil && !errors.Is(err, io.EOF) {
		firstBody.Close() //nolint:errcheck // closing on error path
		h.writeOriginError(w, r, err)

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

	if firstChunk < lastChunk {
		h.streamRemainingChunks(r.Context(), w, bucket, key, info, chunkSize,
			rangeStart, rangeEnd, firstChunk+1, lastChunk)
	}

	h.log.LogAttrs(r.Context(), slog.LevelDebug, "edge_get_complete",
		slog.String("bucket", bucket),
		slog.String("key", key),
		slog.Int64("bytes", rangeEnd-rangeStart+1),
	)
}

// streamRemainingChunks fetches and streams chunks [firstIdx, lastIdx]
// after the first chunk has already been delivered. Honors the
// configured Chunking.Readahead depth: with depth > 0 a producer
// goroutine prefetches up to depth chunks while the consumer streams
// the current one; with depth == 0 the loop is strictly sequential
// (zero-overhead opt-out preserving the pre-readahead behavior).
//
// All failures here are mid-stream aborts: response headers are
// already committed, so the only remedy is logging and returning.
func (h *EdgeHandler) streamRemainingChunks(
	ctx context.Context,
	w http.ResponseWriter,
	bucket, key string,
	info origin.ObjectInfo,
	chunkSize, rangeStart, rangeEnd int64,
	firstIdx, lastIdx int64,
) {
	depth := h.cfg.Chunking.ReadaheadDepth()
	if depth <= 0 {
		h.streamRemainingChunksSequential(ctx, w, bucket, key, info, chunkSize,
			rangeStart, rangeEnd, firstIdx, lastIdx)

		return
	}

	h.streamRemainingChunksReadahead(ctx, w, bucket, key, info, chunkSize,
		rangeStart, rangeEnd, firstIdx, lastIdx, depth)
}

// streamRemainingChunksSequential is the pre-readahead loop body:
// fetch chunk N, stream it, close it, advance. One in-flight chunk
// fetch at a time. Used when Chunking.Readahead is 0.
func (h *EdgeHandler) streamRemainingChunksSequential(
	ctx context.Context,
	w http.ResponseWriter,
	bucket, key string,
	info origin.ObjectInfo,
	chunkSize, rangeStart, rangeEnd int64,
	firstIdx, lastIdx int64,
) {
	for ci := firstIdx; ci <= lastIdx; ci++ {
		ckey := chunk.Key{
			OriginID:  h.cfg.Origin.ID,
			Bucket:    bucket,
			ObjectKey: key,
			ETag:      info.ETag,
			ChunkSize: chunkSize,
			Index:     ci,
		}

		h.log.LogAttrs(ctx, slog.LevelDebug, "edge_get_chunk_next",
			slog.String("bucket", bucket),
			slog.String("key", key),
			slog.Int64("chunk", ci),
		)

		body, err := h.fc.GetChunk(ctx, ckey, info.Size)
		if err != nil {
			h.log.LogAttrs(ctx, slog.LevelWarn, "mid-stream chunk fetch failed",
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
			h.log.LogAttrs(ctx, slog.LevelWarn, "mid-stream copy failed",
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
}

// pendingChunk is one item produced by the readahead pipeline: an
// in-order chunk body (or the error that prevented fetching it).
// The consumer is responsible for Close()ing rc when non-nil.
type pendingChunk struct {
	idx int64
	rc  io.ReadCloser
	err error
}

// readaheadJob is a chunk-fetch slot held in the dispatcher's queue.
// Each job owns a 1-buffered result channel that its worker writes
// to exactly once before exiting.
type readaheadJob struct {
	idx int64
	rc  chan pendingChunk
}

// streamRemainingChunksReadahead runs a producer goroutine that
// fetches chunks ahead into a bounded channel of capacity depth,
// while the main goroutine streams the current chunk to the client.
// This hides per-chunk cachestore RTT behind body transfer time so
// large-blob GETs no longer pay N strictly-serial round trips.
//
// Lifecycle:
//   - Consumer aborts (mid-stream copy failure, fetch error,
//     producer-channel closed early) cancel the producer's context;
//     the producer drains and closes any bodies it has already
//     prefetched on the way out.
//   - Producer panics are recovered, logged, and surface to the
//     consumer as an early channel close; the consumer treats that
//     as a mid-stream abort and returns cleanly.
//   - Context cancellation from the caller (client disconnect)
//     propagates through prefetchCtx, cancelling in-flight
//     GetChunk calls and causing the producer to exit.
func (h *EdgeHandler) streamRemainingChunksReadahead(
	ctx context.Context,
	w http.ResponseWriter,
	bucket, key string,
	info origin.ObjectInfo,
	chunkSize, rangeStart, rangeEnd int64,
	firstIdx, lastIdx int64,
	depth int,
) {
	prefetchCtx, cancelPrefetch := context.WithCancel(ctx)
	defer cancelPrefetch()

	ch := h.prefetchChunks(prefetchCtx, bucket, key, info.ETag, chunkSize, info.Size,
		firstIdx, lastIdx, depth)

	// Drain helper: close any pending bodies left in the channel
	// after we decide to abort. The producer's own deferred
	// per-pending close (on ctx cancel during send-select) covers
	// the in-flight body it is currently fetching; this loop covers
	// the buffered ones the consumer never reaches.
	drain := func() {
		for p := range ch {
			if p.rc != nil {
				_ = p.rc.Close() //nolint:errcheck // drain best-effort
			}
		}
	}

	expectedIdx := firstIdx

	for p := range ch {
		if p.err != nil {
			if p.rc != nil {
				_ = p.rc.Close() //nolint:errcheck // close error-path body
			}

			h.log.LogAttrs(ctx, slog.LevelWarn, "mid-stream chunk fetch failed",
				slog.String("bucket", bucket),
				slog.String("key", key),
				slog.Int64("chunk", p.idx),
				slog.Any("err", p.err),
			)
			cancelPrefetch()
			drain()

			return
		}

		if p.idx != expectedIdx {
			// Defensive: producer is required to deliver chunks in
			// index order. A mismatch indicates a programming error
			// upstream; treat as mid-stream abort.
			if p.rc != nil {
				_ = p.rc.Close() //nolint:errcheck
			}

			h.log.LogAttrs(ctx, slog.LevelError, "readahead order violation",
				slog.String("bucket", bucket),
				slog.String("key", key),
				slog.Int64("expected", expectedIdx),
				slog.Int64("got", p.idx),
			)
			cancelPrefetch()
			drain()

			return
		}

		h.log.LogAttrs(ctx, slog.LevelDebug, "edge_get_chunk_next",
			slog.String("bucket", bucket),
			slog.String("key", key),
			slog.Int64("chunk", p.idx),
		)

		off, length := chunk.ChunkSlice(p.idx, chunkSize, rangeStart, rangeEnd, info.Size)
		if err := streamSlice(w, p.rc, off, length); err != nil {
			_ = p.rc.Close() //nolint:errcheck
			h.log.LogAttrs(ctx, slog.LevelWarn, "mid-stream copy failed",
				slog.String("bucket", bucket),
				slog.String("key", key),
				slog.Int64("chunk", p.idx),
				slog.Any("err", err),
			)
			cancelPrefetch()
			drain()

			return
		}

		_ = p.rc.Close() //nolint:errcheck

		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}

		expectedIdx++
	}

	if expectedIdx <= lastIdx {
		// Channel closed before all chunks were delivered. The
		// producer either panicked (already logged) or its context
		// was cancelled (client disconnect or earlier mid-stream
		// abort - the latter would have returned above). Surface as
		// a mid-stream warning so operators see truncated responses.
		h.log.LogAttrs(ctx, slog.LevelWarn, "readahead truncated response",
			slog.String("bucket", bucket),
			slog.String("key", key),
			slog.Int64("expected_through", lastIdx),
			slog.Int64("delivered_through", expectedIdx-1),
		)
	}
}

// prefetchChunks fetches chunks [firstIdx, lastIdx] into a bounded
// channel of capacity depth, with up to depth fetches in flight in
// parallel. Bodies are delivered in chunk-index order so the
// consumer can stream them straight to the client without
// reassembly. Caller drains the channel and owns Close() for any
// non-nil rc it receives.
//
// Fan-out model:
//   - A dispatcher goroutine spawns one worker goroutine per chunk
//     index, gated by a depth-sized job queue so peak in-flight
//     workers stays at depth (+ at most one in-flight push and one
//     in-flight delivery).
//   - Each worker calls h.fc.GetChunk for its chunk and writes the
//     result to a per-job, 1-buffered result channel.
//   - The dispatcher pushes job descriptors onto the queue in
//     chunk-index order so the delivery loop reads results in that
//     same order.
//
// Lifecycle:
//   - All workers ALWAYS write exactly once to their result channel
//     before exiting. This invariant lets the delivery loop block
//     on `<-j.rc` without risk of deadlock even on ctx-cancel.
//   - On ctx cancellation the dispatcher drains its currently-spawned
//     worker (waiting for the unconditional rc write) and exits.
//     The delivery loop drains any remaining queued jobs the same
//     way, closing the body in each result.
//   - Producer panics are recovered, logged, and surface to the
//     consumer as an early channel close; the consumer treats that
//     as a mid-stream abort.
func (h *EdgeHandler) prefetchChunks(
	ctx context.Context,
	bucket, key, etag string,
	chunkSize, objectSize int64,
	firstIdx, lastIdx int64,
	depth int,
) <-chan pendingChunk {
	out := make(chan pendingChunk, depth)

	queue := make(chan readaheadJob, depth)

	// Dispatcher: spawn workers in chunk-index order, gated by the
	// queue's capacity. Each worker is independent and runs to
	// completion (always writes its result), so the dispatcher
	// doesn't need to track them after spawning.
	go func() {
		defer close(queue)
		defer func() {
			if r := recover(); r != nil {
				h.log.LogAttrs(ctx, slog.LevelError, "readahead dispatcher panic",
					slog.String("bucket", bucket),
					slog.String("key", key),
					slog.Any("panic", r),
				)
			}
		}()

		for ci := firstIdx; ci <= lastIdx; ci++ {
			if err := ctx.Err(); err != nil {
				return
			}

			rc := make(chan pendingChunk, 1)

			// Spawn worker first so the result channel always
			// receives a write, even if ctx is cancelled while we
			// block on the queue push below. The worker's
			// GetChunk call will short-circuit on a cancelled ctx
			// with err != nil and rc == nil, satisfying the
			// "always write" invariant.
			go func(idx int64, rc chan<- pendingChunk) {
				defer func() {
					if r := recover(); r != nil {
						h.log.LogAttrs(ctx, slog.LevelError, "readahead worker panic",
							slog.String("bucket", bucket),
							slog.String("key", key),
							slog.Int64("chunk", idx),
							slog.Any("panic", r),
						)
						// Preserve the write-once invariant: send a
						// synthetic error so the delivery loop sees
						// the panic-affected chunk as a fetch error
						// rather than blocking forever on rc.
						rc <- pendingChunk{idx: idx, err: fmt.Errorf("readahead worker panic: %v", r)}
					}
				}()

				ckey := chunk.Key{
					OriginID:  h.cfg.Origin.ID,
					Bucket:    bucket,
					ObjectKey: key,
					ETag:      etag,
					ChunkSize: chunkSize,
					Index:     idx,
				}

				body, err := h.fc.GetChunk(ctx, ckey, objectSize)
				rc <- pendingChunk{idx: idx, rc: body, err: err}
			}(ci, rc)

			select {
			case queue <- readaheadJob{idx: ci, rc: rc}:
			case <-ctx.Done():
				// Worker is in flight; drain it so the body (if any)
				// is closed and the goroutine doesn't leak.
				p := <-rc
				if p.rc != nil {
					_ = p.rc.Close() //nolint:errcheck // ctx-cancel body close best-effort
				}

				return
			}
		}
	}()

	// Delivery: read worker results in chunk-index order and forward
	// to `out`. Drains in-flight jobs on ctx-cancel.
	go func() {
		defer close(out)
		defer func() {
			if r := recover(); r != nil {
				h.log.LogAttrs(ctx, slog.LevelError, "readahead delivery panic",
					slog.String("bucket", bucket),
					slog.String("key", key),
					slog.Any("panic", r),
				)
			}
		}()

		for j := range queue {
			p := <-j.rc // worker always writes; safe blocking read

			if err := ctx.Err(); err != nil {
				if p.rc != nil {
					_ = p.rc.Close() //nolint:errcheck // drain best-effort
				}

				drainQueue(queue)

				return
			}

			select {
			case out <- p:
			case <-ctx.Done():
				if p.rc != nil {
					_ = p.rc.Close() //nolint:errcheck // drain best-effort
				}

				drainQueue(queue)

				return
			}
		}
	}()

	return out
}

// drainQueue is a helper that empties any remaining job descriptors
// from the readahead queue, waits for each spawned worker to deliver
// its result, and closes any body the result carries. Used on
// ctx-cancel cleanup paths so worker goroutines and cachestore
// response bodies do not leak when the consumer aborts mid-stream.
func drainQueue(queue <-chan readaheadJob) {
	for j := range queue {
		p := <-j.rc
		if p.rc != nil {
			_ = p.rc.Close() //nolint:errcheck // cleanup best-effort
		}
	}
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

func (h *EdgeHandler) notImplemented(w http.ResponseWriter, r *http.Request, op string) {
	writeS3Error(w, r, http.StatusNotImplemented, s3ErrNotImplemented,
		op+" is not implemented by orca.")
}

func (h *EdgeHandler) writeOriginError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, origin.ErrNotFound):
		writeS3Error(w, r, http.StatusNotFound, s3ErrNoSuchKey,
			"The specified key does not exist.")
	case errors.Is(err, origin.ErrAuth):
		// ErrAuth maps to 502 BadGateway, NOT 403 AccessDenied.
		//
		// A 401/403 from the upstream origin means *orca's* own
		// credentials were rejected by the origin; the calling
		// client's credentials are not at fault. Returning 403 to the
		// client would falsely imply the client should rotate its
		// own credentials, which would not fix anything.
		//
		// From the client's perspective an orca-vs-origin auth
		// failure is functionally an upstream outage: orca cannot
		// satisfy the request through no fault of the client.
		// 502 BadGateway communicates that accurately, and the
		// orca-specific OriginUnauthorized code lets operators with
		// access to orca logs tell this case apart from a generic
		// origin failure.
		writeS3Error(w, r, http.StatusBadGateway, s3ErrOriginUnauthorized,
			"Origin rejected orca's credentials. This is an orca/origin configuration issue, not a client problem.")
	default:
		var (
			ube *origin.UnsupportedBlobTypeError
			ec  *origin.OriginETagChangedError
			mte *origin.MissingETagError
		)

		switch {
		case errors.As(err, &ube):
			writeS3Error(w, r, http.StatusBadGateway, s3ErrOriginUnsupported, ube.Error())
		case errors.As(err, &ec):
			writeS3Error(w, r, http.StatusBadGateway, s3ErrOriginETagChanged,
				"Object changed at origin mid-fetch.")
		case errors.As(err, &mte):
			writeS3Error(w, r, http.StatusBadGateway, s3ErrOriginMissingETag, mte.Error())
		default:
			h.log.LogAttrs(context.Background(), slog.LevelWarn, "origin error",
				slog.Any("err", err),
			)
			writeS3Error(w, r, http.StatusBadGateway, s3ErrOriginUnreachable,
				"Origin request failed; see orca logs for details.")
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
