// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package chunkcatalog implements a bounded LRU recording chunks known
// to be present in the CacheStore. Pure hot-path optimization;
// CacheStore is the source of truth.
package chunkcatalog

import (
	"container/list"
	"fmt"
	"sync"
	"time"

	"github.com/Azure/unbounded/internal/orca/cachestore"
	"github.com/Azure/unbounded/internal/orca/chunk"
)

// Catalog is a bounded LRU keyed on chunk.Key.Path().
type Catalog struct {
	mu         sync.Mutex
	maxEntries int
	ll         *list.List
	idx        map[string]*list.Element
}

type entry struct {
	path string
	info cachestore.Info
	at   time.Time
}

// New constructs a Catalog.
func New(maxEntries int) *Catalog {
	if maxEntries <= 0 {
		maxEntries = 100_000
	}

	return &Catalog{
		maxEntries: maxEntries,
		ll:         list.New(),
		idx:        make(map[string]*list.Element, maxEntries),
	}
}

// Lookup returns the cached Info if present and bumps the LRU position.
func (c *Catalog) Lookup(k chunk.Key) (cachestore.Info, bool, error) {
	path := k.Path()

	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.idx[path]
	if !ok {
		return cachestore.Info{}, false, nil
	}

	c.ll.MoveToFront(el)

	e, ok := el.Value.(*entry)
	if !ok {
		return cachestore.Info{}, false, fmt.Errorf("chunkcatalog: list element is not *entry")
	}

	return e.info, true, nil
}

// Record inserts or updates the entry.
func (c *Catalog) Record(k chunk.Key, info cachestore.Info) error {
	path := k.Path()

	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.idx[path]; ok {
		c.ll.MoveToFront(el)

		e, ok := el.Value.(*entry)
		if !ok {
			return fmt.Errorf("chunkcatalog: list element is not *entry")
		}

		e.info = info
		e.at = time.Now()

		return nil
	}

	el := c.ll.PushFront(&entry{path: path, info: info, at: time.Now()})

	c.idx[path] = el
	for c.ll.Len() > c.maxEntries {
		oldest := c.ll.Back()
		if oldest == nil {
			break
		}

		c.ll.Remove(oldest)

		oldEntry, ok := oldest.Value.(*entry)
		if !ok {
			return fmt.Errorf("chunkcatalog: list element is not *entry")
		}

		delete(c.idx, oldEntry.path)
	}

	return nil
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
