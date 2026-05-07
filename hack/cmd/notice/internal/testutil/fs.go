// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package testutil provides shared helpers for hermetic notice-tool tests.
package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

// WriteTree materializes a synthetic file layout under root from a path-to-content
// map. Paths are relative to root and use forward slashes; intermediate directories
// are created automatically. Test failures call t.Fatal.
func WriteTree(t *testing.T, root string, files map[string]string) {
	t.Helper()

	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))

		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}

		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
}
