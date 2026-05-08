// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package metadata is the per-replica object-metadata cache.
//
// Responsibilities:
//   - bounded TTL'd cache of ObjectInfo keyed on (origin_id, bucket,
//     key)
//   - separate negative-TTL handling for 404 / unsupported-blob-type
//     entries (design.md s12)
//   - per-replica HEAD singleflight (s8.7) so concurrent misses
//     collapse to one Origin.Head
package metadata

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Azure/unbounded/internal/orca/config"
	"github.com/Azure/unbounded/internal/orca/origin"
)

// Cache is the per-replica metadata cache.
type Cache struct {
	cfg config.Metadata

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

// NewCache builds a Cache from config.
func NewCache(cfg config.Metadata) *Cache {
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = 10_000
	}

	if cfg.TTL <= 0 {
		cfg.TTL = 5 * time.Minute
	}

	if cfg.NegativeTTL <= 0 {
		cfg.NegativeTTL = 60 * time.Second
	}

	return &Cache{
		cfg: cfg,
		ll:  list.New(),
		idx: make(map[string]*list.Element, cfg.MaxEntries),
	}
}

// Lookup returns the cached ObjectInfo if present and unexpired.
//
// Returns:
//   - info, true,  nil  -> positive cache hit
//   - {}, true,    err  -> negative cache hit (err is the cached error)
//   - {}, false,   nil  -> miss; caller should LookupOrFetch
func (c *Cache) Lookup(originID, bucket, key string) (origin.ObjectInfo, bool, error) {
	k := mkKey(originID, bucket, key)

	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.idx[k]
	if !ok {
		return origin.ObjectInfo{}, false, nil
	}

	e, ok := el.Value.(*cacheEntry)
	if !ok {
		return origin.ObjectInfo{}, false, fmt.Errorf("metadata: list element is not *cacheEntry")
	}

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
func (c *Cache) LookupOrFetch(
	ctx context.Context,
	originID, bucket, key string,
	fetch func(ctx context.Context) (origin.ObjectInfo, error),
) (origin.ObjectInfo, error) {
	if info, ok, err := c.Lookup(originID, bucket, key); ok {
		return info, err
	}

	k := mkKey(originID, bucket, key)
	v, _ := c.sf.LoadOrStore(k, &sfEntry{done: make(chan struct{})})

	sfe, ok := v.(*sfEntry)
	if !ok {
		return origin.ObjectInfo{}, fmt.Errorf("metadata: singleflight value is not *sfEntry")
	}

	first := false

	sfe.once.Do(func() {
		first = true
	})

	if first {
		defer func() {
			close(sfe.done)
			c.sf.Delete(k)
		}()

		info, err := fetch(ctx)
		sfe.info = info
		sfe.err = err

		if recErr := c.recordResult(originID, bucket, key, info, err); recErr != nil {
			err = errors.Join(err, recErr)
		}

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

func (c *Cache) recordResult(originID, bucket, key string, info origin.ObjectInfo, err error) error {
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
			return nil
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

		oldEntry, ok := oldest.Value.(*cacheEntry)
		if !ok {
			return fmt.Errorf("metadata: list element is not *cacheEntry")
		}

		delete(c.idx, oldEntry.key)
	}

	return nil
}

func mkKey(originID, bucket, key string) string {
	return originID + "|" + bucket + "|" + key
}
