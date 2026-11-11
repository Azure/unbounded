// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package contentstore

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// mkfifoBlob places a FIFO with no writer at the digest path for d.
func mkfifoBlob(t *testing.T, dir, d string) {
	t.Helper()

	if err := syscall.Mkfifo(filepath.Join(dir, d[len("sha256:"):]), 0o644); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
}

// finishes fails the test if fn does not return promptly, which is how a
// blocking FIFO open would show up.
func finishes(t *testing.T, fn func()) {
	t.Helper()

	done := make(chan struct{})

	go func() {
		defer close(done)

		fn()
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("open blocked on a FIFO with no writer")
	}
}

func TestOpenRefusesFIFO(t *testing.T) {
	root, dir := newRoot(t)
	d := blobDigest([]byte("fifo"))
	mkfifoBlob(t, dir, d)

	s, _ := New(root) //nolint:errcheck // valid root

	finishes(t, func() {
		if _, _, err := s.Open(d); !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
}

func TestOpenRootFallbackDoesNotBlockOnFIFO(t *testing.T) {
	root, dir := newRoot(t)
	d := blobDigest([]byte("fifo"))
	mkfifoBlob(t, dir, d)

	finishes(t, func() {
		f, err := openRoot(root, filepath.Join("blobs", "sha256", d[len("sha256:"):]))
		if err != nil {
			t.Errorf("openRoot: %v", err)

			return
		}

		info, err := f.Stat()
		_ = f.Close() //nolint:errcheck // test cleanup

		if err != nil {
			t.Errorf("stat: %v", err)

			return
		}

		if info.Mode().IsRegular() {
			t.Error("FIFO reported as a regular file")
		}
	})
}
