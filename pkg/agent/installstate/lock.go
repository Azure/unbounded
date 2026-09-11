// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package installstate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// LockPathForTest overrides the lock location. Empty means the real one.
//
// The lock path is absolute and under /run, which a test cannot write to, and
// the behavior worth testing is what happens when two runs overlap. That needs
// a real flock on a real file, so the path has to be redirectable.
var LockPathForTest string

// LockPath returns the path of the host installation lock.
//
// It lives under /run rather than beside the record: the lock is about who is
// mutating the host right now, so it should not survive a reboot. A stale lock
// file from a machine that lost power would otherwise block the recovery that
// reboot is supposed to enable.
func LockPath() string {
	if LockPathForTest != "" {
		return LockPathForTest
	}

	return "/run/unbounded-agent-install.lock"
}

// ErrLockHeld is returned when another process is already installing or
// tearing down.
var ErrLockHeld = errors.New("another bootstrap or reset is already running on this host")

// Lock is a held host installation lock.
type Lock struct {
	file *os.File
}

// AcquireLock takes the host installation lock without blocking.
//
// Bootstrap and reset both mutate the same files and the same record, and the
// bootstrap unit retries on a timer, so two of them overlapping is a real
// possibility rather than a theoretical one. Failing fast is better than
// queueing: the caller is a retrying systemd unit, and a second attempt is
// cheap.
func AcquireLock() (*Lock, error) {
	return acquireLockAt(LockPath())
}

func acquireLockAt(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	// LOCK_EX|LOCK_NB: an advisory lock tied to this open file description, so
	// the kernel releases it if the process dies without unlocking. That is the
	// case that matters, because the holder is a bootstrap that may be killed
	// partway through.
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close() //nolint:errcheck // best effort, the lock was not taken

		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrLockHeld
		}

		return nil, fmt.Errorf("lock %s: %w", path, err)
	}

	return &Lock{file: file}, nil
}

// Release drops the lock.
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}

	// Closing the descriptor releases the flock; unlocking first makes the
	// intent explicit and surfaces an error that close would discard.
	if err := unix.Flock(int(l.file.Fd()), unix.LOCK_UN); err != nil {
		_ = l.file.Close() //nolint:errcheck // already failing

		return fmt.Errorf("unlock %s: %w", l.file.Name(), err)
	}

	if err := l.file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", l.file.Name(), err)
	}

	l.file = nil

	return nil
}
