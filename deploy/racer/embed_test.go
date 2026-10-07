// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
)

var manifestNames = []string{
	"config.yaml",
	"controller-pdb.yaml",
	"controller.yaml",
	"create-restriction.yaml",
	"installation.yaml",
	"node-restriction.yaml",
	"rbac.yaml",
}

// Retain the manifest contract for standalone rendering, not a custom embedded FS.
func TestEmbeddedManifestContract(t *testing.T) {
	crd, err := os.ReadFile("crd/racer.unbounded-cloud.io_clustercaches.yaml")
	require.NoError(t, err)

	var definition struct {
		Spec struct {
			Names struct {
				Kind       string
				ListKind   string
				Plural     string
				Singular   string
				ShortNames []string
			}
			Scope    string
			Versions []struct {
				AdditionalPrinterColumns []any
				Schema                   struct {
					OpenAPIV3Schema struct {
						Properties map[string]any
						Required   []string
					}
				}
			}
		}
	}

	require.NoError(t, yaml.Unmarshal(crd, &definition))
	require.Equal(t, "ClusterCache", definition.Spec.Names.Kind)
	require.Equal(t, "ClusterCacheList", definition.Spec.Names.ListKind)
	require.Equal(t, "clustercaches", definition.Spec.Names.Plural)
	require.Equal(t, "clustercache", definition.Spec.Names.Singular)
	require.Equal(t, []string{"ccache"}, definition.Spec.Names.ShortNames)
	require.Equal(t, "Cluster", definition.Spec.Scope)
	require.Len(t, definition.Spec.Versions, 1)
	require.Empty(t, definition.Spec.Versions[0].AdditionalPrinterColumns)
	require.NotContains(t, definition.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties, "spec")
	require.NotContains(t, definition.Spec.Versions[0].Schema.OpenAPIV3Schema.Required, "spec")
	out := t.TempDir()
	require.NoError(t, render.Render(".", out, map[string]string{"ControllerImage": "registry/controller:test"}))
	require.NoError(t, fstest.TestFS(os.DirFS(out), manifestNames...))

	var names []string

	require.NoError(t, fs.WalkDir(os.DirFS(out), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !entry.IsDir() {
			names = append(names, path)
		}

		return nil
	}))
	require.Equal(t, manifestNames, names)
}

func TestIgnoredRenderedTreesCannotShadowManifests(t *testing.T) {
	fixture := t.TempDir()
	sources, err := filepath.Glob("*.yaml.tmpl")
	require.NoError(t, err)

	for _, source := range sources {
		data, err := os.ReadFile(source)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(fixture, source), data, 0o600))
	}

	baseline := t.TempDir()
	require.NoError(t, render.Render(fixture, baseline, nil))

	for _, layout := range []string{"absent", "clean", "rendered", "nested-rendered"} {
		t.Run(layout, func(t *testing.T) {
			if layout != "absent" {
				directory := filepath.Join(fixture, "rendered")
				if layout == "nested-rendered" {
					directory = filepath.Join(directory, "rendered")
				}

				require.NoError(t, os.MkdirAll(directory, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(directory, ".gitignore"), []byte("*\n!.gitignore\n"), 0o600))

				if strings.Contains(layout, "rendered") {
					for _, name := range append(append([]string{}, manifestNames...), "unexpected.yaml") {
						require.NoError(t, os.WriteFile(filepath.Join(directory, name), []byte("ignored output must not shadow templates\n"), 0o600))
					}
				}
			}

			out := t.TempDir()
			require.NoError(t, render.Render(fixture, out, nil))

			for _, name := range manifestNames {
				expected, err := os.ReadFile(filepath.Join(baseline, name))
				require.NoError(t, err)
				actual, err := os.ReadFile(filepath.Join(out, name))
				require.NoError(t, err)
				require.Equal(t, expected, actual, name)
			}

			require.NoError(t, fstest.TestFS(os.DirFS(out), manifestNames...))
			_, err := os.Stat(filepath.Join(out, "rendered"))
			require.ErrorIs(t, err, fs.ErrNotExist)
		})
	}
}
