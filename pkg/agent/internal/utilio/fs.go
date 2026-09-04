// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package utilio

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/google/renameio/v2"
)

// IsDirEmpty reports whether dir is empty or does not exist.
func IsDirEmpty(dir string) (bool, error) {
	f, err := os.Open(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return true, nil
	case err != nil:
		return false, err
	}

	defer func() { _ = f.Close() }() //nolint:errcheck // best effort close

	_, err = f.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		// no entry read
		return true, nil
	}

	return false, err
}

// CleanDir removes everything in a directory, but not the directory itself.
func CleanDir(path string) (retErr error) {
	_, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// nothing to do
		return nil
	case err != nil:
		return err
	default:
		// proceed to clean
	}

	d, err := os.Open(filepath.Clean(path))
	if err != nil {
		return err
	}

	defer func() {
		if cerr := d.Close(); cerr != nil && retErr == nil {
			retErr = fmt.Errorf("close directory: %w", cerr)
		}
	}()

	entries, err := d.Readdirnames(-1)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(path, entry)); err != nil {
			return err
		}
	}

	return nil
}

// UpdateSymlink atomically updates linkPath to point at targetPath.
func UpdateSymlink(linkPath, targetPath string) error {
	dir := filepath.Dir(linkPath)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}

	return renameio.Symlink(targetPath, linkPath)
}

// NearestExistingDir returns dir when it is an existing directory, and
// otherwise the closest ancestor that is.
//
// It exists so a caller can ask whether a path could be created without
// creating it. Probing the path itself answers a different question: a
// directory the agent has not made yet is reported as unusable even when its
// parent is perfectly writable, which is the normal state of an install prefix
// before the first bootstrap.
func NearestExistingDir(stat func(string) (fs.FileInfo, error), dir string) string {
	for {
		if info, err := stat(dir); err == nil && info.IsDir() {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}

		dir = parent
	}
}

// ProbeWritableDir verifies that dir accepts file creation and removal without
// leaving durable state behind.
//
// The directory must already exist. Use NearestExistingDir first to check a
// path the caller intends to create.
func ProbeWritableDir(dir string) error {
	f, err := os.CreateTemp(dir, ".unbounded-probe-*")
	if err != nil {
		return err
	}

	name := f.Name()
	if err := f.Close(); err != nil {
		os.Remove(name) //nolint:errcheck // best effort cleanup after close failure.
		return err
	}

	return os.Remove(name)
}
