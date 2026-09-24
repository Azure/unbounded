// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PrepareSocketDirectory creates missing parents with shared-group permissions.
// The deployment provisions the socket root's group. Setgid on each new level
// propagates that group to the client/origin directories and their socket files.
// Existing directories retain their permissions; symlink parents are rejected.
func PrepareSocketDirectory(socket string) error {
	if !filepath.IsAbs(socket) || strings.ContainsRune(socket, 0) || len(socket) > 107 {
		return fmt.Errorf("socket must be an absolute Unix path of at most 107 bytes")
	}

	return prepareDirectory(filepath.Dir(socket))
}

func prepareDirectory(path string) error {
	info, err := os.Lstat(path)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("socket parent %q is not a directory", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if parent := filepath.Dir(path); parent != path {
		if err := prepareDirectory(parent); err != nil {
			return err
		}
	}

	if info != nil {
		return nil
	}

	const mode = os.ModeSetgid | 0o770
	if err := os.Mkdir(path, mode); err != nil {
		if errors.Is(err, os.ErrExist) {
			info, err := os.Lstat(path)
			if err == nil && info.IsDir() {
				return nil
			}
		}

		return err
	}

	return os.Chmod(path, mode)
}
