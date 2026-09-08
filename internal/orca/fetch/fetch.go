// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package fetch is the per-replica fill orchestrator: per-ChunkKey
// singleflight, pre-header origin retry, per-replica origin
// concurrency cap, and cross-replica fill via the cluster's internal
// RPC.
//
// The dedup model is per-replica singleflight + cluster-wide dedup
// via a rendezvous-hashed coordinator. No disk spool; joiners stream
// from the leader's in-memory ring buffer.
//
// Pre-header retry: the coordinator may retry origin GETs up to the
// budget in cfg.Origin.Retry until the first byte is committed to
// the client response. Once headers are sent retries are not safe and
// failures become mid-stream aborts.
package fetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/Azure/unbounded/internal/orca/cachestore"
	"github.com/Azure/unbounded/internal/orca/chunk"
	"github.com/Azure/unbounded/internal/orca/chunkcatalog"
	"github.com/Azure/unbounded/internal/orca/cluster"
	"github.com/Azure/unbounded/internal/orca/config"
	"github.com/Azure/unbounded/internal/orca/metadata"
	"github.com/Azure/unbounded/internal/orca/origin"
)

// Coordinator orchestrates per-replica chunk fills.
type Coordinator struct {
	or  origin.Origin
	cs  cachestore.CacheStore
	cl  *cluster.Cluster
	cat *chunkcatalog.Catalog
	mc  *metadata.Cache
	cfg *config.Config
	log *slog.Logger

	// Per-replica origin concurrency cap. Bounds in-flight
	// Origin.GetRange calls to floor(target_global / target_replicas).
	originSem chan struct{}

	// Per-ChunkKey singleflight. Concurrent local fills for the same
	// chunk collapse to one origin GetRange.
	mu       sync.Mutex
	inflight map[string]*fill
}

type fill struct {
	done    chan struct{}
	bodyBuf *bytes.Buffer // buffered chunk after fetch (in-memory, bounded by chunk size)
	err     error
}

// NewCoordinator wires up the fetch coordinator. The log is used for
// peer-fallback warnings and commit-after-serve failure traces, plus
// debug-level tracing through every chunk-resolution decision point
// when the operator enables logging.level: debug. The caller (usually
// app.Start) injects the app-wide slog.Logger so fetch-path logs are
// unified with the rest of the runtime's output. Passing nil falls
// back to slog.Default().
func NewCoordinator(
	or origin.Origin,
	cs cachestore.CacheStore,
	cl *cluster.Cluster,
	cat *chunkcatalog.Catalog,
	mc *metadata.Cache,
	cfg *config.Config,
	log *slog.Logger,
) *Coordinator {
	tpr := cfg.TargetPerReplica()
	if tpr < 1 {
		tpr = 1
	}

	if log == nil {
		log = slog.Default()
	}

	return &Coordinator{
		or:        or,
		cs:        cs,
		cl:        cl,
		cat:       cat,
		mc:        mc,
		cfg:       cfg,
		log:       log,
		originSem: make(chan struct{}, tpr),
		inflight:  make(map[string]*fill),
	}
}

// HeadObject returns object metadata, satisfying client HEAD requests.
//
// Rejects responses with an empty ETag via origin.MissingETagError.
// chunk.Path encodes the ETag in its hash input; a stable cache key
// requires the origin to supply one. Without an ETag, two different
// versions of the same (bucket, key) would alias to the same
// chunk.Path and serve stale bytes silently. The negative result is
// cached at NegativeTTL so we do not re-Head a misconfigured origin
// on every request.
func (c *Coordinator) HeadObject(ctx context.Context, bucket, key string) (origin.ObjectInfo, error) {
	c.log.LogAttrs(ctx, slog.LevelDebug, "head_object",
		slog.String("origin_id", c.cfg.Origin.ID),
		slog.String("bucket", bucket),
		slog.String("key", key),
	)

	return c.mc.LookupOrFetch(ctx, c.cfg.Origin.ID, bucket, key,
		func(ctx context.Context) (origin.ObjectInfo, error) {
			info, err := c.or.Head(ctx, bucket, key)
			if err != nil {
				return info, err
			}

			if info.ETag == "" {
				return info, &origin.MissingETagError{Bucket: bucket, Key: key}
			}

			return info, nil
		})
}

