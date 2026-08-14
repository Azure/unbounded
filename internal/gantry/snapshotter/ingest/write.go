// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ingest

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"unsafe"

	"github.com/Azure/unbounded/internal/gantry/snapshotter/catalog"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/segment"
)

// Alignment is the memory and offset alignment O_DIRECT requires.
//
// The kernel only demands the device's logical block size, which is 512 on
// most devices, but 4096 is a superset of every case in the fleet and matches
// the catalog block size, so one constant covers both paths.
const Alignment = 4096

// ErrVerify reports that a blob did not read back as it was written.
var ErrVerify = errors.New("ingest: blob verification failed")

// ErrShortLayer reports that the source ran out before the declared size.
var ErrShortLayer = errors.New("ingest: layer shorter than declared")

// ErrLongLayer reports that the source had bytes left after the declared size.
var ErrLongLayer = errors.New("ingest: layer longer than declared")

// Device is the block device a segment is exported on.
type Device interface {
	io.ReaderAt
	io.WriterAt
	io.Closer
}

// OpenFunc opens a segment device for writing. It is a field on the Ingester so
// tests can hand back an ordinary file instead of a ublk device.
type OpenFunc func(path string) (Device, error)

// AlignedBuffer returns a slice of exactly n bytes whose backing array starts
// on an Alignment boundary, as O_DIRECT requires.
//
// Go's allocator gives no alignment guarantee above the word size, so the only
// portable way to get this is to over-allocate and slice into the result. The
// slice keeps the whole allocation alive, so the aligned view cannot outlive
// its backing array.
func AlignedBuffer(n int) []byte {
	buf := make([]byte, n+Alignment)

	off := int(uintptr(unsafe.Pointer(&buf[0])) & (Alignment - 1))
	if off != 0 {
		off = Alignment - off
	}

	return buf[off : off+n : off+n]
}

// WriteBlob copies exactly size bytes from src into dev starting at off and
// returns the sha256 of those bytes.
//
// Writes are always whole 4 MiB pages because RACER's IMMUTABLE_4M extents
// accept nothing else: a partial page write is rejected, not merged. The tail
// of the last page is zero filled, which is also why the catalog record carries
// a byte length separate from the page count. The zero fill is not wasted work
// even ignoring the alignment rule: the padding is inside the blob's own page
// span, so no other blob can ever see it.
func WriteBlob(dev io.WriterAt, off uint64, src io.Reader, size uint64) (catalog.Digest, error) {
	var sum catalog.Digest

	if off%segment.PageBytes != 0 {
		return sum, fmt.Errorf("ingest: offset %d is not page aligned", off)
	}

	if size == 0 {
		return sum, errors.New("ingest: empty blob")
	}

	buf := AlignedBuffer(segment.PageBytes)
	hash := sha256.New()

	for written := uint64(0); written < size; {
		n := size - written
		if n > segment.PageBytes {
			n = segment.PageBytes
		}

		if _, err := io.ReadFull(src, buf[:n]); err != nil {
			return sum, fmt.Errorf("%w: %w", ErrShortLayer, err)
		}

		clear(buf[n:])

		if _, err := hash.Write(buf[:n]); err != nil {
			return sum, fmt.Errorf("ingest: hash: %w", err)
		}

		if _, err := dev.WriteAt(buf, int64(off+written)); err != nil {
			return sum, fmt.Errorf("ingest: write page at %d: %w", off+written, err)
		}

		written += n
	}

	if _, err := src.Read(make([]byte, 1)); err != io.EOF { //nolint:errorlint // io.Reader may return a bare io.EOF
		return sum, ErrLongLayer
	}

	copy(sum[:], hash.Sum(nil))

	return sum, nil
}

// VerifyBlob re-reads size bytes from off and checks them against want.
//
// This exists because RACER's 4 MiB pages carry no data checksum: the huge page
// class is zero copy from a registered guest buffer with no room on the wire for
// a guard. Every other node in the cluster will trust this blob on the strength
// of one catalog record, so paying one extra read on the single node that wrote
// it is cheap insurance. It also catches the case where the reservation
// arithmetic put the blob somewhere other than where the record says it is.
func VerifyBlob(dev io.ReaderAt, off, size uint64, want catalog.Digest) error {
	if off%segment.PageBytes != 0 {
		return fmt.Errorf("ingest: offset %d is not page aligned", off)
	}

	buf := AlignedBuffer(segment.PageBytes)
	hash := sha256.New()

	for read := uint64(0); read < size; {
		if _, err := dev.ReadAt(buf, int64(off+read)); err != nil {
			return fmt.Errorf("ingest: read page at %d: %w", off+read, err)
		}

		n := size - read
		if n > segment.PageBytes {
			n = segment.PageBytes
		}

		if _, err := hash.Write(buf[:n]); err != nil {
			return fmt.Errorf("ingest: hash: %w", err)
		}

		read += n
	}

	var got catalog.Digest

	copy(got[:], hash.Sum(nil))

	if got != want {
		return fmt.Errorf("%w: wrote %s, read back %s", ErrVerify, want.Short(), got.Short())
	}

	return nil
}
