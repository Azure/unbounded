// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package fsutil

import (
	"errors"
	"fmt"
	"os"
)

var errLinuxOnly = errors.New("operation requires Linux")

// SyncFilesystems is unsupported outside Linux.
func SyncFilesystems(...string) error {
	return errLinuxOnly
}

// SyncOpenFilesystems is unsupported outside Linux.
func SyncOpenFilesystems([]*os.File, func(int) error) error {
	return errLinuxOnly
}

// MountedNoexec is unsupported outside Linux.
func MountedNoexec(path string) (bool, error) {
	return false, fmt.Errorf("inspect the filesystem of %s: %w", path, errLinuxOnly)
}
