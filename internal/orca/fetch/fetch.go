// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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
// peer-fallback warnings and commit-after-serve failure traces; the
// caller (usually app.Start) injects the app-wide slog.Logger so
// fetch-path logs are unified with the rest of the runtime's output.
// Passing nil falls back to slog.Default().
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

// Origin returns the underlying origin (used by the LIST passthrough).
func (c *Coordinator) Origin() origin.Origin { return c.or }

// HeadObject returns object metadata, satisfying client HEAD requests.
func (c *Coordinator) HeadObject(ctx context.Context, bucket, key string) (origin.ObjectInfo, error) {
	return c.mc.LookupOrFetch(ctx, c.cfg.Origin.ID, bucket, key,
		func(ctx context.Context) (origin.ObjectInfo, error) {
			return c.or.Head(ctx, bucket, key)
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
//     atomic commit to CacheStore, populate buffer for joiners).
//   - If a peer is the coordinator: send /internal/fill to that peer;
//     stream from peer's response. On 409 Conflict, fall back to local
//     fill.
func (c *Coordinator) GetChunk(ctx context.Context, k chunk.Key, objectSize int64) (io.ReadCloser, error) {
	if rc, hit, err := c.lookupOrStat(ctx, k, objectSize); err != nil {
		return nil, err
	} else if hit {
		return rc, nil
	}

	// Cluster-wide dedup: route to coordinator.
	coord := c.cl.Coordinator(k)
	if !coord.Self {
		rc, err := c.cl.FillFromPeer(ctx, coord, k, objectSize)
		if err == nil {
			return rc, nil
		}

		if errors.Is(err, cluster.ErrPeerNotCoordinator) {
			c.log.Warn("peer reported not-coordinator; falling back to local fill",
				"chunk", k.String(), "peer", coord.IP)
			// fall through to local fill
		} else {
			c.log.Warn("internal-fill RPC failed; falling back to local fill",
				"chunk", k.String(), "peer", coord.IP, "err", err)
		}
	}

	return c.fillLocal(ctx, k, objectSize)
}

// FillForPeer is the path taken by the /internal/fill handler.
//
// The receiver becomes the leader for this fill (or joins an in-flight
// fill for the same key). Returns a streaming body of the entire chunk.
func (c *Coordinator) FillForPeer(ctx context.Context, k chunk.Key, objectSize int64) (io.ReadCloser, error) {
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

	if _, ok := c.cat.Lookup(k); ok {
		rc, err := c.cs.GetChunk(ctx, k, 0, expected)
		if err == nil {
			return rc, true, nil
		}

		if errors.Is(err, cachestore.ErrNotFound) {
			c.cat.Forget(k)
			// fall through to stat
		} else {
			return nil, false, err
		}
	}

	info, err := c.cs.Stat(ctx, k)
	if err != nil {
		if errors.Is(err, cachestore.ErrNotFound) {
			return nil, false, nil
		}

		return nil, false, err
	}

	c.cat.Record(k, info)

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

		go c.runFill(k, objectSize, f)
	} else {
		c.mu.Unlock()
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
	// caller's) so it can complete the cachestore commit-after-serve
	// step even if the originating client disconnects mid-stream.
	// The 5-minute ceiling bounds the cost: a fill no joiner ever
	// reads still releases its origin-semaphore slot and clears its
	// inflight entry within the budget. Peak per-fill heap is one
	// ChunkSize bytes.Buffer (8 MiB default). Without metrics this
	// cost is invisible; revisit if production telemetry shows
	// cancelled-by-client storms.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	defer func() {
		close(f.done)
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

	// expectedLen is the authoritative number of bytes we should
	// receive from origin: ChunkSize for non-tail chunks, the
	// remainder for the tail. We request at most expectedLen and
	// reject responses that don't match.
	expectedLen := k.ExpectedLen(objectSize)
	off := k.Index * k.ChunkSize

	requestLen := expectedLen
	if requestLen == 0 {
		// Fallback when objectSize is unknown: request the full chunk
		// size; the validation below cannot distinguish a legitimate
		// short tail from a flaky-origin short read, so the caller is
		// trusting the origin in this mode.
		requestLen = k.ChunkSize
	}

	body, err := c.fetchWithRetry(ctx, k, off, requestLen)
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

	if expectedLen > 0 && int64(buf.Len()) != expectedLen {
		f.err = fmt.Errorf("origin returned %d bytes, expected %d (chunk=%s)",
			buf.Len(), expectedLen, k.String())

		return
	}

	f.bodyBuf = buf

	// Atomic commit to CacheStore.
	commitErr := c.cs.PutChunk(ctx, k, int64(buf.Len()), bytes.NewReader(buf.Bytes()))
	if commitErr == nil {
		c.cat.Record(k, cachestore.Info{Size: int64(buf.Len()), Committed: time.Now()})
	} else if errors.Is(commitErr, cachestore.ErrCommitLost) {
		// Another replica won; treat existing CacheStore entry as truth.
		if info, err := c.cs.Stat(ctx, k); err == nil {
			c.cat.Record(k, info)
		}
	} else {
		c.log.Warn("commit-after-serve failed",
			"chunk", k.String(), "err", commitErr)
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

		body, err := c.or.GetRange(ctx, k.Bucket, k.ObjectKey, k.ETag, off, length)
		if err == nil {
			return body, nil
		}

		lastErr = err
		// Non-retryable: ETag changed.
		var etagChanged *origin.OriginETagChangedError
		if errors.As(err, &etagChanged) {
			c.mc.Invalidate(c.cfg.Origin.ID, k.Bucket, k.ObjectKey)
			return nil, err
		}
		// Non-retryable: not found.
		if errors.Is(err, origin.ErrNotFound) {
			return nil, err
		}
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
