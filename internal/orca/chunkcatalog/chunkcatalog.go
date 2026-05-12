// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package chunkcatalog implements a bounded LRU recording chunks known
// to be present in the CacheStore. Pure hot-path optimization;
// CacheStore is the source of truth.
//
// The catalog is presence-only: it tracks whether a chunk's path is
// known to exist in the cachestore. No size or metadata is stored.
// chunk.Path encodes (origin_id, bucket, key, etag, chunk_size), so
// a path hit means the cachestore contains bytes for this exact
// version of this chunk - the path encoding IS the integrity
// statement, and a stale entry whose backing bytes have been deleted
// is self-healing (cachestore.GetChunk returns ErrNotFound, caller
// Forget()s the entry and falls through to the stat path).
package chunkcatalog

import (
	"container/list"
	"context"
	"log/slog"
	"sync"

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

// Lookup reports whether the chunk is known to be present in the
// cachestore. Bumps the LRU position on hit.
//
// This is the hottest log site in orca: it fires on every chunk read
// attempt. The LogAttrs path ensures attribute-evaluation cost is
// zero when the configured level is above Debug.
func (c *Catalog) Lookup(k chunk.Key) bool {
	path := k.Path()

	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.idx[path]
	if !ok {
		c.log.LogAttrs(context.Background(), slog.LevelDebug, "chunkcatalog_lookup_miss",
			catalogAttrs(k),
		)

		return false
	}

	c.ll.MoveToFront(el)

	c.log.LogAttrs(context.Background(), slog.LevelDebug, "chunkcatalog_lookup_hit",
		catalogAttrs(k),
	)

	return true
}

// Record marks the chunk as present.
//
// The 'info' argument is accepted for caller convenience (most call
// sites already have a cachestore.Info from the prior Stat) but is
// not stored. See package docstring for the presence-only rationale.
func (c *Catalog) Record(k chunk.Key) {
	path := k.Path()

	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.idx[path]; ok {
		c.ll.MoveToFront(el)

		c.log.LogAttrs(context.Background(), slog.LevelDebug, "chunkcatalog_record_update",
			catalogAttrs(k),
		)

		return
	}

	el := c.ll.PushFront(&entry{path: path})

	c.idx[path] = el

	c.log.LogAttrs(context.Background(), slog.LevelDebug, "chunkcatalog_record_insert",
		catalogAttrs(k),
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
