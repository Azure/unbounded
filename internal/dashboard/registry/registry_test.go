// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package registry

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, contents string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "modules.yaml")

	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}

	return path
}

func TestLoadValid(t *testing.T) {
	path := writeTemp(t, `
modules:
  - id: net
    baseURL: http://net:9999/dashboard/v1
  - id: example
    baseURL: http://example:8090/dashboard/v1
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if len(cfg.Modules) != 2 {
		t.Fatalf("expected 2 modules, got %d", len(cfg.Modules))
	}

	if cfg.Modules[0].ID != "net" {
		t.Errorf("expected first module id net, got %q", cfg.Modules[0].ID)
	}
}

func TestLoadDuplicateID(t *testing.T) {
	path := writeTemp(t, `
modules:
  - id: dup
    baseURL: http://a/dashboard/v1
  - id: dup
    baseURL: http://b/dashboard/v1
`)

	if _, err := Load(path); err == nil {
		t.Fatal("expected duplicate id error, got nil")
	}
}

func TestLoadMissingBaseURL(t *testing.T) {
	path := writeTemp(t, `
modules:
  - id: net
`)

	if _, err := Load(path); err == nil {
		t.Fatal("expected missing baseURL error, got nil")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}
