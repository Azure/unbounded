// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package utilio

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func SyncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}

	return errors.Join(f.Sync(), f.Close())
}

// WriteFileDurable also persists newly created parent directories, so the state
// file cannot outlive the directory entries needed to reach it.
func WriteFileDurable(path string, data []byte, mode os.FileMode) error {
	var parents []string

	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		_, err := os.Stat(dir)
		if err == nil {
			parents = append(parents, dir)
			break
		}

		if !errors.Is(err, os.ErrNotExist) {
			return err
		}

		parents = append(parents, dir)
	}

	if err := WriteFile(path, data, mode); err != nil {
		return err
	}

	for _, dir := range parents {
		if err := SyncDir(dir); err != nil {
			return err
		}
	}

	return nil
}

// SyncFilesystem persists all output on the filesystem before a checkpoint.
func SyncFilesystem(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}

	return errors.Join(unix.Syncfs(int(f.Fd())), f.Close())
}
