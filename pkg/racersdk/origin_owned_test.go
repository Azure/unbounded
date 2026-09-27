// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestOwnedOriginCrashChild(t *testing.T) {
	path := os.Getenv("RACER_ORIGIN_CRASH_PATH")
	if path == "" {
		return
	}

	cache, err := ParseCacheName("gantry")
	if err != nil {
		t.Fatal(err)
	}

	err = serveOrigin(context.Background(), OriginConfig{Cache: cache, RecoverStaleSocket: true},
		func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error) {
			return originMeta(0), nil, nil
		}, path)
	t.Fatal(err)
}

func TestOwnedOriginSIGKILLRestart(t *testing.T) {
	path := filepath.Join(socketDir(t), "socket")
	lockPath := filepath.Join(filepath.Dir(path), ".racer-origin.lock")

	for range 2 {
		child := exec.Command(os.Args[0], "-test.run=^TestOwnedOriginCrashChild$")

		child.Env = append(os.Environ(), "RACER_ORIGIN_CRASH_PATH="+path)

		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() {
			_ = child.Process.Kill()
			if child.ProcessState == nil {
				_ = child.Wait()
			}
		})
		client := testClient(t, path, 1)
		deadline := time.Now().Add(10 * time.Second)

		for {
			conn, err := net.DialTimeout("unix", path, 50*time.Millisecond)
			if err == nil {
				closeBody(conn)
				break
			}

			if time.Now().After(deadline) {
				t.Fatalf("child did not start: %v", err)
			}

			time.Sleep(10 * time.Millisecond)
		}
		// A real protocol request proves the restarted origin serves, not just binds.
		value, err := client.Get(t.Context(), Request{Key: Key{}})
		if err != nil {
			t.Fatal(err)
		}

		closeBody(value)

		before, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}

		lock, err := os.Stat(lockPath)
		if err != nil {
			t.Fatal(err)
		}

		if _, _, err := listenOwnedOrigin(path, 0o600); err == nil {
			t.Fatal("replaced live owner")
		}

		if err := child.Process.Kill(); err != nil {
			t.Fatal(err)
		}

		if err := child.Wait(); err == nil {
			t.Fatal("child was not killed")
		}

		current, err := os.Lstat(path)
		if err != nil || !os.SameFile(before, current) {
			t.Fatalf("SIGKILL did not retain socket: %v", err)
		}

		if _, _, err := listenOrigin(path, 0o600); err == nil {
			t.Fatal("default SDK contract recovered an existing socket")
		}

		current, err = os.Stat(lockPath)
		if err != nil || !os.SameFile(lock, current) {
			t.Fatalf("lock changed: %v", err)
		}
	}

	_, cleanup, err := listenOwnedOrigin(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cleanup()
	cleanup() // Cleanup is idempotent and cannot act on a reused directory FD.

	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("clean shutdown retained socket: %v", err)
	}

	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("clean shutdown removed lock: %v", err)
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
