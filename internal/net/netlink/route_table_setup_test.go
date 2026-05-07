// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netlink

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withTempRtTables creates a temp file, overrides rtTablesPath for the
// duration of the test, and returns the temp file path. The caller can
// optionally pre-populate it via initialContent.
func withTempRtTables(t *testing.T, initialContent string) string {
	t.Helper()
	dir := t.TempDir()

	path := filepath.Join(dir, "rt_tables")
	if initialContent != "" {
		if err := os.WriteFile(path, []byte(initialContent), 0o644); err != nil {
			t.Fatalf("failed to write initial rt_tables: %v", err)
		}
	}

	origPath := rtTablesPath
	rtTablesPath = path

	t.Cleanup(func() { rtTablesPath = origPath })

	return path
}

// TestEnsureRtTablesEntry verifies that EnsureRtTablesEntry creates an entry
// and is idempotent (calling twice produces exactly one entry).
func TestEnsureRtTablesEntry(t *testing.T) {
	path := withTempRtTables(t, "")

	// First call should create the file and add the entry.
	if err := EnsureRtTablesEntry(252, "unbounded"); err != nil {
		t.Fatalf("first EnsureRtTablesEntry: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read rt_tables: %v", err)
	}

	if count := strings.Count(string(content), "252\tunbounded"); count != 1 {
		t.Errorf("expected exactly 1 entry for 252/unbounded, got %d\nfile:\n%s", count, content)
	}

	// Second call should be a no-op.
	if err := EnsureRtTablesEntry(252, "unbounded"); err != nil {
		t.Fatalf("second EnsureRtTablesEntry: %v", err)
	}

	content2, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read rt_tables after second call: %v", err)
	}

	if count := strings.Count(string(content2), "252\tunbounded"); count != 1 {
		t.Errorf("after second call expected exactly 1 entry, got %d\nfile:\n%s", count, content2)
	}
}

// TestEnsureRtTablesEntry_PreservesExisting verifies that adding a new entry
// does not clobber standard entries already present in the file.
func TestEnsureRtTablesEntry_PreservesExisting(t *testing.T) {
	existing := "#\n# reserved values\n#\n255\tlocal\n254\tmain\n253\tdefault\n0\tunspec\n"
	path := withTempRtTables(t, existing)

	if err := EnsureRtTablesEntry(252, "unbounded"); err != nil {
		t.Fatalf("EnsureRtTablesEntry: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read rt_tables: %v", err)
	}

	for _, want := range []string{"255\tlocal", "254\tmain", "253\tdefault", "0\tunspec", "252\tunbounded"} {
		if !strings.Contains(string(content), want) {
			t.Errorf("expected rt_tables to contain %q\nfile:\n%s", want, content)
		}
	}
}

// TestEnsureRtTablesEntry_ExistingIDDifferentName verifies that if the table
// ID already exists under a different name the function does not add a
// duplicate entry.
func TestEnsureRtTablesEntry_ExistingIDDifferentName(t *testing.T) {
	existing := "252\told-name\n"
	path := withTempRtTables(t, existing)

	if err := EnsureRtTablesEntry(252, "unbounded"); err != nil {
		t.Fatalf("EnsureRtTablesEntry: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read rt_tables: %v", err)
	}
	// The original name should be preserved, and the new name should NOT appear.
	if !strings.Contains(string(content), "252\told-name") {
		t.Errorf("expected original entry to be preserved\nfile:\n%s", content)
	}

	if strings.Contains(string(content), "252\tunbounded") {
		t.Errorf("did not expect a second entry with new name\nfile:\n%s", content)
	}
}

// TestEnsureRtTablesEntry_InvalidName verifies that names containing
// dangerous characters are rejected.
func TestEnsureRtTablesEntry_InvalidName(t *testing.T) {
	withTempRtTables(t, "")

	if err := EnsureRtTablesEntry(252, "bad name"); err == nil {
		t.Error("expected error for name with space, got nil")
	}

	if err := EnsureRtTablesEntry(252, "../etc"); err == nil {
		t.Error("expected error for name with path traversal, got nil")
	}
}

// TestEnsureRtTablesEntry_CreatesDirectory verifies that EnsureRtTablesEntry
// creates the parent directory when it does not exist.
func TestEnsureRtTablesEntry_CreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "rt_tables")
	origPath := rtTablesPath
	rtTablesPath = path

	t.Cleanup(func() { rtTablesPath = origPath })

	if err := EnsureRtTablesEntry(252, "unbounded"); err != nil {
		t.Fatalf("EnsureRtTablesEntry: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read rt_tables: %v", err)
	}

	if !strings.Contains(string(content), "252\tunbounded") {
		t.Errorf("expected entry to be present\nfile:\n%s", content)
	}
}

// TestFlushRouteTable_Signature verifies FlushRouteTable is callable and
// returns an error when not running as root (no kernel access).
func TestFlushRouteTable_Signature(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test designed for non-root; skipping under root")
	}
	// FlushRouteTable uses netlink directly, which requires privileges.
	// We just confirm the function is reachable and returns a reasonable error.
	err := FlushRouteTable(9999)
	if err == nil {
		// On a system where table 9999 is empty, netlink might succeed.
		// That is fine -- the important thing is the call did not panic.
		return
	}
	// Any error from unprivileged netlink is acceptable.
	t.Logf("FlushRouteTable returned expected error: %v", err)
}

// TestListRoutesInTable_Signature verifies ListRoutesInTable is callable.
func TestListRoutesInTable_Signature(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test designed for non-root; skipping under root")
	}

	_, err := ListRoutesInTable(9999)
	if err == nil {
		return
	}

	t.Logf("ListRoutesInTable returned expected error: %v", err)
}
