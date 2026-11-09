// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package meshfetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ContentSource reads blobs from the node's containerd content store, the
// only on-disk copy of image data. meshfetch never writes blob bytes to
// disk itself: containerd writes them while it pulls through the mirror,
// and meshfetch reads them back to serve peers.
type ContentSource interface {
	// Available returns how many leading bytes of blob d ("sha256:<hex>")
	// can be read now and the blob size, or -1 when the size is unknown.
	// avail is 0 when the store has no copy.
	Available(d string) (avail, total int64)
	// ReadAt reads exactly len(p) bytes of d at off. A short read is an
	// error.
	ReadAt(d string, p []byte, off int64) (int, error)
}

// errContentShort reports a read of local content that ended early: the
// blob was deleted, or an ingest was restarted.
var errContentShort = errors.New("meshfetch: local content shorter than expected")

// DirContentOptions configures a DirContent.
type DirContentOptions struct {
	// Root is the containerd content root, the directory that holds
	// blobs/ and ingest/ (for example
	// /var/lib/containerd/io.containerd.content.v1.content). Required.
	Root string
	// IdleClose closes a file descriptor unused for this long, so a blob
	// that containerd deletes does not keep its space. Zero uses 2s.
	IdleClose time.Duration
	// ScanInterval bounds how often the ingest directory is listed to find
	// in-progress downloads. Zero uses 100ms.
	ScanInterval time.Duration
	Logger       *slog.Logger
}

// DirContent reads a containerd content store directly from its directory
// (typically a read-only hostPath mount). Committed blobs live at
// blobs/sha256/<hex>; a blob being downloaded lives at ingest/<key>/data,
// where ingest/<key>/ref names its digest and ingest/<key>/total its size.
// containerd writes ingests sequentially and renames data into blobs/ on
// commit, so an open descriptor stays valid across the commit. Reads go
// through the kernel page cache, which usually still holds bytes
// containerd has just written.
type DirContent struct {
	opts DirContentOptions
	log  *slog.Logger

	mu       sync.Mutex
	files    map[string]*openBlob
	ingests  map[string]ingestEntry
	lastScan time.Time
	closed   bool

	disabled atomic.Bool
	reads    atomic.Int64
	stop     chan struct{}
	done     chan struct{}
}

type openBlob struct {
	f       *os.File
	refs    int
	lastUse time.Time
}

type ingestEntry struct {
	dir   string
	total int64
}

// NewDirContent returns a DirContent for opts.Root. It returns an error
// when the root cannot be read, so the caller can fall back to another
// source.
func NewDirContent(opts DirContentOptions) (*DirContent, error) {
	if opts.Root == "" {
		return nil, errors.New("meshfetch: content root is required")
	}

	if opts.IdleClose <= 0 {
		opts.IdleClose = 2 * time.Second
	}

	if opts.ScanInterval <= 0 {
		opts.ScanInterval = 100 * time.Millisecond
	}

	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	if _, err := os.ReadDir(filepath.Join(opts.Root, "blobs", "sha256")); err != nil {
		return nil, fmt.Errorf("meshfetch: content root %s: %w", opts.Root, err)
	}

	c := &DirContent{
		opts:    opts,
		log:     opts.Logger,
		files:   make(map[string]*openBlob),
		ingests: make(map[string]ingestEntry),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}

	go c.janitor()

	return c, nil
}

// Close closes every descriptor and stops the janitor.
func (c *DirContent) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}

	c.closed = true
	close(c.stop)
	c.mu.Unlock()

	<-c.done

	c.mu.Lock()
	defer c.mu.Unlock()

	for h, b := range c.files {
		// A descriptor in use is closed by its last user (release).
		delete(c.files, h)

		if b.refs == 0 {
			_ = b.f.Close() //nolint:errcheck // read-only descriptor
		}
	}
}

// Reads returns the number of ReadAt calls served.
func (c *DirContent) Reads() int64 { return c.reads.Load() }

// hexOf returns the hex of a canonical sha256 digest, or "" when d is not
// one (which also keeps d from naming a path outside the root).
func hexOf(d string) string {
	h, ok := strings.CutPrefix(d, "sha256:")
	if !ok || len(h) != 64 {
		return ""
	}

	for i := range len(h) {
		ch := h[i]
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return ""
		}
	}

	return h
}

func (c *DirContent) blobPath(h string) string {
	return filepath.Join(c.opts.Root, "blobs", "sha256", h)
}

