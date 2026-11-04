// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package imagepull

import (
	"archive/tar"
	"container/list"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/opencontainers/go-digest"
	"golang.org/x/sync/singleflight"
)

// entryKind is the type of an indexed tar entry.
type entryKind uint8

const (
	kindReg entryKind = iota
	kindDir
	kindSymlink
	kindHardlink
	kindOther
)

// indexEntry is the metadata of one path in a layer.
type indexEntry struct {
	kind entryKind
	// link is the raw symlink target, or the cleaned hard link target.
	link string
	// resolved reports that a hard link was resolved to an earlier regular
	// file in the same layer; entry, ordinal, size and sum then describe
	// that file.
	resolved bool
	// entry is the cleaned tar name of the entry holding the content.
	entry string
	// ordinal is the position of that entry in the layer tar stream.
	ordinal int
	size    int64
	mode    int64
	modTime time.Time
	// sum is the sha256 of the regular file content.
	sum digest.Digest
}

// layerIndex records every entry of one layer. It is built from a full,
// digest-verified read of the layer and is immutable afterwards.
type layerIndex struct {
	entries map[string]*indexEntry
	// dirs holds every parent directory of an entry, whether or not the
	// directory has its own tar entry.
	dirs    map[string]bool
	deleted map[string]bool // whiteouts
	opaque  map[string]bool // opaque directories
}

// hides reports whether this layer hides path p in lower layers, through a
// whiteout of p or of a parent, an opaque parent, or a non-directory entry
// at a parent path.
func (x *layerIndex) hides(p string) bool {
	if x.deleted[p] {
		return true
	}

	for d := p; d != "." && d != ""; {
		d = path.Dir(d)

		if x.deleted[d] || x.opaque[d] {
			return true
		}

		if e := x.entries[d]; e != nil && e.kind != kindDir {
			return true
		}
	}

	return false
}

// buildIndex reads layer li to EOF, verifying the layer digest, and records
// every entry. If the layer fails verification after a mirror served any
// of it, the partial index is discarded and the layer is read again from
// the upstream registry, starting at offset zero.
func (i *Image) buildIndex(ctx context.Context, li int) (*layerIndex, error) {
	x, mirrored, err := i.buildIndexOnce(ctx, li)
	if err != nil && mirrored && errors.Is(err, ErrDigestMismatch) && i.mirrorAvoided(i.layers[li].Digest) {
		x, _, err = i.buildIndexOnce(ctx, li)
	}

	return x, err
}

// buildIndexOnce is one buildIndex attempt. mirrored reports whether a
// mirror served any bytes of the layer.
func (i *Image) buildIndexOnce(ctx context.Context, li int) (x *layerIndex, mirrored bool, err error) {
	desc := i.layers[li]

	ls, err := i.openLayer(ctx, li)
	if err != nil {
		return nil, false, err
	}
	defer ls.Close() //nolint:errcheck // read-only stream
	defer func() { mirrored = ls.blob.fromMirror() }()

	x = &layerIndex{entries: map[string]*indexEntry{}, dirs: map[string]bool{}, deleted: map[string]bool{}, opaque: map[string]bool{}}

	for ordinal := 0; ; ordinal++ {
		hdr, err := ls.tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return nil, false, fmt.Errorf("read layer %s: %w", desc.Digest, err)
		}

		name := cleanName(hdr.Name)
		dir, base := path.Split(name)

		dir = cleanName(dir)
		if dir == "" {
			dir = "."
		}

		if base == whiteoutOpaque {
			x.opaque[dir] = true
			continue
		}

		if strings.HasPrefix(base, whiteoutPrefix) {
			x.deleted[cleanName(path.Join(dir, strings.TrimPrefix(base, whiteoutPrefix)))] = true
			continue
		}

		e := &indexEntry{entry: name, ordinal: ordinal, size: hdr.Size, mode: hdr.Mode, modTime: hdr.ModTime}

		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeRegA: //nolint:staticcheck // TypeRegA appears in old archives
			e.kind = kindReg

			h := sha256.New()
			if _, err := io.Copy(h, ls.tr); err != nil {
				return nil, false, fmt.Errorf("read layer %s: %w", desc.Digest, err)
			}

			e.sum = digest.NewDigest(digest.SHA256, h)
		case tar.TypeDir:
			e.kind = kindDir
		case tar.TypeSymlink:
			e.kind = kindSymlink
			e.link = hdr.Linkname
		case tar.TypeLink:
			e.kind = kindHardlink
			e.link = cleanName(hdr.Linkname)

			// A hard link refers to an earlier entry in the same layer.
			if t := x.entries[e.link]; t != nil && (t.kind == kindReg || (t.kind == kindHardlink && t.resolved)) {
				e.resolved = true
				e.entry, e.ordinal, e.size, e.sum = t.entry, t.ordinal, t.size, t.sum
			}
		default:
			e.kind = kindOther
		}

		x.entries[name] = e

		for d := path.Dir(name); d != "." && !x.dirs[d]; d = path.Dir(d) {
			x.dirs[d] = true
		}
	}

	// Read the rest of the stream so that the blob digest is verified. The
	// index is only kept when that succeeds.
	if err := ls.drain(); err != nil {
		return nil, false, fmt.Errorf("read layer %s: %w", desc.Digest, err)
	}

	return x, false, nil
}

