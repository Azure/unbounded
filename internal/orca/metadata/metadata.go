// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package metadata is the per-replica object-metadata cache.
//
// Responsibilities:
//   - bounded TTL'd cache of ObjectInfo keyed on (origin_id, bucket,
//     key)
//   - separate negative-TTL handling for 404 / unsupported-blob-type
//     entries
//   - per-replica HEAD singleflight so concurrent misses collapse to
//     one Origin.Head
package metadata

import (
	"container/list"
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Azure/unbounded/internal/orca/config"
	"github.com/Azure/unbounded/internal/orca/origin"
)

// Cache is the per-replica metadata cache.
type Cache struct {
	cfg config.Metadata
	log *slog.Logger

	mu  sync.Mutex
	ll  *list.List
	idx map[string]*list.Element

	sf sync.Map // map[string]*sfEntry
}

type cacheEntry struct {
	key       string
	info      origin.ObjectInfo
	negative  bool
	negErr    error
	expiresAt time.Time
}

type sfEntry struct {
	once sync.Once
	done chan struct{}
	info origin.ObjectInfo
	err  error
}

// NewCache builds a Cache from config. The log is used at debug
// level for cache hit / miss / record / invalidate trace lines and
// at warn level for unexpected backend errors caught during result
// recording. Passing nil falls back to slog.Default().
func NewCache(cfg config.Metadata, log *slog.Logger) *Cache {
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = 10_000
	}

	if cfg.TTL <= 0 {
		cfg.TTL = 5 * time.Minute
	}

	if cfg.NegativeTTL <= 0 {
		cfg.NegativeTTL = 60 * time.Second
	}

	if log == nil {
		log = slog.Default()
	}

	return &Cache{
		cfg: cfg,
		log: log,
		ll:  list.New(),
		idx: make(map[string]*list.Element, cfg.MaxEntries),
	}
}

// lookup returns the cached ObjectInfo if present and unexpired.
//
// Returns:
//   - info, true,  nil  -> positive cache hit
//   - {}, true,    err  -> negative cache hit (err is the cached error)
//   - {}, false,   nil  -> miss; caller should LookupOrFetch
func (c *Cache) lookup(originID, bucket, key string) (origin.ObjectInfo, bool, error) {
	k := mkKey(originID, bucket, key)

	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.idx[k]
	if !ok {
		return origin.ObjectInfo{}, false, nil
	}

	// The list is private; we control every value inserted (always
	// *cacheEntry). The type assertion is safe.
	e := el.Value.(*cacheEntry) //nolint:errcheck // type invariant: list elements are *cacheEntry

	if time.Now().After(e.expiresAt) {
		c.ll.Remove(el)
		delete(c.idx, k)

		return origin.ObjectInfo{}, false, nil
	}

	c.ll.MoveToFront(el)

	if e.negative {
		return origin.ObjectInfo{}, true, e.negErr
	}

	return e.info, true, nil
}

// LookupOrFetch returns the cached ObjectInfo on hit (positive or
// negative); on miss, runs the per-replica HEAD singleflight against
// fetch and caches the result with the appropriate TTL.
//
// Singleflight tradeoff: the first caller (leader) drives fetch with
// its own ctx. If the leader's ctx is cancelled mid-fetch, joiners
// observe the leader's resulting ctx-error rather than their own
// (still-valid) ctx. This is the standard singleflight contract; a
// joiner can re-issue after seeing ctx.Err on a closed sfe.done if
// it wants to drive its own attempt.
func (c *Cache) LookupOrFetch(
	ctx context.Context,
	originID, bucket, key string,
	fetch func(ctx context.Context) (origin.ObjectInfo, error),
) (origin.ObjectInfo, error) {
	if info, ok, err := c.lookup(originID, bucket, key); ok {
		return info, err
	}

	k := mkKey(originID, bucket, key)
	v, _ := c.sf.LoadOrStore(k, &sfEntry{done: make(chan struct{})})

	// The sync.Map only ever holds *sfEntry; the type assertion is safe.
	sfe := v.(*sfEntry) //nolint:errcheck // type invariant: sf map values are *sfEntry

	first := false

	sfe.once.Do(func() {
		first = true
	})

	if first {
		// Delete the singleflight entry before closing done so a new
		// caller arriving after Delete creates a fresh entry instead
		// of silently replaying our (possibly transient-error) result.
		// Existing joiners already loaded the old pointer and read the
		// result via the closed done. The brief window between Delete
		// and close where a new caller starts a concurrent fetch is
		// benign: the new fetch either confirms or supersedes our
		// result.
		defer func() {
			c.sf.Delete(k)
			close(sfe.done)
		}()

		info, err := fetch(ctx)
		sfe.info = info
		sfe.err = err

		c.recordResult(originID, bucket, key, info, err)

		return info, err
	}
	// Joiner: wait for the leader.
	select {
	case <-ctx.Done():
		return origin.ObjectInfo{}, ctx.Err()
	case <-sfe.done:
	}

	return sfe.info, sfe.err
}

// Invalidate drops the entry.
func (c *Cache) Invalidate(originID, bucket, key string) {
	k := mkKey(originID, bucket, key)

	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.idx[k]; ok {
		c.ll.Remove(el)
		delete(c.idx, k)
	}
}

func (c *Cache) recordResult(originID, bucket, key string, info origin.ObjectInfo, err error) {
	k := mkKey(originID, bucket, key)

	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()

	var e *cacheEntry

	switch {
	case err == nil:
		e = &cacheEntry{key: k, info: info, expiresAt: now.Add(c.cfg.TTL)}
	case errors.Is(err, origin.ErrNotFound):
		e = &cacheEntry{key: k, negative: true, negErr: err, expiresAt: now.Add(c.cfg.NegativeTTL)}
	default:
		var ube *origin.UnsupportedBlobTypeError
		if errors.As(err, &ube) {
			e = &cacheEntry{key: k, negative: true, negErr: err, expiresAt: now.Add(c.cfg.NegativeTTL)}
		} else {
			// Other transient errors not cached.
			return
		}
	}

	if existing, ok := c.idx[k]; ok {
		c.ll.Remove(existing)
		delete(c.idx, k)
	}

	el := c.ll.PushFront(e)

	c.idx[k] = el
	for c.ll.Len() > c.cfg.MaxEntries {
		oldest := c.ll.Back()
		if oldest == nil {
			break
		}

		c.ll.Remove(oldest)

		oldEntry := oldest.Value.(*cacheEntry) //nolint:errcheck // type invariant: list elements are *cacheEntry
		delete(c.idx, oldEntry.key)
	}
}

// mkKey builds an in-memory cache key from (originID, bucket, key).
// The encoding is length-prefixed: each field is written as an
// 8-byte little-endian length followed by the field bytes. This
// guarantees that two distinct triples cannot collide on the
// rendered key. A naive 'origin|bucket|key' concatenation would
// alias e.g. (origin="a|b", bucket="c", key="d") and
// (origin="a", bucket="b|c", key="d") because S3 object keys may
// legally contain '|'. The cache is purely in-memory so this
// encoding has no on-disk compatibility implications.
func mkKey(originID, bucket, key string) string {
	var b strings.Builder

	b.Grow(24 + len(originID) + len(bucket) + len(key))
	writeLP(&b, originID)
	writeLP(&b, bucket)
	writeLP(&b, key)

	return b.String()
}

func writeLP(b *strings.Builder, s string) {
	var lenBuf [8]byte

	binary.LittleEndian.PutUint64(lenBuf[:], uint64(len(s)))
	b.Write(lenBuf[:])
	b.WriteString(s)
}