// fail disables the source on a permission error, logging once. Other
// errors are left to the caller.
func (c *DirContent) fail(err error) {
	if errors.Is(err, fs.ErrPermission) && c.disabled.CompareAndSwap(false, true) {
		c.log.Warn("cachemesh cannot read the containerd content store; serving peers from memory only",
			slog.String("root", c.opts.Root), slog.Any("err", err))
	}
}

// Available implements ContentSource.
func (c *DirContent) Available(d string) (avail, total int64) {
	h := hexOf(d)
	if h == "" || c.disabled.Load() {
		return 0, -1
	}

	fi, err := os.Stat(c.blobPath(h))
	if err == nil {
		return fi.Size(), fi.Size()
	}

	if !errors.Is(err, fs.ErrNotExist) {
		c.fail(err)
		return 0, -1
	}

	e, ok := c.ingest(h, false)
	if !ok {
		return 0, -1
	}

	fi, err = os.Stat(filepath.Join(e.dir, "data"))
	if err != nil {
		// Committed (renamed) or aborted since the scan: look again.
		if e, ok = c.ingest(h, true); !ok {
			if fi, err = os.Stat(c.blobPath(h)); err == nil {
				return fi.Size(), fi.Size()
			}

			return 0, -1
		}

		if fi, err = os.Stat(filepath.Join(e.dir, "data")); err != nil {
			return 0, -1
		}
	}

	size := fi.Size()
	if e.total > 0 {
		size = min(size, e.total)
	}

	return size, e.total
}

// ingest returns the ingest of blob h. It lists the ingest directory when
// the cached listing has no entry for h (or force is set) and the last
// listing is older than ScanInterval.
func (c *DirContent) ingest(h string, force bool) (ingestEntry, bool) {
	c.mu.Lock()
	e, ok := c.ingests[h]

	if ok && !force {
		c.mu.Unlock()
		return e, true
	}

	if time.Since(c.lastScan) < c.opts.ScanInterval {
		c.mu.Unlock()
		return e, ok && !force
	}

	c.lastScan = time.Now()
	c.mu.Unlock()

	m := c.scan()

	c.mu.Lock()
	c.ingests = m
	e, ok = m[h]
	c.mu.Unlock()

	return e, ok
}

// scan lists in-progress ingests by blob hex.
func (c *DirContent) scan() map[string]ingestEntry {
	dir := filepath.Join(c.opts.Root, "ingest")
	m := make(map[string]ingestEntry)

	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			c.fail(err)
		}

		return m
	}

	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}

		p := filepath.Join(dir, ent.Name())

		ref, err := os.ReadFile(filepath.Join(p, "ref"))
		if err != nil {
			continue
		}

		h := refHex(string(ref))
		if h == "" {
			continue
		}

		total := int64(-1)

		if b, err := os.ReadFile(filepath.Join(p, "total")); err == nil {
			if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil && n > 0 {
				total = n
			}
		}

		m[h] = ingestEntry{dir: p, total: total}
	}

	return m
}

// refHex extracts the blob hex from an ingest ref such as
// "layer-sha256:<hex>" (CRI) or "<prefix>sha256:<hex>".
func refHex(ref string) string {
	i := strings.LastIndex(ref, "sha256:")
	if i < 0 {
		return ""
	}

	return hexOf(strings.TrimSpace(ref[i:]))
}

// ReadAt implements ContentSource.
func (c *DirContent) ReadAt(d string, p []byte, off int64) (int, error) {
	h := hexOf(d)
	if h == "" {
		return 0, fmt.Errorf("meshfetch: bad digest %q", d)
	}

	b, err := c.acquire(h)
	if err != nil {
		return 0, err
	}

	n, err := b.f.ReadAt(p, off)
	short := n < len(p)

	// A short read may come from a truncated ingest: drop the descriptor.
	c.release(h, b, short)
	c.reads.Add(1)

	if short {
		if err == nil || errors.Is(err, io.EOF) {
			err = errContentShort
		}

		return n, err
	}

	return n, nil
}

// release drops a reference to b, removing it from the cache when drop is
// set. A descriptor no longer cached is closed by its last user.
func (c *DirContent) release(h string, b *openBlob, drop bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	b.refs--
	b.lastUse = time.Now()

	if drop && c.files[h] == b {
		delete(c.files, h)
	}

	if c.files[h] != b && b.refs == 0 {
		_ = b.f.Close() //nolint:errcheck // read-only descriptor
	}
}

