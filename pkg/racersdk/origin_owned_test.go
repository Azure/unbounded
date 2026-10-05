// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
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
		func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error) { return originMeta(0), nil, nil }, path)
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
		client := originClient(t, path, 1)
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
