// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
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
	// Keep the fixture inside this module so it uses the real dependencies and
	// version package without downloading or maintaining a separate go.mod.
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "tmp"), 0o755))
	fixture, err := os.MkdirTemp(filepath.Join(root, "tmp"), "racer-embed-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(fixture)) })

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

			cmd := exec.CommandContext(t.Context(), "go", "test", "-count=1", "-run=^TestEmbeddedManifestContract$", ".")
			cmd.Dir = fixture
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", output)
		})
	}
}