// GetChunk returns a reader over the chunk's bytes, fulfilling either
// from CacheStore (hit) or by orchestrating a cluster-wide
// dedup'd fill (miss).
//
// objectSize is the authoritative size of the object the chunk
// belongs to (from origin Head). It is used to clamp the cachestore
// read length and to size the tail chunk correctly on a miss.
//
// On miss:
//   - If self is the coordinator: run local fill (origin GET via retry,
//     commit to CacheStore, populate buffer for joiners).
//   - If a peer is the coordinator: send /internal/fill to that peer;
//     stream from peer's response. On 409 Conflict, fall back to local
//     fill.
func (c *Coordinator) GetChunk(ctx context.Context, k chunk.Key, objectSize int64) (io.ReadCloser, error) {
	c.log.LogAttrs(ctx, slog.LevelDebug, "get_chunk",
		chunkAttrs(k),
		slog.Int64("object_size", objectSize),
		slog.Int64("expected_len", k.ExpectedLen(objectSize)),
	)

	if rc, hit, err := c.lookupOrStat(ctx, k, objectSize); err != nil {
		return nil, err
	} else if hit {
		return rc, nil
	}

	// Cluster-wide dedup: route to coordinator.
	coord := c.cl.Coordinator(k)

	c.log.LogAttrs(ctx, slog.LevelDebug, "coordinator_selected",
		chunkAttrs(k),
		slog.String("coord_ip", coord.IP),
		slog.Bool("is_self", coord.Self),
	)

	if !coord.Self {
		c.log.LogAttrs(ctx, slog.LevelDebug, "peer_fill_attempt",
			chunkAttrs(k),
			slog.String("peer_ip", coord.IP),
		)

		rc, err := c.cl.FillFromPeer(ctx, coord, k, objectSize)
		if err == nil {
			c.log.LogAttrs(ctx, slog.LevelDebug, "peer_fill_success",
				chunkAttrs(k),
				slog.String("peer_ip", coord.IP),
			)

			return rc, nil
		}

		if errors.Is(err, cluster.ErrPeerNotCoordinator) {
			c.log.LogAttrs(ctx, slog.LevelWarn, "peer reported not-coordinator; falling back to local fill",
				chunkAttrs(k),
				slog.String("peer_ip", coord.IP),
			)
			// fall through to local fill
		} else {
			c.log.LogAttrs(ctx, slog.LevelWarn, "internal-fill RPC failed; falling back to local fill",
				chunkAttrs(k),
				slog.String("peer_ip", coord.IP),
				slog.Any("err", err),
			)
		}
	}

	return c.fillLocal(ctx, k, objectSize)
}

// FillForPeer is the path taken by the /internal/fill handler.
//
// The receiver becomes the leader for this fill (or joins an in-flight
// fill for the same key). Returns a streaming body of the entire chunk.
func (c *Coordinator) FillForPeer(ctx context.Context, k chunk.Key, objectSize int64) (io.ReadCloser, error) {
	c.log.LogAttrs(ctx, slog.LevelDebug, "fill_for_peer",
		chunkAttrs(k),
		slog.Int64("object_size", objectSize),
	)

	if rc, hit, err := c.lookupOrStat(ctx, k, objectSize); err != nil {
		return nil, err
	} else if hit {
		return rc, nil
	}

	return c.fillLocal(ctx, k, objectSize)
}

// lookupOrStat is the shared catalog-hit / cachestore-stat probe used
// by both GetChunk and FillForPeer. Returns (body, true, nil) when a
// pre-existing chunk is found, (nil, false, nil) on a clean miss
// (caller should run the appropriate fill path), or (nil, false, err)
// for non-recoverable cachestore errors.
//
// On a catalog hit that turns out to be stale (cachestore returns
// ErrNotFound), the catalog entry is forgotten so the next call
// re-stats fresh.
func (c *Coordinator) lookupOrStat(ctx context.Context, k chunk.Key, objectSize int64) (io.ReadCloser, bool, error) {
	expected := k.ExpectedLen(objectSize)

	if c.cat.Lookup(k) {
		c.log.LogAttrs(ctx, slog.LevelDebug, "catalog_hit",
			chunkAttrs(k),
		)

		rc, err := c.cs.GetChunk(ctx, k, 0, expected)
		if err == nil {
			return rc, true, nil
		}

		if errors.Is(err, cachestore.ErrNotFound) {
			c.log.LogAttrs(ctx, slog.LevelDebug, "catalog_stale_forgotten",
				chunkAttrs(k),
			)
			c.cat.Forget(k)
			// fall through to stat
		} else {
			return nil, false, err
		}
	}

	info, err := c.cs.Stat(ctx, k)
	if err != nil {
		if errors.Is(err, cachestore.ErrNotFound) {
			c.log.LogAttrs(ctx, slog.LevelDebug, "cachestore_stat_miss",
				chunkAttrs(k),
			)

			return nil, false, nil
		}

		return nil, false, err
	}

	c.log.LogAttrs(ctx, slog.LevelDebug, "cachestore_stat_hit",
		chunkAttrs(k),
		slog.Int64("size", info.Size),
	)

	c.cat.Record(k)

	// Trust the stat's reported size if it disagrees with our
	// expectation (e.g. older committed entry from before a chunk
	// size change), but clamp to the expected length so a corrupt
	// larger stat does not leak bytes past the object end.
	readLen := info.Size
	if expected > 0 && readLen > expected {
		readLen = expected
	}

	rc, err := c.cs.GetChunk(ctx, k, 0, readLen)
	if err != nil {
		return nil, false, err
	}

	return rc, true, nil
}

