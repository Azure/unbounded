// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package meshfetch

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
)

// PageCacheMode selects how chunk files use the kernel page cache.
type PageCacheMode string

const (
	// PageCacheBypass opens chunk files with O_DIRECT, so chunk bytes
	// never enter the page cache (and never count toward the container's
	// working set). Falls back to PageCacheDrop where O_DIRECT is not
	// supported or the page size is not 4KiB aligned.
	PageCacheBypass PageCacheMode = "bypass"
	// PageCacheDrop uses buffered I/O and drops written and read ranges
	// from the page cache (sync_file_range plus posix_fadvise DONTNEED).
	PageCacheDrop PageCacheMode = "drop"
	// PageCacheKeep uses buffered I/O and leaves caching to the kernel.
	PageCacheKeep PageCacheMode = "keep"
)

// dioAlign is the O_DIRECT offset, length, and buffer alignment.
const dioAlign = 4096

func alignUp(n int64) int64 { return (n + dioAlign - 1) &^ (dioAlign - 1) }

// errDiskFull reports that the chunk directory has no space left.
var errDiskFull = errors.New("meshfetch: chunk disk full")

// chunkFile is the on-disk copy of one chunk.
type chunkFile struct {
	f      *os.File
	path   string
	direct bool
	drop   bool
	// unlinked is set once the path is removed; the descriptor stays
	// open until the last reader is done.
	unlinked atomic.Bool
}

// diskDir creates chunk files in one directory.
type diskDir struct {
	dir  string
	mode PageCacheMode
	seq  atomic.Uint64
}

// openDiskDir prepares dir: it creates it if needed and removes everything
// in it. Chunks are not individually digest-verified, so files left by an
// earlier process are never trusted.
func openDiskDir(dir string, mode PageCacheMode) (*diskDir, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return nil, fmt.Errorf("meshfetch: wipe chunk dir: %w", err)
		}
	}

	return &diskDir{dir: dir, mode: mode}, nil
}

// create makes a new chunk file and reserves size bytes for it. direct
// requests O_DIRECT; it is ignored where unsupported. A full disk returns
// errDiskFull.
func (d *diskDir) create(size int64, direct bool) (*chunkFile, error) {
	path := filepath.Join(d.dir, "c"+strconv.FormatUint(d.seq.Add(1), 10))

	f, isDirect, err := openChunkFile(path, direct && d.mode == PageCacheBypass)
	if err != nil {
		return nil, mapDiskErr(err)
	}

	cf := &chunkFile{f: f, path: path, direct: isDirect, drop: !isDirect && d.mode != PageCacheKeep}

	if err := preallocate(f, alignUp(size)); err != nil {
		cf.remove()
		cf.close()

		return nil, mapDiskErr(err)
	}

	return cf, nil
}

// writeAt writes b at off. With O_DIRECT, b's capacity must cover the
// length rounded up to the alignment; the padding bytes land past the
// chunk's end and are never read.
func (c *chunkFile) writeAt(b []byte, off int64) error {
	if c.direct {
		b = b[:alignUp(int64(len(b)))]
	}

	if _, err := c.f.WriteAt(b, off); err != nil {
		return mapDiskErr(err)
	}

	if c.drop {
		dropWritten(c.f, off, int64(len(b)))
	}

	return nil
}

// readAt fills b[:n] from off. With O_DIRECT, b's capacity must cover n
// rounded up to the alignment.
func (c *chunkFile) readAt(b []byte, off int64, n int) error {
	want := int64(n)
	if c.direct {
		want = alignUp(want)
	}

	got, err := c.f.ReadAt(b[:want], off)
	if got >= n {
		err = nil
	} else if err == nil || errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}

	if c.drop {
		dropRead(c.f, off, want)
	}

	return err
}

// remove unlinks the file. Open descriptors keep working.
func (c *chunkFile) remove() {
	if c.unlinked.CompareAndSwap(false, true) {
		_ = os.Remove(c.path) //nolint:errcheck // best effort; the dir is wiped at startup
	}
}

func (c *chunkFile) close() { _ = c.f.Close() } //nolint:errcheck // read-mostly file