// acquire returns an open descriptor of blob h with a reference.
func (c *DirContent) acquire(h string) (*openBlob, error) {
	c.mu.Lock()
	if b := c.files[h]; b != nil {
		b.refs++
		c.mu.Unlock()

		return b, nil
	}
	c.mu.Unlock()

	f, err := os.Open(c.blobPath(h))
	if errors.Is(err, fs.ErrNotExist) {
		if e, ok := c.ingest(h, false); ok {
			f, err = os.Open(filepath.Join(e.dir, "data"))
		}

		if errors.Is(err, fs.ErrNotExist) {
			// Committed between the two opens.
			f, err = os.Open(c.blobPath(h))
		}
	}

	if err != nil {
		c.fail(err)
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if b := c.files[h]; b != nil {
		_ = f.Close() //nolint:errcheck // read-only descriptor
		b.refs++

		return b, nil
	}

	if c.closed {
		_ = f.Close() //nolint:errcheck // read-only descriptor
		return nil, errClosed
	}

	b := &openBlob{f: f, refs: 1}
	c.files[h] = b

	return b, nil
}

// janitor closes descriptors idle for IdleClose.
func (c *DirContent) janitor() {
	defer close(c.done)

	t := time.NewTicker(max(c.opts.IdleClose/4, 10*time.Millisecond))
	defer t.Stop()

	for {
		select {
		case <-c.stop:
			return
		case now := <-t.C:
			c.closeIdle(now.Add(-c.opts.IdleClose))
		}
	}
}

func (c *DirContent) closeIdle(cutoff time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for h, b := range c.files {
		if b.refs == 0 && b.lastUse.Before(cutoff) {
			_ = b.f.Close() //nolint:errcheck // read-only descriptor

			delete(c.files, h)
		}
	}
}

// openFiles returns the number of cached descriptors (tests).
func (c *DirContent) openFiles() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.files)
}

// BlobOpener opens committed blobs, for example over the containerd
// content API.
type BlobOpener interface {
	Open(ctx context.Context, d string) (io.ReaderAt, int64, io.Closer, error)
}

// OpenerContent is a ContentSource over a BlobOpener for committed blobs
// only. It is the fallback when the content root is not mounted: every
// ReadAt opens the blob, so callers should read whole pages.
type OpenerContent struct {
	Opener  BlobOpener
	Timeout time.Duration

	mu    sync.Mutex
	sizes map[string]sizeEntry
}

type sizeEntry struct {
	size int64
	at   time.Time
}

// sizeTTL bounds how long OpenerContent trusts a looked-up size, absent or
// present (blobs can be garbage collected).
const sizeTTL = time.Second

func (o *OpenerContent) ctx() (context.Context, context.CancelFunc) {
	t := o.Timeout
	if t <= 0 {
		t = 10 * time.Second
	}

	return context.WithTimeout(context.Background(), t)
}

// Available implements ContentSource.
func (o *OpenerContent) Available(d string) (avail, total int64) {
	o.mu.Lock()
	if e, ok := o.sizes[d]; ok && time.Since(e.at) < sizeTTL {
		o.mu.Unlock()

		if e.size < 0 {
			return 0, -1
		}

		return e.size, e.size
	}
	o.mu.Unlock()

	ctx, cancel := o.ctx()
	defer cancel()

	size := int64(-1)

	if _, n, cl, err := o.Opener.Open(ctx, d); err == nil {
		size = n
		_ = cl.Close() //nolint:errcheck // read-only handle
	}

	o.mu.Lock()
	if o.sizes == nil || len(o.sizes) > 4096 {
		o.sizes = make(map[string]sizeEntry)
	}

	o.sizes[d] = sizeEntry{size: size, at: time.Now()}
	o.mu.Unlock()

	if size < 0 {
		return 0, -1
	}

	return size, size
}

// ReadAt implements ContentSource.
func (o *OpenerContent) ReadAt(d string, p []byte, off int64) (int, error) {
	ctx, cancel := o.ctx()
	defer cancel()

	ra, _, cl, err := o.Opener.Open(ctx, d)
	if err != nil {
		return 0, err
	}

	defer func() { _ = cl.Close() }() //nolint:errcheck // read-only handle

	n, err := ra.ReadAt(p, off)
	if n < len(p) {
		if err == nil || errors.Is(err, io.EOF) {
			err = errContentShort
		}

		return n, err
	}

	return n, nil
}