// fillLocal runs (or joins) the singleflight for k on this replica.
func (c *Coordinator) fillLocal(ctx context.Context, k chunk.Key, objectSize int64) (io.ReadCloser, error) {
	path := k.Path()

	c.mu.Lock()

	f, ok := c.inflight[path]
	if !ok {
		f = &fill{done: make(chan struct{})}
		c.inflight[path] = f
		c.mu.Unlock()

		c.log.LogAttrs(ctx, slog.LevelDebug, "fill_local_lead",
			chunkAttrs(k),
		)

		go c.runFill(k, objectSize, f)
	} else {
		c.mu.Unlock()
		c.log.LogAttrs(ctx, slog.LevelDebug, "fill_local_join",
			chunkAttrs(k),
		)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.done:
	}

	if f.err != nil {
		return nil, f.err
	}

	return io.NopCloser(bytes.NewReader(f.bodyBuf.Bytes())), nil
}

func (c *Coordinator) runFill(k chunk.Key, objectSize int64, f *fill) {
	// runFill runs on a fill-scoped detached context (not the
	// caller's) so it can complete the cachestore commit step even
	// if the originating client disconnects mid-stream. The 5-minute
	// ceiling bounds the cost: a fill no joiner ever reads still
	// releases its origin-semaphore slot and clears its inflight
	// entry within the budget. Peak per-fill heap is one ChunkSize
	// bytes.Buffer (8 MiB default).
	//
	// Commit-after-serve ordering: once the origin body is fully
	// fetched and validated, joiners are released (close(f.done))
	// BEFORE the PutChunk RPC begins. This shaves joiner latency by
	// the cachestore commit time on the cold-fill path: joiners get
	// bytes as soon as origin delivered them, and the commit runs in
	// parallel from the joiners' perspective. Correctness is
	// preserved because the buffer is fully populated and
	// length-validated before release; PutChunk reads buf.Bytes()
	// concurrently with joiner reads, but bytes.Buffer is never
	// mutated after the final io.Copy returns, so the underlying
	// byte slice is effectively immutable and safe for concurrent
	// reads.
	//
	// release() is sync.Once-wrapped so close(f.done) fires exactly
	// once whether via the explicit success-path call or the deferred
	// safety net (which catches panic paths).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var releaseOnce sync.Once

	release := func() {
		releaseOnce.Do(func() { close(f.done) })
	}

	defer func() {
		release()
		c.mu.Lock()
		delete(c.inflight, k.Path())
		c.mu.Unlock()
	}()

	// Acquire per-replica origin slot.
	queueCtx, queueCancel := context.WithTimeout(ctx, c.cfg.Origin.QueueTimeout)
	defer queueCancel()

	select {
	case c.originSem <- struct{}{}:
	case <-queueCtx.Done():
		f.err = fmt.Errorf("origin: queue timeout (cap=%d)", cap(c.originSem))
		return
	}

	defer func() { <-c.originSem }()

	c.log.LogAttrs(ctx, slog.LevelDebug, "origin_slot_acquired",
		chunkAttrs(k),
		slog.Int("slot_cap", cap(c.originSem)),
	)

	// expectedLen is the authoritative number of bytes we should
	// receive from origin: ChunkSize for non-tail chunks, the
	// remainder for the tail. Production callers always supply a
	// known objectSize, so expectedLen > 0; the wire format
	// (DecodeChunkKey) and edge handler both reject the
	// objectSize == 0 case at their boundaries, so the validation
	// below is always exercised.
	expectedLen := k.ExpectedLen(objectSize)
	off := k.Index * k.ChunkSize

	body, err := c.fetchWithRetry(ctx, k, off, expectedLen)
	if err != nil {
		f.err = err
		return
	}
	defer body.Close() //nolint:errcheck // origin body close best-effort

	buf := &bytes.Buffer{}
	if _, err := io.Copy(buf, body); err != nil {
		f.err = fmt.Errorf("fill copy: %w", err)
		return
	}

	c.log.LogAttrs(ctx, slog.LevelDebug, "origin_body_received",
		chunkAttrs(k),
		slog.Int("bytes", buf.Len()),
		slog.Int64("expected_len", expectedLen),
	)

	if int64(buf.Len()) != expectedLen {
		f.err = fmt.Errorf("origin returned %d bytes, expected %d (chunk=%s)",
			buf.Len(), expectedLen, k.String())

		return
	}

	f.bodyBuf = buf

	// Release joiners BEFORE the PutChunk commit. Joiners' reads of
	// f.bodyBuf.Bytes() are safe to overlap with the PutChunk RPC's
	// read of the same slice: bytes.Buffer's internal slice is no
	// longer mutated after io.Copy returned above.
	release()

	// Commit to CacheStore (asynchronous from joiners'
	// perspective; they have their bytes already).
	commitErr := c.cs.PutChunk(ctx, k, int64(buf.Len()), bytes.NewReader(buf.Bytes()))

	switch {
	case commitErr == nil:
		c.cat.Record(k)
		c.log.LogAttrs(ctx, slog.LevelDebug, "commit_success",
			chunkAttrs(k),
			slog.Int("bytes", buf.Len()),
		)
	case errors.Is(commitErr, cachestore.ErrCommitLost):
		// Another replica won the fill race. The cachestore's
		// stat-then-put step already confirmed the chunk is present
		// (that is how ErrCommitLost is produced), so record it as a
		// hit without re-Stat'ing.
		c.cat.Record(k)
		c.log.LogAttrs(ctx, slog.LevelDebug, "commit_lost",
			chunkAttrs(k),
		)
	default:
		c.log.LogAttrs(ctx, slog.LevelWarn, "commit-after-serve failed",
			chunkAttrs(k),
			slog.Any("err", commitErr),
		)
		// Don't record in catalog; next request refills.
	}
}

