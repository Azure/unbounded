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

// TestNearestExistingDir pins the walk that lets a caller ask whether a path
// could be created without creating it.
func TestNearestExistingDir(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	nested := filepath.Join(root, "a", "b")

	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, tc := range []struct {
		name, path, want string
	}{
		{"existing directory is its own answer", nested, nested},
		{"absent leaf walks up one", filepath.Join(nested, "absent"), nested},
		{"absent subtree walks up to the deepest existing", filepath.Join(nested, "absent", "deeper"), nested},
		{"absent intermediate walks past it", filepath.Join(root, "missing", "bin"), root},
		{"a file is not a directory", file, root},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := NearestExistingDir(os.Stat, tc.path); got != tc.want {
				t.Fatalf("NearestExistingDir(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}

	// The walk terminates at the filesystem root rather than looping forever
	// when nothing on the path exists.
	t.Run("terminates at the root", func(t *testing.T) {
		t.Parallel()

		neverExists := func(string) (fs.FileInfo, error) { return nil, os.ErrNotExist }
		if got := NearestExistingDir(neverExists, "/definitely/not/here"); got != "/" {
			t.Fatalf("NearestExistingDir = %q, want /", got)
		}
	})
}
