// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func socketPath(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "loadgen-")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	return filepath.Join(dir, "origin")
}

func unixTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	path := socketPath(t)

	l, err := listenOrigin(path)
	if err != nil {
		t.Fatal(err)
	}

	s := httptest.NewUnstartedServer(handler)
	_ = s.Listener.Close()
	s.Listener = l
	s.Start()
	// Tests pass this directly to the Unix-only SDK.
	s.URL = path
	t.Cleanup(s.Close)

	return s
}

func TestOriginSocketOwnershipAndRestart(t *testing.T) {
	path := socketPath(t)

	l, err := listenOrigin(path)
	if err != nil {
		t.Fatal(err)
	}

	if second, err := listenOrigin(path); err == nil {
		_ = second.Close()

		t.Fatal("second origin acquired live socket")
	}

	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o660 {
		t.Fatalf("socket permissions: %v, %v", info, err)
	}

	_ = l.Close()

	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}

	stale.SetUnlinkOnClose(false)
	_ = stale.Close()

	l, err = listenOrigin(path)
	if err != nil {
		t.Fatalf("stale socket restart: %v", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}

	_ = l.Close()

	if content, err := os.ReadFile(path); err != nil || string(content) != "replacement" {
		t.Fatalf("close removed replacement: %q, %v", content, err)
	}

	if l, err := listenOrigin(path); err == nil {
		_ = l.Close()

		t.Fatal("accepted non-socket pathname")
	}
}

func TestManagementAndOriginSeparation(t *testing.T) {
	h := managementHandler(prometheus.NewRegistry())

	for target, want := range map[string]int{"/healthz": 200, "/metrics": 200, "/healthz?": 404, "/objects/0": 404} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))

		if w.Code != want {
			t.Errorf("%s: got %d, want %d", target, w.Code, want)
		}
	}

	path := socketPath(t)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	c := config{
		endpoint: path, originSocket: path, listen: "127.0.0.1:0", footprint: 64, objectSize: 64,
		concurrency: 1, pageConcurrency: 1, timeout: time.Second, ttl: time.Second,
	}
	if err := serve(ctx, c); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("origin socket survived shutdown: %v", err)
	}
}
