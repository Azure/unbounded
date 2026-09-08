// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package streamcopy copies known-length blob streams in larger pooled chunks.
package streamcopy

import (
	"errors"
	"io"
	"sync"
)

// BufferSize balances stream-call reduction against concurrent memory use. At
// the benchmark's 10 peer serves plus 6 local downloads, a fully busy agent can
// hold at most roughly 16 MiB of copy buffers.
const BufferSize = 1 << 20

type buffer [BufferSize]byte

var buffers = sync.Pool{ //nolint:gochecknoglobals // One bounded reusable pool per process.
	New: func() any { return new(buffer) },
}

// CopyN copies exactly size bytes. It fills each pooled chunk before writing,
// reducing containerd ReaderAt streams and downstream Writer calls while
// preserving partial bytes and the original read error on interruption.
func CopyN(dst io.Writer, src io.Reader, size int64) (int64, error) {
	if size < 0 {
		return 0, errors.New("streamcopy: negative size")
	}

	if size == 0 {
		return 0, nil
	}

	copyBuffer, ok := buffers.Get().(*buffer)
	if !ok {
		return 0, errors.New("streamcopy: invalid pooled buffer")
	}

	defer buffers.Put(copyBuffer)

	var written int64

	for written < size {
		chunkSize := min(int64(len(copyBuffer)), size-written)

		read, readErr := io.ReadFull(src, copyBuffer[:chunkSize])
		if read > 0 {
			writeCount, writeErr := dst.Write(copyBuffer[:read])
			written += int64(writeCount)

			if writeErr != nil {
				return written, writeErr
			}

			if writeCount != read {
				return written, io.ErrShortWrite
			}
		}

		if readErr != nil {
			return written, readErr
		}
	}

	return written, nil
}
