// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package utilio

import (
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

func TestGetEnvOrDefault(t *testing.T) {
	t.Run("default when unset", func(t *testing.T) {
		if got := GetEnvOrDefault("UNBOUNDED_TEST_UNSET", "default"); got != "default" {
			t.Fatalf("GetEnvOrDefault() = %q, want %q", got, "default")
		}
	})

	t.Run("trimmed value when set", func(t *testing.T) {
		t.Setenv("UNBOUNDED_TEST_SET", " value ")

		if got := GetEnvOrDefault("UNBOUNDED_TEST_SET", "default"); got != "value" {
			t.Fatalf("GetEnvOrDefault() = %q, want %q", got, "value")
		}
	})

	t.Run("default when blank", func(t *testing.T) {
		t.Setenv("UNBOUNDED_TEST_BLANK", " ")

		if got := GetEnvOrDefault("UNBOUNDED_TEST_BLANK", "default"); got != "default" {
			t.Fatalf("GetEnvOrDefault() = %q, want %q", got, "default")
		}
	})

	t.Run("default when empty", func(t *testing.T) {
		t.Setenv("UNBOUNDED_TEST_EMPTY", "")

		if got := GetEnvOrDefault("UNBOUNDED_TEST_EMPTY", "default"); got != "default" {
			t.Fatalf("GetEnvOrDefault() = %q, want %q", got, "default")
		}
	})
}