func (c *Coordinator) fetchWithRetry(ctx context.Context, k chunk.Key, off, length int64) (io.ReadCloser, error) {
	deadline := time.Now().Add(c.cfg.Origin.Retry.MaxTotalDuration)
	backoff := c.cfg.Origin.Retry.BackoffInitial

	var lastErr error

	for attempt := 1; attempt <= c.cfg.Origin.Retry.Attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("origin retry exhausted (duration); last err: %w", lastErr)
		}

		c.log.LogAttrs(ctx, slog.LevelDebug, "origin_get_range_attempt",
			chunkAttrs(k),
			slog.Int("attempt", attempt),
			slog.Int64("off", off),
			slog.Int64("length", length),
		)

		body, err := c.or.GetRange(ctx, k.Bucket, k.ObjectKey, k.ETag, off, length)
		if err == nil {
			c.log.LogAttrs(ctx, slog.LevelDebug, "origin_get_range_ok",
				chunkAttrs(k),
				slog.Int("attempt", attempt),
			)

			return body, nil
		}

		lastErr = err
		// Non-retryable: ETag changed.
		var etagChanged *origin.OriginETagChangedError
		if errors.As(err, &etagChanged) {
			c.log.LogAttrs(ctx, slog.LevelDebug, "origin_etag_changed",
				chunkAttrs(k),
				slog.Int("attempt", attempt),
			)
			c.mc.Invalidate(c.cfg.Origin.ID, k.Bucket, k.ObjectKey)

			return nil, err
		}
		// Non-retryable: not found.
		if errors.Is(err, origin.ErrNotFound) {
			c.log.LogAttrs(ctx, slog.LevelDebug, "origin_not_found",
				chunkAttrs(k),
				slog.Int("attempt", attempt),
			)

			return nil, err
		}

		c.log.LogAttrs(ctx, slog.LevelDebug, "origin_retryable_error",
			chunkAttrs(k),
			slog.Int("attempt", attempt),
			slog.Any("err", err),
			slog.Duration("next_backoff", backoff),
		)
		// Backoff.
		if attempt < c.cfg.Origin.Retry.Attempts {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}

			backoff *= 2
			if backoff > c.cfg.Origin.Retry.BackoffMax {
				backoff = c.cfg.Origin.Retry.BackoffMax
			}
		}
	}

	return nil, fmt.Errorf("origin retry exhausted (attempts); last err: %w", lastErr)
}

// chunkAttrs returns a slog.Attr group identifying the chunk by its
// (origin, bucket, key, index) tuple. Used at every fetch-path log
// callsite for consistent grep / filter syntax across emissions.
// ETag is intentionally not surfaced here - log it via slog.String
// where needed using the chunk.Key's truncated String() form.
func chunkAttrs(k chunk.Key) slog.Attr {
	return slog.Group("chunk",
		slog.String("origin_id", k.OriginID),
		slog.String("bucket", k.Bucket),
		slog.String("key", k.ObjectKey),
		slog.Int64("index", k.Index),
	)
}
