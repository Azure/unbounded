// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package npm

import (
	"strings"
	"testing"

	"github.com/Azure/unbounded/hack/cmd/notice/internal/testutil"
)

func TestCollector_Collect_Hermetic(t *testing.T) {
	root := t.TempDir()

	testutil.WriteTree(t, root, map[string]string{
		"frontend/package.json": `{
  "name":"app",
  "version":"0.0.0",
  "dependencies":{"foo":"^1.0.0"},
  "devDependencies":{"jest":"*"}
}`,
		"frontend/node_modules/foo/package.json": `{
  "name":"foo",
  "version":"1.2.3",
  "license":"MIT",
  "repository":"https://github.com/acme/foo"
}`,
		"frontend/node_modules/foo/LICENSE": testutil.MITLicense("Copyright (c) 2020 Acme Corp"),
	})

	c := New()

	if err := c.Precheck(root); err != nil {
		t.Fatalf("Precheck: %v", err)
	}

	entries, err := c.Collect(root)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}

	e := entries[0]
	if e.Dependency != "foo" {
		t.Errorf("Dependency = %q", e.Dependency)
	}

	if e.Ecosystem != "npm" {
		t.Errorf("Ecosystem = %q", e.Ecosystem)
	}

	if len(e.Copyright) != 1 || !strings.Contains(e.Copyright[0], "Acme Corp") {
		t.Errorf("Copyright = %#v", e.Copyright)
	}

	if len(e.License) != 1 || e.License[0].Name != "MIT License" {
		t.Errorf("License = %#v", e.License)
	}

	if e.License[0].Link != "https://github.com/acme/foo/blob/HEAD/LICENSE" {
		t.Errorf("Link = %q", e.License[0].Link)
	}
}

func TestCollector_Collect_DevDepsExcluded(t *testing.T) {
	root := t.TempDir()

	testutil.WriteTree(t, root, map[string]string{
		"frontend/package.json": `{
  "name":"app",
  "devDependencies":{"jest":"*"}
}`,
		"frontend/node_modules/.placeholder": "x",
	})

	c := New()

	entries, err := c.Collect(root)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if len(entries) != 0 {
		t.Errorf("got %d entries, want 0 (devDependencies must be excluded)", len(entries))
	}
}

func TestCollector_Precheck_NoFrontendIsSoftSuccess(t *testing.T) {
	root := t.TempDir()

	c := New()
	if err := c.Precheck(root); err != nil {
		t.Errorf("expected nil for missing frontend/, got %v", err)
	}
}

func TestCollector_Precheck_FrontendWithoutNodeModules(t *testing.T) {
	root := t.TempDir()

	testutil.WriteTree(t, root, map[string]string{
		"frontend/package.json": `{}`,
	})

	c := New()
	if err := c.Precheck(root); err == nil {
		t.Errorf("expected error for missing node_modules")
	}
}
