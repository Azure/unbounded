// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package utilio

import (
	"errors"
	"io"
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

// UpdateSymlink atomically updates linkPath to point at targetPath.
func UpdateSymlink(linkPath, targetPath string) error {
	dir := filepath.Dir(linkPath)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}

	return renameio.Symlink(targetPath, linkPath)
}

// ProbeWritableDir verifies that dir accepts file creation and removal without
// leaving durable state behind.
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
