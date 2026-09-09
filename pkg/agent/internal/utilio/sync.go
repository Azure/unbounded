// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package utilio

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// SyncDir flushes a directory entry so that names created or removed inside it
// survive a crash.
//
// Writing a file durably is two steps, not one: the file's own contents have to
// reach disk, and so does the directory entry that names it. renameio handles
// the first for the files it writes. This is the second, and it is what makes
// the difference between "the marker exists" and "the marker exists after the
// power comes back".
func SyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		// A directory that is not there has no entry to flush.
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("open %s: %w", dir, err)
	}

	defer func() { _ = f.Close() }() //nolint:errcheck // read-only handle

	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}

	return nil
}

// SyncDirTree flushes every regular file and directory under root.
//
// Used after unpacking an image, before anything records that the unpack
// finished. Without it the completion marker can reach disk while some of the
// extracted files have not, which produces exactly the state the marker exists
// to rule out: a rootfs that claims to be complete and is missing files.
func SyncDirTree(root string) error {
	if _, err := os.Stat(root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("stat %s: %w", root, err)
	}

	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Symlinks are named by their parent directory's entry, which is
		// flushed when that directory is walked; opening them here would
		// follow to the target and sync the wrong file.
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}

		defer func() { _ = f.Close() }() //nolint:errcheck // read-only handle

		if err := f.Sync(); err != nil {
			return fmt.Errorf("sync %s: %w", path, err)
		}

		return nil
	})
}

// WriteFileDurable writes a file and flushes its parent directory, so that both
// the contents and the name that refers to them survive a crash.
func WriteFileDurable(path string, content []byte, perm os.FileMode) error {
	if err := WriteFile(path, content, perm); err != nil {
		return err
	}

	return SyncDir(filepath.Dir(path))
}
