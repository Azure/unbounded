// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package installstate

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
)

var ErrLockHeld = errors.New("another host lifecycle operation holds the installation lock")

type Lock struct{ flock *flock.Flock }

// acquireLockAt is nonblocking. The kernel releases the lock on process exit; a
// leftover lock file does not imply a held lock and must not be deleted by reset.
func acquireLockAt(path string) (*Lock, error) {
	// flock.New does not create the parent directory.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	l := flock.New(path)

	switch locked, err := l.TryLock(); {
	case err != nil:
		return nil, err
	case !locked:
		return nil, ErrLockHeld
	}

	return &Lock{flock: l}, nil
}

func (l *Lock) Release() error {
	if l == nil || l.flock == nil {
		return nil
	}

	return l.flock.Unlock()
}
