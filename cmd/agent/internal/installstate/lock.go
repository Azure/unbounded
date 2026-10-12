// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package installstate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

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

// AcquireWithin calls acquire until it returns anything but ErrLockHeld, or
// until wait has passed. It returns ErrLockHeld if the lock is still held then,
// and ctx's error if ctx ends first.
func AcquireWithin(ctx context.Context, wait time.Duration, acquire func() (*Lock, error)) (*Lock, error) {
	deadline := time.Now().Add(wait)

	for {
		lock, err := acquire()
		if !errors.Is(err, ErrLockHeld) || !time.Now().Before(deadline) {
			return lock, err
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
