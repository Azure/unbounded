// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package chunkcatalog implements a bounded LRU recording chunks known
// to be present in the CacheStore. Pure hot-path optimization;
// CacheStore is the source of truth.
package chunkcatalog

import (
	"container/list"
	"context"
	"log/slog"
	"sync"

	"github.com/Azure/unbounded/internal/orca/cachestore"
	"github.com/Azure/unbounded/internal/orca/chunk"
)

// Catalog is a bounded LRU keyed on chunk.Key.Path().
type Catalog struct {
	mu         sync.Mutex
	maxEntries int
	ll         *list.List
	idx        map[string]*list.Element
	log        *slog.Logger
}

type entry struct {
	path string
	info cachestore.Info
}

// New constructs a Catalog. The log is used at debug level for
// per-call hit / miss / record / forget / evict trace lines via
// slog.LogAttrs so the cost when filtered out (operator runs at
// info or higher) is just the handler's level check. Passing nil
// falls back to slog.Default().
func New(maxEntries int, log *slog.Logger) *Catalog {
	if maxEntries <= 0 {
		maxEntries = 100_000
	}

	if log == nil {
		log = slog.Default()
	}

	return &Catalog{
		maxEntries: maxEntries,
		ll:         list.New(),
		idx:        make(map[string]*list.Element, maxEntries),
		log:        log,
	}
}

// Lookup returns the cached Info if present and bumps the LRU position.
//
// This is the hottest log site in orca: it fires on every chunk read
// attempt. The LogAttrs path ensures attribute-evaluation cost is
// zero when the configured level is above Debug.
func (c *Catalog) Lookup(k chunk.Key) (cachestore.Info, bool) {
	path := k.Path()

	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.idx[path]
	if !ok {
		c.log.LogAttrs(context.Background(), slog.LevelDebug, "chunkcatalog_lookup_miss",
			catalogAttrs(k),
		)

		return cachestore.Info{}, false
	}

	c.ll.MoveToFront(el)

	// The list is private to this package; we control every value
	// inserted (always *entry). The type assertion is safe.
	info := el.Value.(*entry).info //nolint:errcheck // type invariant: list elements are *entry

	c.log.LogAttrs(context.Background(), slog.LevelDebug, "chunkcatalog_lookup_hit",
		catalogAttrs(k),
		slog.Int64("size", info.Size),
	)

	return info, true
}

// Record inserts or updates the entry.
func (c *Catalog) Record(k chunk.Key, info cachestore.Info) {
	path := k.Path()

	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.idx[path]; ok {
		c.ll.MoveToFront(el)

		e := el.Value.(*entry) //nolint:errcheck // type invariant: list elements are *entry
		e.info = info

		c.log.LogAttrs(context.Background(), slog.LevelDebug, "chunkcatalog_record_update",
			catalogAttrs(k),
			slog.Int64("size", info.Size),
		)

		return
	}

	el := c.ll.PushFront(&entry{path: path, info: info})

	c.idx[path] = el

	c.log.LogAttrs(context.Background(), slog.LevelDebug, "chunkcatalog_record_insert",
		catalogAttrs(k),
		slog.Int64("size", info.Size),
	)

	for c.ll.Len() > c.maxEntries {
		oldest := c.ll.Back()
		if oldest == nil {
			break
		}

		c.ll.Remove(oldest)

		oldEntry := oldest.Value.(*entry) //nolint:errcheck // type invariant: list elements are *entry
		delete(c.idx, oldEntry.path)

		c.log.LogAttrs(context.Background(), slog.LevelDebug, "chunkcatalog_evict",
			slog.String("evicted_path", oldEntry.path),
			slog.Int("lru_len", c.ll.Len()),
		)
	}
}

// Forget removes the entry if present.
func (c *Catalog) Forget(k chunk.Key) {
	path := k.Path()

	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.idx[path]; ok {
		c.ll.Remove(el)
		delete(c.idx, path)
		c.log.LogAttrs(context.Background(), slog.LevelDebug, "chunkcatalog_forget",
			catalogAttrs(k),
		)
	}
}

// catalogAttrs renders the chunk's identifying tuple as a slog
// group attribute, matching the 'chunk' taxonomy used by
// fetch.Coordinator emissions so operator queries can grep on a
// single consistent attribute path across packages.
func catalogAttrs(k chunk.Key) slog.Attr {
	return slog.Group("chunk",
		slog.String("origin_id", k.OriginID),
		slog.String("bucket", k.Bucket),
		slog.String("key", k.ObjectKey),
		slog.Int64("index", k.Index),
	)
}
