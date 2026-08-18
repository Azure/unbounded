// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package clean

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// SystemDiscarder trims a device with BLKDISCARD.
//
// RACER exports discard with a granularity of one page and refuses a partial
// one, so a segment is trimmed as a whole or not at all. A page that was never
// written, or that has already been trimmed, is a no-op rather than an error,
// which is what makes a retried drain safe.
type SystemDiscarder struct{}

// Discard trims the range.
func (SystemDiscarder) Discard(device string, offset, length uint64) error {
	if length == 0 {
		return nil
	}

	f, err := os.OpenFile(device, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("clean: open %s: %w", device, err)
	}

	defer f.Close() //nolint:errcheck // nothing was written through this descriptor

	// BLKDISCARD takes a two element array of unsigned 64 bit values, the
	// offset and the length, both in bytes.
	rng := [2]uint64{offset, length}

	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		f.Fd(),
		unix.BLKDISCARD,
		uintptr(unsafe.Pointer(&rng[0])),
	)
	if errno != 0 {
		return fmt.Errorf("clean: discard %s at %d for %d bytes: %w", device, offset, length, errno)
	}

	return nil
}