// indexCache is a bounded LRU of layer indexes keyed by layer digest.
// Layers are content-addressed and immutable, so entries never expire.
type indexCache struct {
	mu    sync.Mutex
	max   int
	ll    *list.List
	items map[digest.Digest]*list.Element
	sf    singleflight.Group
}

type indexItem struct {
	key digest.Digest
	idx *layerIndex
}

func newIndexCache(maxItems int) *indexCache {
	return &indexCache{max: maxItems, ll: list.New(), items: map[digest.Digest]*list.Element{}}
}

func (c *indexCache) get(d digest.Digest) (*layerIndex, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.items[d]
	if !ok {
		return nil, false
	}

	c.ll.MoveToFront(el)

	return el.Value.(*indexItem).idx, true //nolint:errcheck,forcetypeassert // only indexItem is stored
}

func (c *indexCache) add(d digest.Digest, idx *layerIndex) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.items[d]; ok {
		c.ll.MoveToFront(el)
		return
	}

	c.items[d] = c.ll.PushFront(&indexItem{key: d, idx: idx})

	for c.ll.Len() > c.max {
		el := c.ll.Back()
		c.ll.Remove(el)
		delete(c.items, el.Value.(*indexItem).key) //nolint:errcheck,forcetypeassert // only indexItem is stored
	}
}

func (c *indexCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.ll.Len()
}

// load returns the index for d, building it with build at most once at a
// time per digest.
//
// The build runs with the context of the caller that started it (the
// leader). Every caller, leader or waiter, stops waiting as soon as its own
// ctx is done and returns ctx's error; the build keeps running in the
// singleflight goroutine and its result is still cached and delivered to the
// remaining waiters, so no goroutine is leaked. If a waiter receives a
// cancellation error from a build it did not run (the leader's context was
// canceled), and its own ctx is still live, it retries and becomes the new
// leader. A caller whose own build failed returns that error unchanged.
func (c *indexCache) load(ctx context.Context, d digest.Digest, build func(context.Context) (*layerIndex, error)) (*layerIndex, error) {
	for {
		if idx, ok := c.get(d); ok {
			return idx, nil
		}

		// ran is written by the singleflight goroutine before the result
		// is sent on ch, so reading it after receiving is race free.
		ran := false
		ch := c.sf.DoChan(string(d), func() (any, error) {
			ran = true

			if idx, ok := c.get(d); ok {
				return idx, nil
			}

			idx, err := build(ctx)
			if err != nil {
				return nil, err
			}

			c.add(d, idx)

			return idx, nil
		})

		var res singleflight.Result

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case res = <-ch:
		}

		if res.Err == nil {
			return res.Val.(*layerIndex), nil //nolint:errcheck,forcetypeassert // only *layerIndex is returned
		}

		if !ran && ctx.Err() == nil && (errors.Is(res.Err, context.Canceled) || errors.Is(res.Err, context.DeadlineExceeded)) {
			continue
		}

		return nil, res.Err
	}
}

func (i *Image) layerIndex(ctx context.Context, li int) (*layerIndex, error) {
	return i.puller.state().indexes.load(ctx, i.layers[li].Digest, func(ctx context.Context) (*layerIndex, error) {
		return i.buildIndex(ctx, li)
	})
}
