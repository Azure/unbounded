// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"sync/atomic"
)

// DownloadFile performs HEAD, then downloads into dst starting at dstOffset.
// It has the same ownership and partial-output rules as Object.DownloadFile.
func (c *Client) DownloadFile(ctx context.Context, target string, dst *os.File, dstOffset int64) (Metadata, error) {
	o, err := c.Open(ctx, target)
	if err != nil {
		return Metadata{}, err
	}

	_, err = o.DownloadFile(ctx, dst, dstOffset)

	return o.meta, err
}

// DownloadFile writes the snapshot using parallel, aligned page requests and
// explicit destination offsets. Linux uses socket-to-pipe-to-file splice when
// supported; other paths use bounded WriteAt copies. Neither path moves dst's
// cursor. dst must be a writable regular file, not opened with O_APPEND.
// The caller owns dst and must keep it open, without overlapping writes, until
// return. The file is never closed, truncated, or synced. On error, the count is
// the sum of actual writes, not necessarily a contiguous prefix; no retry occurs.
func (o *Object) DownloadFile(ctx context.Context, dst *os.File, dstOffset int64) (int64, error) {
	if err := validateFileDestination(dst, dstOffset, o.meta.Size); err != nil {
		return 0, err
	}

	var written atomic.Int64

	err := o.pages(ctx, 0, o.meta.Size, func(ctx context.Context, start, end int64) error {
		s, err := o.ReadRange(ctx, start, end-start+1)
		if err != nil {
			return err
		}
		defer s.Close() //nolint:errcheck // Close only releases stream resources.

		n, err := s.WriteToFile(dst, dstOffset+start)
		written.Add(n)

		return err
	})

	return written.Load(), err
}

// WriteToFile consumes the remaining stream into dst at dstOffset without
// changing the file cursor. Ownership and errors follow Object.DownloadFile;
// this sequential call's count is a contiguous written prefix. Stats reports
// actual splice traffic. Close may interrupt socket I/O concurrently. Regular
// file syscalls cannot be interrupted by context; cancellation is checked between
// them. Unsupported splice operations resume with WriteAt at exact progress.
func (s *Stream) WriteToFile(dst *os.File, dstOffset int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := validateFileDestination(dst, dstOffset, s.end-s.offset); err != nil {
		return 0, err
	}

	return s.spliceToFile(dst, dstOffset)
}

func validateFileDestination(dst *os.File, offset, length int64) error {
	if dst == nil || offset < 0 || length < 0 || offset > math.MaxInt64-length {
		return fmt.Errorf("racer: invalid file destination or interval")
	}

	info, err := dst.Stat()
	if err != nil {
		return err
	}

	if !info.Mode().IsRegular() {
		return fmt.Errorf("racer: destination must be a regular file")
	}

	return nil
}

func (s *Stream) copyToFile(dst io.WriterAt, offset int64) (int64, error) {
	buf := copyBuffers.Get().(*[]byte) //nolint:errcheck // The private pool only contains *[]byte.
	defer copyBuffers.Put(buf)

	return s.copyToFileBuffer(dst, offset, *buf)
}

func (s *Stream) copyToFileBuffer(dst io.WriterAt, offset int64, buf []byte) (int64, error) {
	var total int64

	for {
		n, readErr := s.read(buf)
		if n > 0 {
			written, err := dst.WriteAt(buf[:n], offset+total)

			total += int64(written)
			if err == nil && written != n {
				err = io.ErrShortWrite
			}

			if err != nil {
				return total, s.fail(err)
			}
		}

		if readErr != nil {
			if readErr == io.EOF {
				readErr = nil
			}

			return total, readErr
		}
	}
}
