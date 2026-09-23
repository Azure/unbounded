// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func originTestPath(t *testing.T) string {
	t.Helper()
	// Short names also fit when TMPDIR is inside a worktree.
	dir, err := os.MkdirTemp(os.TempDir(), "s")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { os.RemoveAll(dir) })

	path, err := filepath.Abs(filepath.Join(dir, "o"))
	if err != nil {
		t.Fatal(err)
	}

	return path
}

func socketInfo(t *testing.T, path string) os.FileInfo {
	t.Helper()

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	return info
}

func foreignListener(t *testing.T, path string) *net.UnixListener {
	t.Helper()

	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}

	l.SetUnlinkOnClose(false)
	t.Cleanup(func() { l.Close() })

	return l
}

func assertOriginRejected(t *testing.T, path string) {
	t.Helper()

	if l, err := listenOrigin(path); err == nil {
		l.Close()
		t.Fatal("unexpectedly acquired socket")
	}
}

func assertSocketConnects(t *testing.T, path string) {
	t.Helper()

	c, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	c.Close()
}

func TestOriginSocketLifecycle(t *testing.T) {
	path := originTestPath(t)
	parent := socketInfo(t, filepath.Dir(path))

	l, err := listenOrigin(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	lock := socketInfo(t, path+".lock")
	if got := socketInfo(t, path).Mode().Perm(); got != 0o660 {
		t.Fatalf("socket mode=%o, want 660", got)
	}

	assertOriginRejected(t, path)
	assertSocketConnects(t, path)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := l.Close(); err != nil {
				t.Error(err)
			}
		})
	}

	wg.Wait()

	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("closed socket remains: %v", err)
	}

	replacement, err := listenOrigin(path)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()

	if !os.SameFile(lock, socketInfo(t, path+".lock")) {
		t.Fatal("lock inode replaced across restart")
	}

	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	assertOriginRejected(t, path)
	assertSocketConnects(t, path)

	if after := socketInfo(t, filepath.Dir(path)); !os.SameFile(parent, after) || parent.Mode() != after.Mode() {
		t.Fatal("parent directory changed")
	}
}

func TestOriginSocketForeignLiveAndStale(t *testing.T) {
	path := originTestPath(t)
	foreign := foreignListener(t, path)
	identity := socketInfo(t, path)
	assertOriginRejected(t, path)

	if !os.SameFile(identity, socketInfo(t, path)) {
		t.Fatal("live socket replaced")
	}

	assertSocketConnects(t, path)
	foreign.Close()

	l, err := listenOrigin(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	assertSocketConnects(t, path)
}

func TestOriginSocketFullBacklog(t *testing.T) {
	path := originTestPath(t)

	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)

	if err := unix.Bind(fd, &unix.SockaddrUnix{Name: path}); err != nil {
		t.Fatal(err)
	}

	if err := unix.Listen(fd, 0); err != nil {
		t.Fatal(err)
	}

	assertSocketConnects(t, path) // Fill the one-entry backlog without accepting.
	identity := socketInfo(t, path)
	start := time.Now()

	assertOriginRejected(t, path)

	if time.Since(start) > time.Second {
		t.Fatal("probe blocked on a full backlog")
	}

	if !os.SameFile(identity, socketInfo(t, path)) {
		t.Fatal("full-backlog socket replaced")
	}
}

func TestOriginSocketPreservesUnsafePaths(t *testing.T) {
	for _, kind := range []string{"file", "directory", "symlink", "fifo", "lock-symlink", "lock-fifo", "lock-directory", "lock-hardlink"} {
		t.Run(kind, func(t *testing.T) {
			path := originTestPath(t)
			target := path

			var err error

			switch kind {
			case "file":
				err = os.WriteFile(target, []byte("preserve"), 0o600)
			case "directory":
				err = os.Mkdir(target, 0o700)
			case "symlink":
				err = os.Symlink("missing", target)
			case "fifo":
				err = unix.Mkfifo(target, 0o600)
			case "lock-symlink":
				target += ".lock"
				err = os.Symlink("missing", target)
			case "lock-fifo":
				target += ".lock"
				err = unix.Mkfifo(target, 0o600)
			case "lock-directory":
				target += ".lock"
				err = os.Mkdir(target, 0o700)
			case "lock-hardlink":
				target += ".lock"

				if err := os.WriteFile(path+".other", []byte("preserve"), 0o600); err != nil {
					t.Fatal(err)
				}

				err = os.Link(path+".other", target)
			}

			if err != nil {
				t.Fatal(err)
			}

			identity := socketInfo(t, target)
			assertOriginRejected(t, path)

			if !os.SameFile(identity, socketInfo(t, target)) {
				t.Fatal("unsafe object replaced")
			}

			if kind == "file" || kind == "lock-hardlink" {
				data, err := os.ReadFile(target)
				if err != nil || string(data) != "preserve" {
					t.Fatalf("object contents changed: %q, %v", data, err)
				}
			}
		})
	}
}

func TestOriginSocketParent(t *testing.T) {
	path := originTestPath(t)

	parent := filepath.Dir(path)
	if err := os.Chmod(parent, 0o777); err != nil {
		t.Fatal(err)
	}

	assertOriginRejected(t, path)

	if err := os.Chmod(parent, os.ModeSetgid|0o770); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(parent, "link")
	if err := os.Symlink(parent, link); err != nil {
		t.Fatal(err)
	}

	assertOriginRejected(t, filepath.Join(link, "o"))
	assertOriginRejected(t, filepath.Join(parent, "missing", "o"))

	l, err := listenOrigin(path)
	if err != nil {
		t.Fatal(err)
	}

	l.Close()
}

