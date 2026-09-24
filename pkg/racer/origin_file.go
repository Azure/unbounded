// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"time"
)

// PinnedFileRange is an optional capability on a successful Source or
// OpenRange body (including ResolvedRange bodies). The returned file and bounds
// describe exactly that source's bytes: the entire representation for Source,
// or the requested interval for an OpenRange body. Offset is a physical file
// offset, not an object offset. Bounds must be nonnegative and fit in int64.
//
// The source owns the file and closes it in its own Close; Origin closes only
// the source, exactly once. The file must be a readable regular file with an
// exclusively owned open-file description (dup is insufficient): Origin may
// seek and advance its cursor. No concurrent use or close is allowed. Pinning
// must atomically bind immutable bytes and metadata to the requested ETag, or
// opening must return ErrVersionChanged. An open FD alone does not prevent
// in-place mutation/truncation. Keep the inode immutable, including after Close:
// kernel network queues may still reference its pages. Replace/unlink old files
// rather than modifying their contents or reusing their inodes in place.
// Returning this capability opts into platform file dispatch; it does not
// weaken the Store/RangeStore context, metadata, or ownership contracts.
type PinnedFileRange interface {
	FileRange() (file *os.File, offset, length int64)
}

type pinnedSourceRange struct {
	sourceRange
	file           *os.File
	offset, length int64
}

func (s pinnedSourceRange) FileRange() (*os.File, int64, int64) {
	return s.file, s.offset, s.length
}

// Embedding the file preserves SyscallConn for Go's TCP sendfile dispatch;
// Read still observes cancellation on portable HTTP transports and wrappers.
type contextFile struct {
	*os.File
	ctx context.Context
}

func (f contextFile) Read(p []byte) (int, error) {
	if err := f.ctx.Err(); err != nil {
		return 0, err
	}

	return f.File.Read(p)
}

func originFileReader(ctx context.Context, source io.ReadCloser, length int64) (io.Reader, error) {
	pin, ok := source.(PinnedFileRange)
	if !ok {
		return io.LimitReader(source, length), nil
	}

	file, offset, size := pin.FileRange()
	if file == nil || offset < 0 || size != length || size < 0 || offset > math.MaxInt64-size {
		return nil, fmt.Errorf("racer: invalid pinned file range")
	}

	info, err := file.Stat()
	if err != nil {
		return nil, err
	}

	if !info.Mode().IsRegular() || offset > info.Size() || size > info.Size()-offset {
		return nil, fmt.Errorf("racer: pinned file range exceeds regular file bounds")
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}

	return io.LimitReader(contextFile{file, ctx}, length), nil
}

// net/http owns framing and the connection. Interrupt supported network writes
// on cancellation, and join the callback before the handler returns. Unsupported
// ResponseWriter wrappers must provide their own blocked-write cancellation.
func cancelOriginWrite(ctx context.Context, w http.ResponseWriter) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now()) //nolint:errcheck // Some ResponseWriters cannot interrupt writes.

		close(done)
	})

	return func() {
		if !stop() {
			<-done
		}
	}
}
