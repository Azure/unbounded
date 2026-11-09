// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package meshfetch

import (
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

// defaultScanInterval bounds how often the ingest directory is listed to
// find in-progress downloads.
const defaultScanInterval = 100 * time.Millisecond

// DirContent reads a containerd content store directly from its directory
// (typically a read-only hostPath mount). Committed blobs live at
// blobs/sha256/<hex>; a blob being downloaded lives at ingest/<key>/data,
// where ingest/<key>/ref names its digest and ingest/<key>/total its size.
// containerd writes ingests sequentially and renames data into blobs/ on
// commit, so an open descriptor stays valid across the commit. Descriptors
// are reused across page reads (a fresh descriptor per 1MiB read costs
// about a third of the read) and closed fileIdle after their last use, so
// a blob containerd deletes does not keep its space for long. Reads go
// through the kernel page cache, which usually still holds bytes
// containerd has just written.
type DirContent struct {
	root string
	log  *slog.Logger
	// scanInterval bounds how often the ingest directory is listed.
	scanInterval time.Duration

	mu       sync.Mutex
	ingests  map[string]ingestEntry
	lastScan time.Time
	// files holds open descriptors by blob hex.
	files map[string]*openFile
	// fileIdle closes a descriptor unused for this long.
	fileIdle time.Duration

	disabled atomic.Bool
}

// openFile is a shared descriptor of one blob. Guarded by DirContent.mu.
type openFile struct {
	f    *os.File
	refs int
	// gone is set once the entry left the files map; the last user
	// closes it.
	gone bool
	// idle closes the descriptor fileIdle after its last use.
	idle *time.Timer
}

// defaultFileIdle bounds how long an unused descriptor stays open.
const defaultFileIdle = 2 * time.Second

type ingestEntry struct {
	dir   string
	total int64
}

// NewDirContent returns a DirContent for the content root, the directory
// that holds blobs/ and ingest/ (for example
// /var/lib/containerd/io.containerd.content.v1.content). It returns an error
// when the root cannot be read.
func NewDirContent(root string, logger *slog.Logger) (*DirContent, error) {
	if root == "" {
		return nil, errors.New("meshfetch: content root is required")
	}

	if logger == nil {
		logger = slog.Default()
	}

	if _, err := os.ReadDir(filepath.Join(root, "blobs", "sha256")); err != nil {
		return nil, fmt.Errorf("meshfetch: content root %s: %w", root, err)
	}

	return &DirContent{
		root:         root,
		log:          logger,
		scanInterval: defaultScanInterval,
		ingests:      make(map[string]ingestEntry),
		files:        make(map[string]*openFile),
		fileIdle:     defaultFileIdle,
	}, nil
}

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
	return filepath.Join(c.root, "blobs", "sha256", h)
}

// fail disables the source on a permission error, logging once. Other
// errors are left to the caller.
func (c *DirContent) fail(err error) {
	if errors.Is(err, fs.ErrPermission) && c.disabled.CompareAndSwap(false, true) {
		c.log.Warn("cachemesh cannot read the containerd content store; serving peers from memory only",
			slog.String("root", c.root), slog.Any("err", err))
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
// listing is older than scanInterval.
func (c *DirContent) ingest(h string, force bool) (ingestEntry, bool) {
	c.mu.Lock()
	e, ok := c.ingests[h]

	if ok && !force {
		c.mu.Unlock()
		return e, true
	}

	if time.Since(c.lastScan) < c.scanInterval {
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
	dir := filepath.Join(c.root, "ingest")
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

	of, err := c.acquire(h)
	if err != nil {
		return 0, err
	}

	n, err := of.f.ReadAt(p, off)

	// A short read may come from a truncated ingest or a deleted blob:
	// drop the descriptor so the next read opens the blob afresh.
	c.release(h, of, n < len(p))

	if n < len(p) {
		if err == nil || errors.Is(err, io.EOF) {
			err = errContentShort
		}

		return n, err
	}

	return n, nil
}

// acquire returns a shared descriptor of blob h with a reference.
func (c *DirContent) acquire(h string) (*openFile, error) {
	c.mu.Lock()
	if of := c.files[h]; of != nil {
		of.refs++
		c.mu.Unlock()

		return of, nil
	}
	c.mu.Unlock()

	f, err := c.open(h)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if of := c.files[h]; of != nil {
		// Another reader opened it meanwhile.
		_ = f.Close() //nolint:errcheck // read-only descriptor
		of.refs++

		return of, nil
	}

	of := &openFile{f: f, refs: 1}
	of.idle = time.AfterFunc(c.fileIdle, func() { c.closeIdle(h, of) })
	of.idle.Stop()
	c.files[h] = of

	return of, nil
}

// release drops a reference to of; drop removes it from the cache. The
// last reference arms the idle timer, or closes a dropped descriptor.
func (c *DirContent) release(h string, of *openFile, drop bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	of.refs--

	if drop && !of.gone {
		of.gone = true
		of.idle.Stop()
		delete(c.files, h)
	}

	if of.refs > 0 {
		return
	}

	if of.gone {
		_ = of.f.Close() //nolint:errcheck // read-only descriptor
		return
	}

	of.idle.Reset(c.fileIdle)
}

// closeIdle closes of when nobody used it since its timer was armed.
func (c *DirContent) closeIdle(h string, of *openFile) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if of.refs > 0 || of.gone {
		return
	}

	of.gone = true

	delete(c.files, h)

	_ = of.f.Close() //nolint:errcheck // read-only descriptor
}

// Close closes every idle descriptor; descriptors in use are closed by
// their last reader.
func (c *DirContent) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for h, of := range c.files {
		of.gone = true
		of.idle.Stop()
		delete(c.files, h)

		if of.refs == 0 {
			_ = of.f.Close() //nolint:errcheck // read-only descriptor
		}
	}
}

// openFiles returns the number of cached descriptors (tests).
func (c *DirContent) openFiles() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.files)
}

// open opens blob h: committed, else its ingest, else committed again in
// case it was committed between the two opens.
func (c *DirContent) open(h string) (*os.File, error) {
	f, err := os.Open(c.blobPath(h))
	if errors.Is(err, fs.ErrNotExist) {
		if e, ok := c.ingest(h, false); ok {
			f, err = os.Open(filepath.Join(e.dir, "data"))
		}

		if errors.Is(err, fs.ErrNotExist) {
			f, err = os.Open(c.blobPath(h))
		}
	}

	if err != nil {
		c.fail(err)
		return nil, err
	}

	return f, nil
}