func TestOriginSocketInvalidPath(t *testing.T) {
	for _, path := range []string{"", "relative", "/invalid\x00socket", "/" + strings.Repeat("x", 107)} {
		assertOriginRejected(t, path)
	}
}

func TestOriginSocketProbePermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses socket mode permissions")
	}

	path := originTestPath(t)
	foreignListener(t, path).Close()

	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}

	identity := socketInfo(t, path)
	if l, err := listenOrigin(path); !errors.Is(err, unix.EACCES) {
		if l != nil {
			l.Close()
		}

		t.Fatalf("expected probe permission error, got %v", err)
	}

	if !os.SameFile(identity, socketInfo(t, path)) {
		t.Fatal("inaccessible socket removed")
	}

	if err := os.Chmod(path, 0o660); err != nil {
		t.Fatal(err)
	}

	l, err := listenOrigin(path)
	if err != nil {
		t.Fatalf("failed setup retained the ownership lock: %v", err)
	}

	l.Close()
}

func TestOriginSocketClosePreservesReplacement(t *testing.T) {
	path := originTestPath(t)

	l, err := listenOrigin(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	foreignListener(t, path)

	if err := checkSocketIdentity(path, l.identity); err == nil {
		t.Fatal("replacement passed inode check")
	}

	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	assertSocketConnects(t, path)
}

func TestOriginSocketHeldLockPreservesStale(t *testing.T) {
	path := originTestPath(t)
	foreignListener(t, path).Close()
	identity := socketInfo(t, path)

	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o660)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	assertOriginRejected(t, path)

	if !os.SameFile(identity, socketInfo(t, path)) {
		t.Fatal("stale socket removed without lock ownership")
	}

	lock.Close()

	l, err := listenOrigin(path)
	if err != nil {
		t.Fatal(err)
	}

	l.Close()
}

func TestOriginSocketForeignOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chown requires root")
	}

	for _, suffix := range []string{"", ".lock"} {
		t.Run("owner"+suffix, func(t *testing.T) {
			path := originTestPath(t)
			foreignListener(t, path).Close()

			if suffix != "" {
				if err := os.WriteFile(path+suffix, nil, 0o660); err != nil {
					t.Fatal(err)
				}
			}

			if err := os.Chown(path+suffix, 65534, -1); err != nil {
				t.Fatal(err)
			}

			identity := socketInfo(t, path)
			assertOriginRejected(t, path)

			if !os.SameFile(identity, socketInfo(t, path)) {
				t.Fatal("foreign-owned socket removed")
			}
		})
	}
}

// The helper executes the real backend run path, including HTTP shutdown.
func TestOriginSocketBackendProcess(t *testing.T) {
	if os.Getenv("RACER_OBJECT_SOCKET_HELPER") != "1" {
		return
	}

	if err := run(t.Context(), []string{"backend", "--config", os.Getenv("RACER_OBJECT_SOCKET_CONFIG"), "--socket", os.Getenv("RACER_OBJECT_SOCKET_PATH"), "--azure-auth", "anonymous"}); err != nil {
		t.Fatal(err)
	}
}

func TestOriginSocketKilledBackendRestart(t *testing.T) {
	path := originTestPath(t)
	config := filepath.Join(filepath.Dir(path), "config.json")

	data, err := json.Marshal(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(config, data, 0o600); err != nil {
		t.Fatal(err)
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	child := exec.CommandContext(t.Context(), executable, "-test.run=^TestOriginSocketBackendProcess$")

	child.Env = append(os.Environ(), "RACER_OBJECT_SOCKET_HELPER=1", "RACER_OBJECT_SOCKET_CONFIG="+config, "RACER_OBJECT_SOCKET_PATH="+path)

	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}

	waited := false

	defer func() {
		if !waited {
			child.Process.Kill()
			child.Wait()
		}
	}()

	waitForOriginHTTP(t, path)

	if err := run(t.Context(), []string{"backend", "--config", config, "--socket", path, "--azure-auth", "anonymous"}); err == nil {
		t.Fatal("second backend acquired a live instance's socket")
	}

	identity := socketInfo(t, path)

	lock := socketInfo(t, path+".lock")
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}

	err = child.Wait()
	waited = true

	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("expected killed subprocess, got %v", err)
	}

	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("expected SIGKILL, got %v", exit)
	}

	if !os.SameFile(identity, socketInfo(t, path)) {
		t.Fatal("SIGKILL did not leave the socket pathname")
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() {
		done <- run(ctx, []string{"backend", "--config", config, "--socket", path, "--azure-auth", "anonymous"})
	}()

	t.Cleanup(func() {
		cancel()

		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}

			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("graceful shutdown left socket: %v", err)
			}

			l, err := listenOrigin(path)
			if err != nil {
				t.Errorf("graceful shutdown retained lock: %v", err)
			} else {
				l.Close()
			}
		case <-time.After(5 * time.Second):
			t.Error("backend shutdown timed out")
		}
	})
	waitForOriginHTTP(t, path)

	if !os.SameFile(lock, socketInfo(t, path+".lock")) {
		t.Fatal("restart replaced persistent lock")
	}
}

func waitForOriginHTTP(t *testing.T, path string) {
	t.Helper()

	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}
	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport, Timeout: time.Second}
	deadline := time.Now().Add(10 * time.Second)

	for {
		response, err := client.Get("http://origin/unknown")
		if err == nil {
			response.Body.Close()

			if response.StatusCode != http.StatusNotFound {
				t.Fatalf("backend status=%d, want 404", response.StatusCode)
			}

			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("backend did not become ready: %v", err)
		}

		time.Sleep(10 * time.Millisecond)
	}
}
