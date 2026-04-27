// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gomod

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/unbounded/hack/cmd/notice/internal/testutil"
)

// fakeFS reads from an in-memory map keyed by absolute path. Other paths
// fall through to the real filesystem so go/modfile parsing of go.mod works
// against a real on-disk file we wrote into a t.TempDir().
type fakeFS struct{}

func (fakeFS) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

// fakeRunner returns canned `go mod download -json` output keyed on the
// "<modPath>@<version>" argument.
type fakeRunner struct {
	t      *testing.T
	canned map[string]goModDownload
}

func (f *fakeRunner) Run(name string, args ...string) ([]byte, error) {
	if name != "go" {
		f.t.Fatalf("unexpected command %q", name)
	}

	// args = ["mod", "download", "-json", "-x", "<path>@<ver>"]
	if len(args) < 5 {
		f.t.Fatalf("unexpected args %v", args)
	}

	spec := args[len(args)-1]

	info, ok := f.canned[spec]
	if !ok {
		return nil, fmt.Errorf("no canned response for %s", spec)
	}

	return json.Marshal(info)
}

func TestCollector_Collect_Hermetic(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "cache", "spf13", "cobra@v1.10.2")

	testutil.WriteTree(t, root, map[string]string{
		"go.mod": `module example.com/repo

go 1.24

require github.com/spf13/cobra v1.10.2
`,
		"cache/spf13/cobra@v1.10.2/LICENSE.txt": testutil.MITLicense("Copyright (c) 2013 Steve Francia"),
	})

	runner := &fakeRunner{
		t: t,
		canned: map[string]goModDownload{
			"github.com/spf13/cobra@v1.10.2": {
				Path:    "github.com/spf13/cobra",
				Version: "v1.10.2",
				Dir:     cache,
			},
		},
	}

	c := New(WithRunner(runner), WithFS(fakeFS{}))

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
	if e.Dependency != "github.com/spf13/cobra" {
		t.Errorf("Dependency = %q", e.Dependency)
	}

	if e.Ecosystem != "go" {
		t.Errorf("Ecosystem = %q", e.Ecosystem)
	}

	if len(e.Copyright) != 1 || !strings.Contains(e.Copyright[0], "Steve Francia") {
		t.Errorf("Copyright = %#v", e.Copyright)
	}

	if len(e.License) != 1 || e.License[0].Name != "MIT License" {
		t.Errorf("License = %#v", e.License)
	}

	wantLink := "https://github.com/spf13/cobra/blob/v1.10.2/LICENSE.txt"
	if e.License[0].Link != wantLink {
		t.Errorf("Link = %q, want %q", e.License[0].Link, wantLink)
	}
}

func TestCollector_Collect_SkipsIndirect(t *testing.T) {
	root := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		"go.mod": `module example.com/repo

go 1.24

require github.com/foo/bar v1.0.0 // indirect
`,
	})

	c := New(WithRunner(&fakeRunner{t: t, canned: nil}), WithFS(fakeFS{}))

	entries, err := c.Collect(root)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if len(entries) != 0 {
		t.Errorf("got %d entries, want 0", len(entries))
	}
}

func TestCollector_Precheck_MissingGoMod(t *testing.T) {
	root := t.TempDir()
	c := New(WithRunner(&fakeRunner{t: t}), WithFS(fakeFS{}))

	if err := c.Precheck(root); err == nil {
		t.Errorf("expected error for missing go.mod")
	}
}
