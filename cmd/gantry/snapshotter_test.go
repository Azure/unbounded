// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunSnapshotterRejectsSelfFallback(t *testing.T) {
	err := runSnapshotter([]string{"--fallback-snapshotter=gantry-overlaybd"})
	if err == nil || !strings.Contains(err.Error(), "cannot be gantry-overlaybd") {
		t.Fatalf("runSnapshotter() error = %v", err)
	}
}

func TestRunSnapshotterRejectsUnsupportedFallback(t *testing.T) {
	err := runSnapshotter([]string{"--fallback-snapshotter=native"})
	if err == nil || !strings.Contains(err.Error(), "unsupported local fallback") {
		t.Fatalf("runSnapshotter() error = %v", err)
	}
}

func TestListenSnapshotterReplacesStaleSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshotter.sock")

	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}

	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}

	listener, err := listenSnapshotter(path)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = listener.Close() })

	if info, err := os.Stat(path); err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o660 {
		t.Fatalf("socket info = %v, %v", info, err)
	}
}

func TestListenSnapshotterPreservesNonSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshotter.sock")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := listenSnapshotter(path); err == nil {
		t.Fatal("non-socket was replaced")
	}

	if data, err := os.ReadFile(path); err != nil || string(data) != "keep" {
		t.Fatalf("preserved file = %q, %v", data, err)
	}
}
