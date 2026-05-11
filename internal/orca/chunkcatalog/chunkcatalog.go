// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package chunkcatalog implements a bounded LRU recording chunks known
// to be present in the CacheStore. Pure hot-path optimization;
// CacheStore is the source of truth.
package chunkcatalog

import (
	"container/list"
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
// per-call hit / miss / record / forget / evict trace lines.
// Passing nil falls back to slog.Default().
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
func (c *Catalog) Lookup(k chunk.Key) (cachestore.Info, bool) {
	path := k.Path()

	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.idx[path]
	if !ok {
		return cachestore.Info{}, false
	}

	c.ll.MoveToFront(el)

	// The list is private to this package; we control every value
	// inserted (always *entry). The type assertion is safe.
	return el.Value.(*entry).info, true //nolint:errcheck // type invariant: list elements are *entry
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

		return
	}

	el := c.ll.PushFront(&entry{path: path, info: info})

	c.idx[path] = el
	for c.ll.Len() > c.maxEntries {
		oldest := c.ll.Back()
		if oldest == nil {
			break
		}

		c.ll.Remove(oldest)

		oldEntry := oldest.Value.(*entry) //nolint:errcheck // type invariant: list elements are *entry
		delete(c.idx, oldEntry.path)
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
	}
}
