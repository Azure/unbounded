// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package utilio

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestIsDirEmpty(t *testing.T) {
	t.Parallel()

	t.Run("non-existent directory", func(t *testing.T) {
		t.Parallel()

		empty, err := IsDirEmpty(filepath.Join(t.TempDir(), "does-not-exist"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !empty {
			t.Fatal("expected true for non-existent directory")
		}
	})

	t.Run("empty directory", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()

		empty, err := IsDirEmpty(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !empty {
			t.Fatal("expected true for empty directory")
		}
	})

	t.Run("directory with a file", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()

		if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hello"), 0o644); err != nil {
			t.Fatalf("setup: %v", err)
		}

		empty, err := IsDirEmpty(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if empty {
			t.Fatal("expected false for directory with a file")
		}
	})

	t.Run("directory with a subdirectory", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()

		if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
			t.Fatalf("setup: %v", err)
		}

		empty, err := IsDirEmpty(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if empty {
			t.Fatal("expected false for directory with a subdirectory")
		}
	})

	t.Run("path is a file", func(t *testing.T) {
		t.Parallel()

		f := filepath.Join(t.TempDir(), "file.txt")

		if err := os.WriteFile(f, []byte("hello"), 0o644); err != nil {
			t.Fatalf("setup: %v", err)
		}

		_, err := IsDirEmpty(f)
		if err == nil {
			t.Fatal("expected error when path is a file")
		}
	})
}

func TestUpdateSymlink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	firstTarget := filepath.Join(dir, "first")
	secondTarget := filepath.Join(dir, "second")
	linkPath := filepath.Join(dir, "nested", "current")

	if err := os.WriteFile(firstTarget, []byte("first"), 0o644); err != nil {
		t.Fatalf("setup first target: %v", err)
	}

	if err := os.WriteFile(secondTarget, []byte("second"), 0o644); err != nil {
		t.Fatalf("setup second target: %v", err)
	}

	if err := UpdateSymlink(linkPath, firstTarget); err != nil {
		t.Fatalf("update symlink to first target: %v", err)
	}

	info, err := os.Stat(filepath.Dir(linkPath))
	if err != nil {
		t.Fatalf("stat created link directory: %v", err)
	}

	if got := info.Mode().Perm(); got != 0o750 {
		t.Fatalf("link directory mode = %o, want %o", got, 0o750)
	}

	target, err := filepath.EvalSymlinks(linkPath)
	if err != nil {
		t.Fatalf("resolve first symlink target: %v", err)
	}

	if target != firstTarget {
		t.Fatalf("first target = %q, want %q", target, firstTarget)
	}

	if err := UpdateSymlink(linkPath, secondTarget); err != nil {
		t.Fatalf("update symlink to second target: %v", err)
	}

	target, err = filepath.EvalSymlinks(linkPath)
	if err != nil {
		t.Fatalf("resolve second symlink target: %v", err)
	}

	if target != secondTarget {
		t.Fatalf("second target = %q, want %q", target, secondTarget)
	}
}

func TestProbeWritableDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := ProbeWritableDir(dir); err != nil {
		t.Fatalf("probe writable dir: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}

	if len(entries) != 0 {
		t.Fatalf("probe left entries behind: %v", entries)
	}
}

func TestNearestExistingDir(t *testing.T) {
	root := t.TempDir()

	existing := filepath.Join(root, "opt")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// A path that exists resolves to itself.
	if got := NearestExistingDir(os.Stat, existing); got != existing {
		t.Fatalf("existing dir: got %q, want %q", got, existing)
	}

	// A path that does not exist resolves to its nearest existing ancestor,
	// which is what lets preflight ask whether it could be created.
	missing := filepath.Join(existing, "unbounded", "bin")
	if got := NearestExistingDir(os.Stat, missing); got != existing {
		t.Fatalf("missing dir: got %q, want %q", got, existing)
	}

	// A file is not a directory, so the walk continues past it.
	filePath := filepath.Join(existing, "file")
	if err := os.WriteFile(filePath, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if got := NearestExistingDir(os.Stat, filepath.Join(filePath, "child")); got != existing {
		t.Fatalf("through file: got %q, want %q", got, existing)
	}

	// The walk terminates at the filesystem root rather than looping.
	if got := NearestExistingDir(func(string) (fs.FileInfo, error) {
		return nil, fs.ErrNotExist
	}, "/a/b/c"); got != "/" {
		t.Fatalf("unterminated walk: got %q, want %q", got, "/")
	}
}
