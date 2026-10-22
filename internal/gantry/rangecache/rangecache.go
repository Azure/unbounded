// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package rangecache stores byte ranges of blobs the node has already fetched.
//
// The containerd content store is all-or-nothing: a blob is either committed
// whole or absent, so a peer cannot serve any part of a 1 GiB layer until it
// holds all of it. Under Artifact Streaming that makes a cold image
// unserveable between peers - every node reads a few KiB of OverlayBD index
// and every one of those reads has to go to origin. This cache holds the
// ranges a node actually fetched so they can be answered locally and served to
// peers long before any layer is complete.
package rangecache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/httprange"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

// span is an inclusive byte interval.
type span struct{ start, end int64 }

// spans is kept sorted and non-overlapping.
type spans []span

func (s spans) covers(start, end int64) bool {
	for _, sp := range s {
		if sp.start <= start && end <= sp.end {
			return true
		}

		if sp.start > start {
			break
		}
	}

	return false
}

// insert adds [start,end] and merges anything it touches or overlaps.
func (s spans) insert(start, end int64) spans {
	out := make(spans, 0, len(s)+1)
	added := false

	for _, sp := range s {
		switch {
		case sp.end+1 < start:
			out = append(out, sp)
		case end+1 < sp.start:
			if !added {
				out = append(out, span{start, end})
				added = true
			}

			out = append(out, sp)
		default:
			if sp.start < start {
				start = sp.start
			}

			if sp.end > end {
				end = sp.end
			}
		}
	}

	if !added {
		out = append(out, span{start, end})
	}

	return out
}

type entry struct {
	mu    sync.Mutex
	file  *os.File
	total int64
	have  spans
}

// Cache is a disk-backed store of partial blob content. It is intentionally
// non-durable: entries are discarded on restart rather than revalidated,
// because a stale range served as authoritative would corrupt an image.
type Cache struct {
	root     string
	maxBytes int64

	mu      sync.Mutex
	entries map[digest.Digest]*entry
	used    int64
	full    bool
}

// New prepares an empty cache rooted at dir. Any previous contents are removed:
// the interval index lives in memory, so leftover files cannot be trusted.
func New(dir string, maxBytes int64) (*Cache, error) {
	if dir == "" {
		return nil, errors.New("rangecache: empty directory")
	}

	if err := os.RemoveAll(dir); err != nil {
		return nil, fmt.Errorf("rangecache: reset %s: %w", dir, err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("rangecache: create %s: %w", dir, err)
	}

	return &Cache{root: dir, maxBytes: maxBytes, entries: map[digest.Digest]*entry{}}, nil
}

func (c *Cache) path(d digest.Digest) string {
	return filepath.Join(c.root, strings.ReplaceAll(d.String(), ":", "_"))
}

// lookup returns the existing entry for d, or nil.
func (c *Cache) lookup(d digest.Digest) *entry {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.entries[d]
}

// OpenRange satisfies streaming.LocalRangeStore. It returns ifaces.ErrNotFound
// unless the whole requested interval is cached, so a partial hit never
// produces a short body.
func (c *Cache) OpenRange(_ context.Context, d digest.Digest, requested httprange.Range) (io.ReadCloser, int64, error) {
	e := c.lookup(d)
	if e == nil {
		return nil, -1, &ifaces.ErrNotFound{Digest: d}
	}

	e.mu.Lock()
	total, covered := e.total, e.have.covers(requested.Start, requested.End)
	e.mu.Unlock()

	if !covered {
		return nil, -1, &ifaces.ErrNotFound{Digest: d}
	}

	return io.NopCloser(io.NewSectionReader(e.file, requested.Start, requested.Length())), total, nil
}

// Has reports whether the full interval is already cached.
func (c *Cache) Has(d digest.Digest, requested httprange.Range) bool {
	e := c.lookup(d)
	if e == nil {
		return false
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	return e.have.covers(requested.Start, requested.End)
}

// HasAny reports whether any range of d is cached, which is enough to make
// this node a useful provider for it.
func (c *Cache) HasAny(d digest.Digest) bool {
	e := c.lookup(d)
	if e == nil {
		return false
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	return len(e.have) > 0
}

// Put stores data read from r at start. total is the blob's full size, used for
// Content-Range on later reads. Put reports the number of bytes stored; a
// truncated read still records what landed.
func (c *Cache) Put(d digest.Digest, total, start int64, r io.Reader) (int64, error) {
	if start < 0 {
		return 0, fmt.Errorf("rangecache: negative offset %d", start)
	}

	e, err := c.entryFor(d)
	if err != nil {
		return 0, err
	}

	if e == nil {
		return 0, nil // cache is full; caller still gets its bytes from r
	}

	written, err := copyAt(e.file, r, start)
	if written > 0 {
		e.mu.Lock()
		e.have = e.have.insert(start, start+written-1)

		if total > 0 {
			e.total = total
		}

		e.mu.Unlock()

		c.mu.Lock()
		c.used += written
		c.mu.Unlock()
	}

	return written, err
}

// entryFor returns the entry for d, creating it when there is room. A nil
// entry with a nil error means the cache is at capacity.
func (c *Cache) entryFor(d digest.Digest) (*entry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if e, ok := c.entries[d]; ok {
		return e, nil
	}

	if c.maxBytes > 0 && c.used >= c.maxBytes {
		c.full = true
		return nil, nil
	}

	f, err := os.OpenFile(c.path(d), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("rangecache: open %s: %w", d, err)
	}

	e := &entry{file: f, total: -1}
	c.entries[d] = e

	return e, nil
}

// Full reports whether the cache stopped admitting new blobs.
func (c *Cache) Full() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.full
}

// Digests lists blobs with at least one cached range.
func (c *Cache) Digests() []digest.Digest {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]digest.Digest, 0, len(c.entries))
	for d := range c.entries {
		out = append(out, d)
	}

	return out
}

// Close releases the open files.
func (c *Cache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, e := range c.entries {
		_ = e.file.Close() //nolint:errcheck // best-effort close
	}

	c.entries = map[digest.Digest]*entry{}

	return nil
}

const copyChunk = 1 << 20

func copyAt(f *os.File, r io.Reader, start int64) (int64, error) {
	buf := make([]byte, copyChunk)
	offset := start
	total := int64(0)

	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			if _, err := f.WriteAt(buf[:n], offset); err != nil {
				return total, fmt.Errorf("rangecache: write at %d: %w", offset, err)
			}

			offset += int64(n)
			total += int64(n)
		}

		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return total, nil
			}

			return total, readErr
		}
	}
}
