// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package fsutil

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// SyncFilesystems persists every filesystem backing the given paths.
func SyncFilesystems(paths ...string) error {
	var files []*os.File

	defer func() {
		for _, f := range files {
			_ = f.Close() //nolint:errcheck // Read-only handle; sync errors are returned.
		}
	}()

	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return err
		}

		files = append(files, f)
	}

	return SyncOpenFilesystems(files, unix.Syncfs)
}

// SyncOpenFilesystems synchronizes each distinct filesystem once, using open
// handles that stay valid after teardown removes their paths.
func SyncOpenFilesystems(files []*os.File, syncfs func(int) error) error {
	seen := map[uint64]bool{}

	for _, f := range files {
		var stat unix.Stat_t
		if err := unix.Fstat(int(f.Fd()), &stat); err != nil {
			return err
		}

		if seen[uint64(stat.Dev)] {
			continue
		}

		seen[uint64(stat.Dev)] = true

		if err := syncfs(int(f.Fd())); err != nil {
			return fmt.Errorf("sync %s: %w", f.Name(), err)
		}
	}

	return nil
}

// MountedNoexec reports whether the filesystem holding path is mounted without
// permission to run programs from it, so a binary placed there cannot run.
func MountedNoexec(path string) (bool, error) {
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		return false, fmt.Errorf("inspect the filesystem of %s: %w", path, err)
	}

	return fs.Flags&unix.ST_NOEXEC != 0, nil
}
