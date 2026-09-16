// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package installstate

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

var ErrLockHeld = errors.New("another host lifecycle operation holds the installation lock")

type Lock struct{ file *os.File }

// acquireLockAt is nonblocking. The kernel releases flock on process exit; a
// leftover lock file does not imply a held lock and must not be deleted by reset.
func acquireLockAt(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}

	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		closeErr := f.Close()

		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errors.Join(ErrLockHeld, closeErr)
		}

		return nil, errors.Join(err, closeErr)
	}

	return &Lock{file: f}, nil
}

func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}

	err := l.file.Close()
	l.file = nil

	return err
}
