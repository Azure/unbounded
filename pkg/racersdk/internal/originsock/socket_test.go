// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package originsock

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func socketDir(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("../../../../tmp", "sdk-")
	if err != nil {
		t.Fatal(err)
	}

	path, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := os.RemoveAll(path); err != nil {
			t.Error(err)
		}
	})

	return path
}

func TestOriginSocketLifecycle(t *testing.T) {
	dir := socketDir(t)

	path := filepath.Join(dir, "socket")
	if err := os.WriteFile(path, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := listenOrigin(path, 0o600); err == nil {
		t.Fatal("replaced file")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink("missing", path); err != nil {
		t.Fatal(err)
	}

	if _, _, err := listenOrigin(path, 0o600); err == nil {
		t.Fatal("followed socket symlink")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(dir, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}

	if _, _, err := listenOrigin(filepath.Join(link, "socket"), 0o600); err == nil {
		t.Fatal("followed parent symlink")
	}

	l, cleanup, err := listenOrigin(path, 0o640)
	if err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatal("mode", err)
	}

	if _, _, err := listenOrigin(path, 0o600); err == nil {
		t.Fatal("replaced live socket")
	}

	closeBody(l)

	if _, _, err := listenOrigin(path, 0o600); err == nil {
		t.Fatal("replaced stale socket")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}

	cleanup()

	data, err := os.ReadFile(path)
	if err != nil || string(data) != "replacement" {
		t.Fatal("removed replacement", err)
	}
}

func TestOwnedOriginRecoversWitnessBeforePublication(t *testing.T) {
	dir := socketDir(t)
	path := filepath.Join(dir, "socket")
	// Keep sun_path short in deeply nested worktrees.
	anchor, err := openOriginDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(anchor)

	witness := fmt.Sprintf("/proc/self/fd/%d/.racer-origin.socket", anchor.Fd())

	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: witness, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}

	listener.SetUnlinkOnClose(false)
	closeBody(listener)

	_, cleanup, err := listenOwnedOrigin(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cleanup()
}

func TestOwnedOriginRejectsSymlinkAncestorAndStaleReplacement(t *testing.T) {
	dir := socketDir(t)

	link := filepath.Join(dir, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}

	if _, _, err := listenOwnedOrigin(filepath.Join(link, "socket"), 0o600); err == nil {
		t.Fatal("followed directory symlink")
	}

	path := filepath.Join(dir, "socket")

	l, cleanup, err := listenOwnedOrigin(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	closeBody(l)

	if err := os.Rename(path, filepath.Join(dir, "original")); err != nil {
		t.Fatal(err)
	}

	foreign, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}

	foreign.SetUnlinkOnClose(false)
	closeBody(foreign)

	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := recoverOriginSocket(path, filepath.Join(dir, ".racer-origin.socket")); !errors.Is(err, os.ErrExist) {
		t.Fatalf("accepted foreign stale inode: %v", err)
	}

	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, current) {
		t.Fatalf("foreign stale inode removed: %v", err)
	}
}

func TestOwnedOriginUnsafePathsAndForeignEndpoints(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "directory", "fifo", "permissions", "writable-directory", "foreign-live", "foreign-stale", "socket-symlink", "socket-file", "witness-symlink", "witness-file"} {
		t.Run(kind, func(t *testing.T) {
			dir := socketDir(t)
			path := filepath.Join(dir, "socket")
			lock := filepath.Join(dir, ".racer-origin.lock")

			target := filepath.Join(dir, "target")
			if err := os.WriteFile(target, []byte("preserve"), 0o600); err != nil {
				t.Fatal(err)
			}

			var err error

			switch kind {
			case "symlink":
				err = os.Symlink(target, lock)
			case "hardlink":
				err = os.Link(target, lock)
			case "directory":
				err = os.Mkdir(lock, 0o700)
			case "fifo":
				err = unix.Mkfifo(lock, 0o600)
			case "permissions":
				err = os.WriteFile(lock, nil, 0o644)
			case "writable-directory":
				err = os.Chmod(dir, 0o777)
			case "socket-symlink":
				err = os.Symlink(target, path)
			case "socket-file":
				err = os.WriteFile(path, []byte("foreign"), 0o600)
			case "witness-symlink":
				err = os.Symlink(target, filepath.Join(dir, ".racer-origin.socket"))
			case "witness-file":
				err = os.WriteFile(filepath.Join(dir, ".racer-origin.socket"), nil, 0o600)
			default:
				var listener *net.UnixListener

				listener, err = net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
				if err == nil {
					listener.SetUnlinkOnClose(false)
					t.Cleanup(func() { closeBody(listener) })

					if kind == "foreign-stale" {
						closeBody(listener)
					}
				}
			}

			if err != nil {
				t.Fatal(err)
			}

			before, _ := os.Lstat(path)
			if _, _, err := listenOwnedOrigin(path, 0o600); err == nil {
				t.Fatal("accepted unsafe endpoint")
			}

			if before != nil {
				current, err := os.Lstat(path)
				if err != nil || !os.SameFile(before, current) {
					t.Fatalf("foreign endpoint changed: %v", err)
				}
			}

			data, err := os.ReadFile(target)
			if err != nil || string(data) != "preserve" {
				t.Fatalf("target changed: %q %v", data, err)
			}
		})
	}
}

func TestOwnedOriginPreservesReplacementAndLiveWitness(t *testing.T) {
	dir := socketDir(t)
	path := filepath.Join(dir, "socket")

	_, cleanup, err := listenOwnedOrigin(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// Even loss of the lock path cannot authorize replacing a live witness.
	if err := os.Rename(filepath.Join(dir, ".racer-origin.lock"), filepath.Join(dir, "old-lock")); err != nil {
		t.Fatal(err)
	}

	if _, _, err := listenOwnedOrigin(path, 0o600); err == nil {
		t.Fatal("replaced live witness")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}

	cleanup()

	data, err := os.ReadFile(path)
	if err != nil || string(data) != "foreign" {
		t.Fatalf("cleanup removed replacement: %q %v", data, err)
	}

	if _, _, err := listenOwnedOrigin(path, 0o600); !errors.Is(err, os.ErrExist) {
		t.Fatalf("foreign replacement accepted: %v", err)
	}
}
