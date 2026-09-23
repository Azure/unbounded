// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// The persistent lock inode serializes cooperating origins across restarts.
// Never remove the lock pathname: a replacement inode could admit two owners.
type originListener struct {
	*net.UnixListener
	lock     *os.File
	path     string
	identity os.FileInfo
	once     sync.Once
	err      error
}

func listenOrigin(path string) (*originListener, error) {
	if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) || len(path) > 107 {
		return nil, fmt.Errorf("origin-socket must be an absolute Unix socket path of at most 107 bytes")
	}

	fd, err := unix.Open(path+".lock", unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o660)
	if err != nil {
		return nil, err
	}

	lock := os.NewFile(uintptr(fd), path+".lock")
	ok := false

	defer func() {
		if !ok {
			_ = lock.Close() //nolint:errcheck // Preserve the setup error.
		}
	}()

	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, fmt.Errorf("origin socket already owned: %w", err)
	}

	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("origin socket path is not a socket")
		}

		connection, dialErr := net.DialTimeout("unix", path, time.Second)
		if dialErr == nil {
			_ = connection.Close() //nolint:errcheck // The successful probe already establishes a live owner.
			return nil, fmt.Errorf("origin socket is already listening")
		}

		if !errors.Is(dialErr, unix.ECONNREFUSED) {
			return nil, fmt.Errorf("probe origin socket: %w", dialErr)
		}

		current, err := os.Lstat(path)
		if err != nil || !os.SameFile(info, current) {
			return nil, fmt.Errorf("origin socket changed while probing")
		}

		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}

	listener.SetUnlinkOnClose(false)

	identity, err := os.Lstat(path)
	if err != nil {
		_ = listener.Close() //nolint:errcheck // Preserve the stat error.
		return nil, err
	}

	result := &originListener{UnixListener: listener, lock: lock, path: path, identity: identity}
	if err := os.Chmod(path, 0o660); err != nil {
		_ = result.Close() //nolint:errcheck // Preserve the permission error.
		return nil, err
	}

	ok = true

	return result, nil
}

func (l *originListener) Close() error {
	l.once.Do(func() {
		l.err = l.UnixListener.Close()
		if current, err := os.Lstat(l.path); err == nil && os.SameFile(l.identity, current) {
			l.err = errors.Join(l.err, os.Remove(l.path))
		}

		l.err = errors.Join(l.err, l.lock.Close())
	})

	return l.err
}
