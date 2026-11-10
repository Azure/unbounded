// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package contentstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func blobDigest(data []byte) string {
	sum := sha256.Sum256(data)

	return "sha256:" + hex.EncodeToString(sum[:])
}

func newRoot(t *testing.T) (string, string) {
	t.Helper()

	root := t.TempDir()
	dir := filepath.Join(root, "blobs", "sha256")

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	return root, dir
}

func writeBlob(t *testing.T, dir string, data []byte) string {
	t.Helper()

	d := blobDigest(data)
	if err := os.WriteFile(filepath.Join(dir, d[len("sha256:"):]), data, 0o644); err != nil {
		t.Fatal(err)
	}

	return d
}

func TestNewRejectsBadRoots(t *testing.T) {
	for _, root := range []string{"", "relative", "/a/../b", "/a/"} {
		if _, err := New(root); err == nil {
			t.Errorf("New(%q) succeeded", root)
		}
	}
}

func TestOpenPresent(t *testing.T) {
	root, dir := newRoot(t)
	data := []byte("hello content store")
	d := writeBlob(t, dir, data)

	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}

	f, size, err := s.Open(d)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // test cleanup

	if size != int64(len(data)) {
		t.Fatalf("size = %d, want %d", size, len(data))
	}

	got, err := io.ReadAll(f)
	if err != nil || string(got) != string(data) {
		t.Fatalf("read = %q, %v", got, err)
	}
}

func TestOpenEmptyBlob(t *testing.T) {
	root, dir := newRoot(t)
	d := writeBlob(t, dir, nil)
	s, _ := New(root) //nolint:errcheck // valid root

	f, size, err := s.Open(d)
	if err != nil || size != 0 {
		t.Fatalf("Open = %d, %v", size, err)
	}

	_ = f.Close() //nolint:errcheck // test cleanup
}

func TestOpenMissing(t *testing.T) {
	root, _ := newRoot(t)
	s, _ := New(root) //nolint:errcheck // valid root

	if _, _, err := s.Open(blobDigest([]byte("absent"))); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}

	missingRoot, _ := New(filepath.Join(root, "nope")) //nolint:errcheck // valid root
	if _, _, err := missingRoot.Open(blobDigest([]byte("absent"))); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing root err = %v, want ErrNotFound", err)
	}
}

func TestOpenInvalidDigest(t *testing.T) {
	root, _ := newRoot(t)
	s, _ := New(root) //nolint:errcheck // valid root
	good := blobDigest([]byte("x"))

	for _, d := range []string{
		"",
		good[len("sha256:"):],
		"sha512:" + good[len("sha256:"):],
		"sha256:" + good[len("sha256:")+1:],
		"sha256:" + good[len("sha256:"):] + "0",
		"SHA256:" + good[len("sha256:"):],
		"sha256:" + "A" + good[len("sha256:")+1:],
		"sha256:../../../../../../etc/passwd" + good[len("sha256:")+35:],
		"sha256:" + "../" + good[len("sha256:")+3:],
	} {
		if _, _, err := s.Open(d); !errors.Is(err, ErrInvalidDigest) {
			t.Errorf("Open(%q) err = %v, want ErrInvalidDigest", d, err)
		}
	}
}

func TestOpenRefusesSymlink(t *testing.T) {
	root, dir := newRoot(t)
	outside := filepath.Join(t.TempDir(), "secret")

	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	d := blobDigest([]byte("secret"))
	if err := os.Symlink(outside, filepath.Join(dir, d[len("sha256:"):])); err != nil {
		t.Fatal(err)
	}

	s, _ := New(root) //nolint:errcheck // valid root
	if f, _, err := s.Open(d); err == nil {
		_ = f.Close() //nolint:errcheck // test cleanup

		t.Fatal("Open followed a symlink")
	}
}

func TestOpenRefusesSymlinkedDirectory(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	data := []byte("outside blob")
	d := writeBlob(t, outside, data)

	if err := os.Symlink(outside, filepath.Join(root, "blobs")); err != nil {
		t.Fatal(err)
	}

	if err := os.Mkdir(filepath.Join(outside, "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}

	writeBlob(t, filepath.Join(outside, "sha256"), data)

	s, _ := New(root) //nolint:errcheck // valid root
	if f, _, err := s.Open(d); err == nil {
		_ = f.Close() //nolint:errcheck // test cleanup

		t.Fatal("Open followed a symlinked directory")
	}
}

func TestOpenRefusesDirectory(t *testing.T) {
	root, dir := newRoot(t)
	d := blobDigest([]byte("dir"))

	if err := os.Mkdir(filepath.Join(dir, d[len("sha256:"):]), 0o755); err != nil {
		t.Fatal(err)
	}

	s, _ := New(root) //nolint:errcheck // valid root
	if _, _, err := s.Open(d); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestOpenRootFallbackRefusesSymlink(t *testing.T) {
	root, dir := newRoot(t)
	outside := filepath.Join(t.TempDir(), "secret")

	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	if f, err := openRoot(root, filepath.Join("blobs", "sha256", "link")); err == nil {
		_ = f.Close() //nolint:errcheck // test cleanup

		t.Fatal("openRoot followed a symlink")
	}

	if err := os.WriteFile(filepath.Join(dir, "plain"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	f, err := openRoot(root, filepath.Join("blobs", "sha256", "plain"))
	if err != nil {
		t.Fatal(err)
	}

	_ = f.Close() //nolint:errcheck // test cleanup
}
