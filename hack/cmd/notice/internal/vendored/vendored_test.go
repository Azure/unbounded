// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package vendored

import (
	"path/filepath"
	"testing"

	"github.com/Azure/unbounded/hack/cmd/notice/internal/testutil"
)

func TestCollectReadsManifest(t *testing.T) {
	root := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		ManifestPath: `assets:
  - dependency: bootstrap
    copyright:
      - Copyright (c) 2011-2024 The Bootstrap Authors
    license:
      - name: MIT License
        link: https://github.com/twbs/bootstrap/blob/v5.3.3/LICENSE
  - dependency: htmx.org
    copyright:
      - Copyright (c) 2020 Big Sky Software
    license:
      - name: BSD 2-Clause "Simplified" License
        link: https://github.com/bigskysoftware/htmx/blob/v2.0.3/LICENSE
`,
	})

	entries, err := New().Collect(root)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	if entries[0].Dependency != "bootstrap" {
		t.Errorf("entries[0].Dependency = %q", entries[0].Dependency)
	}

	if entries[0].Ecosystem != "vendored" {
		t.Errorf("ecosystem = %q, want vendored", entries[0].Ecosystem)
	}

	if got := entries[1].License[0].Name; got != `BSD 2-Clause "Simplified" License` {
		t.Errorf("entries[1] license = %q", got)
	}
}

func TestCollectMissingManifestIsSoftSuccess(t *testing.T) {
	entries, err := New().Collect(t.TempDir())
	if err != nil {
		t.Fatalf("expected nil error for missing manifest, got %v", err)
	}

	if entries != nil {
		t.Errorf("expected nil entries, got %v", entries)
	}
}

func TestCollectRejectsUnknownFields(t *testing.T) {
	root := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		ManifestPath: `assets:
  - dependency: bootstrap
    bogus: true
    license:
      - name: MIT License
        link: https://example.com/LICENSE
`,
	})

	if _, err := New().Collect(root); err == nil {
		t.Fatal("expected error for unknown field, got nil")
	}
}

func TestCollectValidation(t *testing.T) {
	cases := map[string]string{
		"missing dependency": `assets:
  - license:
      - name: MIT License
        link: https://example.com/LICENSE
`,
		"missing license": `assets:
  - dependency: bootstrap
`,
		"license without link": `assets:
  - dependency: bootstrap
    license:
      - name: MIT License
`,
	}

	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			testutil.WriteTree(t, root, map[string]string{ManifestPath: doc})

			if _, err := New().Collect(root); err == nil {
				t.Fatalf("%s: expected validation error, got nil", name)
			}
		})
	}
}

func TestManifestPathLocation(t *testing.T) {
	want := filepath.Join("hack", "cmd", "notice", "vendored-assets.yaml")
	if ManifestPath != want {
		t.Errorf("ManifestPath = %q, want %q", ManifestPath, want)
	}
}
