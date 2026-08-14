// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package ingest

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// FreeSpace reports the bytes available on the filesystem holding path.
//
// It reads Bavail rather than Bfree: the difference is the reserve only root
// can spend, and although this agent does run as root, spending a filesystem's
// last-resort reserve to cache a container image is exactly the trade this
// package exists to avoid.
func FreeSpace(path string) (uint64, error) {
	var st unix.Statfs_t

	if err := unix.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}

	return st.Bavail * uint64(st.Bsize), nil //nolint:gosec // a filesystem block size is never negative
}
