// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/google/renameio/v2"
)

// writeFile writes content to filename atomically, creating parent directories
// as needed. The temp file is created in the same directory as the destination to
// preserve the correct SELinux label (avoiding the user_tmp_t label from /tmp).
func writeFile(filename string, content []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(filename), 0o750); err != nil {
		return err
	}

	return renameio.WriteFile(filename, content, perm, renameio.WithTempDir(filepath.Dir(filename)))
}

func installBinary(source, target string) (err error) {
	f, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()

	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return err
	}

	pending, err := renameio.NewPendingFile(target, renameio.WithPermissions(0o755), renameio.WithTempDir(filepath.Dir(target)))
	if err != nil {
		return err
	}
	defer pending.Cleanup() //nolint:errcheck // Pending file cleanup after atomic replacement.

	if _, err := io.Copy(pending, f); err != nil {
		return err
	}

	return pending.CloseAtomicallyReplace()
}
