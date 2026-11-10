// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package contentstore

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const noFollow = unix.O_NOFOLLOW

// openBeneath uses openat2 with RESOLVE_BENEATH and RESOLVE_NO_SYMLINKS so no
// path component may be a symlink or escape root. Kernels without openat2 fall
// back to os.Root with O_NOFOLLOW.
func openBeneath(root, rel string) (*os.File, error) {
	dir, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: root, Err: err}
	}
	defer unix.Close(dir) //nolint:errcheck // read-only directory handle

	fd, err := unix.Openat2(dir, rel, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EPERM) {
		return openRoot(root, rel)
	}

	if err != nil {
		return nil, &os.PathError{Op: "openat2", Path: filepath.Join(root, rel), Err: err}
	}

	return os.NewFile(uintptr(fd), filepath.Join(root, rel)), nil
}

func isSymlinkError(err error) bool {
	return errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EXDEV)
}
