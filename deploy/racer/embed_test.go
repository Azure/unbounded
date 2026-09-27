// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
	"github.com/Azure/unbounded/internal/version"
)

var manifestNames = []string{
	"bootstrap-trust.yaml",
	"config.yaml",
	"controller.yaml",
	"crd/racer.unbounded-cloud.io_clustercaches.yaml",
	"installation.yaml",
	"rbac.yaml",
}

// This contract also runs in separately compiled source fixtures below, so Go's
// embed directive is tested against actual clean and dirty directory layouts.
func TestEmbeddedManifestContract(t *testing.T) {
	require.NoError(t, fstest.TestFS(Manifests, manifestNames...))
	out := t.TempDir()
	require.NoError(t, render.Render(".", out, map[string]string{
		"ControllerImage": "ghcr.io/azure/racer-controller:" + version.Version,
		"DataplaneImage":  "ghcr.io/azure/racer-dataplane:" + version.Version,
	}))

	var names []string

	require.NoError(t, fs.WalkDir(Manifests, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !entry.IsDir() {
			names = append(names, path)
		}

		return nil
	}))
	require.Equal(t, manifestNames, names)

	for _, name := range manifestNames {
		expectedPath := filepath.Join(out, name)
		if strings.HasPrefix(name, "crd/") {
			expectedPath = name
		}

		expected, err := os.ReadFile(expectedPath)
		require.NoError(t, err)
		actual, err := fs.ReadFile(Manifests, name)
		require.NoError(t, err)
		require.Equal(t, string(expected), string(actual), name)
	}
}

func TestIgnoredRenderedTreesCannotShadowManifests(t *testing.T) {
	// Scratch directories can contain unrelated modules. Reproduce one at the
	// subprocess's TMPDIR and give the fixture an explicit nested module so Go
	// never discovers that unrelated go.mod (or a developer's go.work).
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "tmp"), 0o755))
	scratch, err := os.MkdirTemp(filepath.Join(root, "tmp"), "racer-embed-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(scratch)) })

	unrelatedModule := []byte("module example.invalid/unrelated-scratch\n\ngo 1.26.6\n")
	require.NoError(t, os.WriteFile(filepath.Join(scratch, "go.mod"), unrelatedModule, 0o600))
	fixture := filepath.Join(scratch, "fixture")
	require.NoError(t, os.Mkdir(fixture, 0o755))
	// Reuse the repository's dependency versions and checksums. The nested
	// module path retains access to internal packages through a local replace.
	module, err := os.ReadFile(filepath.Join(root, "go.mod"))
	require.NoError(t, err)

	moduleText := strings.Replace(string(module), "module github.com/Azure/unbounded", "module github.com/Azure/unbounded/embedfixture", 1)
	moduleText += "\nrequire github.com/Azure/unbounded v0.0.0\nreplace github.com/Azure/unbounded => " + strconv.Quote(root) + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(fixture, "go.mod"), []byte(moduleText), 0o600))
	checksums, err := os.ReadFile(filepath.Join(root, "go.sum"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(fixture, "go.sum"), checksums, 0o600))

	sources, err := filepath.Glob("*.yaml.tmpl")
	require.NoError(t, err)

	sources = append(sources, "embed.go", "embed_test.go", "crd/racer.unbounded-cloud.io_clustercaches.yaml")
	for _, source := range sources {
		data, err := os.ReadFile(source)
		require.NoError(t, err)

		path := filepath.Join(fixture, source)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, data, 0o600))
	}

	for _, layout := range []string{"absent", "clean", "rendered", "nested-rendered"} {
		t.Run(layout, func(t *testing.T) {
			if layout == "clean" {
				require.NoError(t, os.MkdirAll(filepath.Join(fixture, "rendered"), 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(fixture, "rendered/.gitignore"), []byte("*\n!.gitignore\n"), 0o600))
			}

			if layout == "rendered" || layout == "nested-rendered" {
				directory := "rendered"
				if layout == "nested-rendered" {
					directory = "rendered/rendered"
				}

				for _, name := range append(append([]string{}, manifestNames...), "unexpected.yaml") {
					path := filepath.Join(fixture, directory, name)
					require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
					require.NoError(t, os.WriteFile(path, []byte("ignored output must not be embedded\n"), 0o600))
				}
			}

			cmd := exec.CommandContext(t.Context(), "go", "test", "-mod=readonly", "-count=1", "-run=^TestEmbeddedManifestContract$", ".")
			cmd.Dir = fixture
			cmd.Env = append(cmd.Environ(), "GOWORK=off", "TMPDIR="+scratch)
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", output)
		})
	}

	remaining, err := os.ReadFile(filepath.Join(scratch, "go.mod"))
	require.NoError(t, err)
	require.Equal(t, unrelatedModule, remaining)
}
