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
	"syscall"

	"golang.org/x/sys/unix"
)

// originListener holds the persistent lock until socket cleanup has completed.
// Never remove the lock file: replacing its inode could admit two owners.
type originListener struct {
	net.Listener
	lock     *os.File
	path     string
	identity os.FileInfo
	once     sync.Once
	err      error
}

func listenOrigin(path string) (*originListener, error) {
	if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) || len(path) > 107 {
		return nil, fmt.Errorf("origin socket must be an absolute Unix socket path of at most 107 bytes")
	}

	// The cache volume provisions the directory and shared group. Do not create,
	// chmod, or chown it here. Its ancestors and group writers must be trusted:
	// flock serializes cooperating owners, not arbitrary directory mutations.
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return nil, err
	}

	if !parent.IsDir() || parent.Mode().Perm()&0o002 != 0 {
		return nil, fmt.Errorf("origin socket parent must be a real directory without world write access")
	}

	fd, err := unix.Open(path+".lock", unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o660)
	if err != nil {
		return nil, err
	}

	lock := os.NewFile(uintptr(fd), path+".lock")
	ok := false

	defer func() {
		if !ok {
			closeResource(lock)
		}
	}()

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}

	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) {
		return nil, fmt.Errorf("origin socket lock must be a singly linked regular file owned by this user")
	}

	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, fmt.Errorf("origin socket already owned: %w", err)
	}

	identity, err := lock.Stat()
	if err != nil {
		return nil, err
	}

	if err := checkSocketIdentity(path+".lock", identity); err != nil {
		return nil, err
	}

	if err := reclaimOriginSocket(path); err != nil {
		return nil, err
	}

	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	// net/http and deferred cleanup can both close the listener. Automatic
	// pathname unlinking could delete a replacement, even after losing ownership.
	listener.SetUnlinkOnClose(false)

	identity, err = os.Lstat(path)
	if err != nil {
		closeResource(listener)
		return nil, err
	}

	result := &originListener{Listener: listener, lock: lock, path: path, identity: identity}
	// Ownership has moved to result, including on a permission setup failure.
	ok = true

	if err := os.Chmod(path, 0o660); err != nil {
		closeResource(result)
		return nil, err
	}

	return result, nil
}

func reclaimOriginSocket(path string) error {
	identity, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return err
	}

	stat, ok := identity.Sys().(*syscall.Stat_t)
	if identity.Mode()&os.ModeSocket == 0 || !ok || stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("origin socket path must be a socket owned by this user")
	}

	// A nonblocking connect fails closed on a full backlog, permission errors,
	// and every result except ECONNREFUSED. Only that result proves staleness.
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}

	probe := os.NewFile(uintptr(fd), "origin socket probe")
	defer closeResource(probe)

	if err := unix.Connect(fd, &unix.SockaddrUnix{Name: path}); !errors.Is(err, unix.ECONNREFUSED) {
		if err == nil {
			return fmt.Errorf("origin socket is already listening")
		}

		return fmt.Errorf("probe origin socket: %w", err)
	}

	if err := checkSocketIdentity(path, identity); err != nil {
		return err
	}

	return os.Remove(path)
}

func checkSocketIdentity(path string, identity os.FileInfo) error {
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}

	if !os.SameFile(identity, current) {
		return fmt.Errorf("origin socket ownership changed: %s", path)
	}

	return nil
}

func (l *originListener) Close() error {
	l.once.Do(func() {
		l.err = l.Listener.Close()
		if current, err := os.Lstat(l.path); err == nil && os.SameFile(l.identity, current) {
			l.err = errors.Join(l.err, os.Remove(l.path))
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			l.err = errors.Join(l.err, err)
		}

		l.err = errors.Join(l.err, l.lock.Close())
	})

	return l.err
}
