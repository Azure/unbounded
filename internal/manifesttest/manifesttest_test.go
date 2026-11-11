// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package manifesttest

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// repoRoot is the repository root, relative to this package's directory.
const repoRoot = "../.."

func placeholderOnly() fs.FS {
	return fstest.MapFS{".gitignore": {Data: []byte("*\n!.gitignore\n")}}
}

func setNames(sets []Set) []string {
	names := make([]string, 0, len(sets))
	for _, set := range sets {
		names = append(names, set.Dir)
	}

	return names
}

func TestUnrendered(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		fsys fs.FS
		want bool
	}{
		{name: "placeholder only, as on a fresh clone", fsys: placeholderOnly(), want: false},
		{name: "empty", fsys: fstest.MapFS{}, want: false},
		{
			name: "files that are not manifests",
			fsys: fstest.MapFS{".gitignore": {}, "README.md": {}, "values.yaml.tmpl": {}},
			want: false,
		},
		{name: "a top-level manifest", fsys: fstest.MapFS{"01-namespace.yaml": {}}, want: true},
		{name: "a nested manifest", fsys: fstest.MapFS{"crd/machines.yaml": {}}, want: true},
		{name: "the .yml extension", fsys: fstest.MapFS{"deployment.yml": {}}, want: true},
		{name: "an upper-case extension", fsys: fstest.MapFS{"deployment.YAML": {}}, want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			missing, err := Unrendered(Set{Dir: "deploy/x/rendered", FS: tc.fsys})
			if err != nil {
				t.Fatalf("Unrendered: %v", err)
			}

			if rendered := len(missing) == 0; rendered != tc.want {
				t.Errorf("rendered = %v, want %v", rendered, tc.want)
			}
		})
	}
}

func TestUnrenderedReturnsOnlyTheMissingSetsInOrder(t *testing.T) {
	t.Parallel()

	a := Set{Dir: "deploy/a/rendered", FS: placeholderOnly()}
	b := Set{Dir: "deploy/b/rendered", FS: fstest.MapFS{"b.yaml": {}}}
	c := Set{Dir: "deploy/c/rendered", FS: placeholderOnly()}

	missing, err := Unrendered(a, b, c)
	if err != nil {
		t.Fatalf("Unrendered: %v", err)
	}

	if got, want := setNames(missing), []string{a.Dir, c.Dir}; !slices.Equal(got, want) {
		t.Errorf("missing = %q, want %q", got, want)
	}
}

func TestUnrenderedReportsAnUnreadableSet(t *testing.T) {
	t.Parallel()

	_, err := Unrendered(Set{Dir: "deploy/x/rendered", FS: os.DirFS(filepath.Join(t.TempDir(), "absent"))})
	if err == nil {
		t.Fatal("Unrendered: want an error for a directory that cannot be read")
	}

	if !strings.Contains(err.Error(), "deploy/x/rendered") {
		t.Errorf("error = %q, want it to name the directory", err)
	}
}

// fakeRunner stands in for *testing.M.
type fakeRunner struct {
	runs int
	code int
}

func (f *fakeRunner) Run() int {
	f.runs++

	return f.code
}

func TestRunFailsBeforeAnyTestWhenASetIsMissing(t *testing.T) {
	t.Parallel()

	m := &fakeRunner{}

	code, out := run(m, []Set{
		{Dir: "deploy/machina/rendered", Target: "machina-manifests", FS: placeholderOnly()},
		{Dir: "deploy/gantry/rendered", Target: "gantry-manifests", FS: fstest.MapFS{"a.yaml": {}}},
		{Dir: "deploy/net/rendered", Target: "net-manifests", FS: placeholderOnly()},
	})

	if code == 0 {
		t.Error("exit code = 0, want a failure")
	}

	if m.runs != 0 {
		t.Errorf("tests ran %d times, want 0", m.runs)
	}

	for _, want := range []string{
		"deploy/machina/rendered has no manifests",
		"deploy/net/rendered has no manifests",
		"make machina-manifests net-manifests",
		"make test",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("message does not contain %q:\n%s", want, out)
		}
	}

	if strings.Contains(out, "gantry") {
		t.Errorf("message names a rendered set:\n%s", out)
	}
}

func TestRunFailsWhenASetCannotBeRead(t *testing.T) {
	t.Parallel()

	m := &fakeRunner{}

	code, out := run(m, []Set{{Dir: "deploy/x/rendered", FS: os.DirFS(filepath.Join(t.TempDir(), "absent"))}})

	if code == 0 || m.runs != 0 {
		t.Errorf("exit code = %d, runs = %d; want a failure and no tests run", code, m.runs)
	}

	if !strings.Contains(out, "deploy/x/rendered") {
		t.Errorf("message does not name the directory:\n%s", out)
	}
}

func TestRunRunsTheTestsWhenEverySetIsRendered(t *testing.T) {
	t.Parallel()

	m := &fakeRunner{code: 3}

	code, out := run(m, []Set{{Dir: "deploy/x/rendered", FS: fstest.MapFS{"x.yaml": {}}}})

	if m.runs != 1 {
		t.Errorf("tests ran %d times, want 1", m.runs)
	}

	if code != 3 {
		t.Errorf("exit code = %d, want the tests' own code 3", code)
	}

	if out != "" {
		t.Errorf("unexpected message: %s", out)
	}
}

var predefined = map[string]Set{
	"Gantry":            Gantry,
	"Machina":           Machina,
	"Net":               Net,
	"TokenRefresher":    TokenRefresher,
	"UnboundedOperator": UnboundedOperator,
}

// TestPredefinedSetsMatchTheRepository pins each set's directory and make
// target to the repository, so a rename cannot leave the message pointing at a
// directory or target that no longer exists.
func TestPredefinedSetsMatchTheRepository(t *testing.T) {
	t.Parallel()

	makefile, err := os.ReadFile(filepath.Join(repoRoot, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}

	for name, set := range predefined {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			placeholder := filepath.Join(repoRoot, filepath.FromSlash(set.Dir), ".gitignore")
			if _, err := os.Stat(placeholder); err != nil {
				t.Errorf("%s: no tracked placeholder: %v", set.Dir, err)
			}

			target := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(set.Target) + `:`)
			if !target.Match(makefile) {
				t.Errorf("Makefile has no %q target", set.Target)
			}
		})
	}
}

// TestPredefinedSetsEmbedTheirDirectory pins each set's FS to its Dir: the
// embedded YAML files are exactly the ones on disk. This holds whether or not
// the manifests have been rendered, because both are read from the same tree.
func TestPredefinedSetsEmbedTheirDirectory(t *testing.T) {
	t.Parallel()

	for name, set := range predefined {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			embedded := yamlFiles(t, set.FS)
			onDisk := yamlFiles(t, os.DirFS(filepath.Join(repoRoot, filepath.FromSlash(set.Dir))))

			if !slices.Equal(embedded, onDisk) {
				t.Errorf("embedded YAML files differ from %s:\nembedded: %q\non disk:  %q", set.Dir, embedded, onDisk)
			}
		})
	}
}

func yamlFiles(t *testing.T, fsys fs.FS) []string {
	t.Helper()

	var files []string

	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if ext := strings.ToLower(filepath.Ext(name)); !d.IsDir() && (ext == ".yaml" || ext == ".yml") {
			files = append(files, name)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	return files
}
