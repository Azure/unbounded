// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Never unlink the lock file: waiters must always contend on the same inode.
// A hard-linked socket witness pins identity across crashes and inode reuse.
func listenOwnedOrigin(path string, mode os.FileMode) (*net.UnixListener, func(), error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(path) > socketPathLimit {
		return nil, nil, failure(ErrorInvalidArgument, "socket path", nil)
	}

	dir, err := openOriginDirectory(filepath.Dir(path))
	if err != nil {
		return nil, nil, ioFailure("owned socket directory", err)
	}

	keepDir := false

	defer func() {
		if !keepDir {
			closeBody(dir)
		}
	}()

	base := fmt.Sprintf("/proc/self/fd/%d/", dir.Fd())
	lockPath := base + ".racer-origin.lock"

	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, nil, ioFailure("socket lock", err)
	}

	keep := false

	defer func() {
		if !keep {
			closeBody(lock)
		}
	}()

	info, err := lock.Stat()
	if err != nil || !safeOriginFile(info) {
		return nil, nil, ioFailure("unsafe socket lock", os.ErrPermission)
	}

	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, nil, ioFailure("socket owner active", err)
	}

	current, err := os.Lstat(lockPath)
	if err != nil || !os.SameFile(info, current) {
		return nil, nil, ioFailure("socket lock replaced", os.ErrPermission)
	}

	socket := base + filepath.Base(path)

	witness := base + ".racer-origin.socket"
	if err := recoverOriginSocket(socket, witness); err != nil {
		return nil, nil, ioFailure("recover origin socket", err)
	}

	l, cleanupWitness, err := listenOriginAtWitness(witness, mode)
	if err != nil {
		return nil, nil, err
	}

	identity, err := os.Lstat(witness)
	if err != nil {
		cleanupWitness()
		return nil, nil, ioFailure("socket identity", err)
	}

	if err := os.Link(witness, socket); err != nil {
		cleanupWitness()
		return nil, nil, ioFailure("publish origin socket", err)
	}

	keep = true
	keepDir = true

	var once sync.Once

	return l, func() {
		once.Do(func() {
			defer closeBody(lock)
			defer closeBody(dir)

			closeBody(l)

			for _, name := range []string{filepath.Base(path), ".racer-origin.socket"} {
				entry := base + name
				if current, err := os.Lstat(entry); err == nil && os.SameFile(identity, current) {
					if err := os.Remove(entry); err != nil {
						// Keep the witness if canonical cleanup failed, for recovery.
						return
					}
				}
			}
		})
	}, nil
}

func safeOriginFile(info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return false
	}

	stat, ok := info.Sys().(*syscall.Stat_t)

	return ok && stat.Uid == uint32(os.Geteuid()) && stat.Nlink == 1
}

func openOriginDirectory(path string) (*os.File, error) {
	dir, err := os.Open("/")
	if err != nil {
		return nil, err
	}

	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		fd, err := unix.Openat(int(dir.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		closeBody(dir)

		if err != nil {
			return nil, err
		}

		dir = os.NewFile(uintptr(fd), part)
	}

	info, err := dir.Stat()
	if err != nil {
		closeBody(dir)
		return nil, err
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0o022 != 0 {
		closeBody(dir)
		return nil, os.ErrPermission
	}

	return dir, nil
}

func recoverOriginSocket(socket, witness string) error {
	owned, err := os.Lstat(witness)
	if os.IsNotExist(err) {
		if _, err := os.Lstat(socket); !os.IsNotExist(err) {
			return os.ErrExist
		}

		return nil
	}

	if err != nil {
		return err
	}

	if owned.Mode()&os.ModeSocket == 0 {
		return os.ErrPermission
	}

	current, err := os.Lstat(socket)
	if err == nil && !os.SameFile(owned, current) {
		return os.ErrExist
	}

	if err != nil && !os.IsNotExist(err) {
		return err
	}
	// A listener without our lock (for example an inherited descriptor) is live.
	conn, probeErr := net.DialTimeout("unix", witness, time.Second)
	if probeErr == nil {
		closeBody(conn)
		return os.ErrExist
	}

	if !errors.Is(probeErr, unix.ECONNREFUSED) {
		return probeErr
	}

	if err == nil {
		if err := os.Remove(socket); err != nil {
			return err
		}
	}

	return os.Remove(witness)
}

func listenOriginAtWitness(path string, mode os.FileMode) (*net.UnixListener, func(), error) {
	// The caller pinned and validated the directory; /proc/self/fd is intentional.
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, nil, ioFailure("socket bind", err)
	}

	l.SetUnlinkOnClose(false)

	identity, err := os.Lstat(path)
	if err != nil {
		closeBody(l)
		return nil, nil, ioFailure("witness identity", err)
	}

	cleanup := func() {
		closeBody(l)

		if current, err := os.Lstat(path); err == nil && os.SameFile(identity, current) {
			if err := os.Remove(path); err != nil {
				return
			}
		}
	}
	if err := os.Chmod(path, mode); err != nil {
		cleanup()
		return nil, nil, ioFailure("socket mode", err)
	}

	return l, cleanup, nil
}
