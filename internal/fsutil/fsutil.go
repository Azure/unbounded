// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package fsutil provides durable filesystem helpers shared by the agent
// library and the agent commands.
package fsutil

import (
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/google/renameio/v2"
)

// SyncDir persists a directory entry so newly written names survive a crash.
func SyncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}

	return errors.Join(f.Sync(), f.Close())
}

// WriteFileDurable also persists newly created parent directories, so the
// written file cannot outlive the directory entries needed to reach it.
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

	if err := writeFile(path, data, mode); err != nil {
		return err
	}

	for _, dir := range parents {
		if err := SyncDir(dir); err != nil {
			return err
		}
	}

	return nil
}

// writeFile writes content atomically, creating parent directories as needed.
// The temporary file shares the destination directory so it inherits the
// correct SELinux label instead of the temp-directory label.
func writeFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}

	return renameio.WriteFile(path, data, mode, renameio.WithTempDir(filepath.Dir(path)))
}

// InstallFile streams source onto target atomically. Large executables are
// copied rather than buffered in memory.
func InstallFile(source, target string, mode os.FileMode) (err error) {
	f, err := os.Open(source)
	if err != nil {
		return err
	}

	defer func() { err = errors.Join(err, f.Close()) }()

	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return err
	}

	pending, err := renameio.NewPendingFile(target, renameio.WithPermissions(mode), renameio.WithTempDir(filepath.Dir(target)))
	if err != nil {
		return err
	}

	defer pending.Cleanup() //nolint:errcheck // Pending file cleanup after atomic replacement.

	if _, err := io.Copy(pending, f); err != nil {
		return err
	}

	return pending.CloseAtomicallyReplace()
}

// FileSHA256 returns the SHA-256 digest of the file at path.
func FileSHA256(path string) ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte

	file, err := os.Open(path)
	if err != nil {
		return digest, err
	}
	defer file.Close() //nolint:errcheck // read error is authoritative

	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return digest, err
	}

	copy(digest[:], hasher.Sum(nil))

	return digest, nil
}
