// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package meshfetch

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// openChunkFile creates path, with O_DIRECT when direct is set and the
// file system supports it.
func openChunkFile(path string, direct bool) (*os.File, bool, error) {
	const flags = os.O_RDWR | os.O_CREATE | os.O_EXCL

	if direct {
		f, err := os.OpenFile(path, flags|unix.O_DIRECT, 0o600)
		if err == nil {
			return f, true, nil
		}

		if !errors.Is(err, unix.EINVAL) {
			return nil, false, err
		}
	}

	f, err := os.OpenFile(path, flags, 0o600)

	return f, false, err
}

// preallocate reserves size bytes so that a full disk fails here, before
// any byte is relied on, rather than in a later write. File systems
// without fallocate get a sparse file.
func preallocate(f *os.File, size int64) error {
	if size <= 0 {
		return nil
	}

	err := unix.Fallocate(int(f.Fd()), 0, 0, size) //nolint:gosec // fd fits in int
	if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOSYS) {
		return nil
	}

	return err
}

// dropWritten writes back a range and drops it from the page cache.
func dropWritten(f *os.File, off, n int64) {
	fd := int(f.Fd())                                                                                                               //nolint:gosec // fd fits in int
	_ = unix.SyncFileRange(fd, off, n, unix.SYNC_FILE_RANGE_WAIT_BEFORE|unix.SYNC_FILE_RANGE_WRITE|unix.SYNC_FILE_RANGE_WAIT_AFTER) //nolint:errcheck // advisory
	_ = unix.Fadvise(fd, off, n, unix.FADV_DONTNEED)                                                                                //nolint:errcheck // advisory
}

// dropRead drops a clean range from the page cache.
func dropRead(f *os.File, off, n int64) {
	_ = unix.Fadvise(int(f.Fd()), off, n, unix.FADV_DONTNEED) //nolint:errcheck,gosec // advisory
}

func mapDiskErr(err error) error {
	if errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.EDQUOT) {
		return errors.Join(errDiskFull, err)
	}

	return err
}
