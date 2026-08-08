// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package bootstrapartifacts

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLocalBundleAndCompareContents(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "manifest.json"), []byte("{}"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "runc"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "runc", "runc.amd64"), []byte("runc"), 0o644))

	bundle, err := Open(root)
	require.NoError(t, err)

	source, err := bundle.Artifact("manifest.json")
	require.NoError(t, err)
	require.Contains(t, source.String(), "manifest.json")
	require.Contains(t, bundle.ArtifactURL("runc/runc.amd64"), "file://")

	diff, err := CompareContents(context.Background(), bundle, []string{
		"manifest.json",
		"missing",
	})
	require.NoError(t, err)
	require.Equal(t, []string{"missing"}, diff.Missing)
	require.Equal(t, []string{"runc/runc.amd64"}, diff.Unexpected)
}

func TestOCIBundleArtifactURL(t *testing.T) {
	t.Parallel()

	bundle, err := Open("oci://registry.example.test/unbounded/bootstrap:v1")
	require.NoError(t, err)
	require.Equal(t,
		"oci://registry.example.test/unbounded/bootstrap:v1#manifest.json",
		bundle.ArtifactURL("manifest.json"),
	)

	source, err := bundle.Artifact("manifest.json")
	require.NoError(t, err)
	require.Equal(t, bundle.ArtifactURL("manifest.json"), source.String())
}

func TestResolveHTTPSArchiveValidation(t *testing.T) {
	t.Parallel()

	_, _, err := Resolve(context.Background(), "https://artifacts.example.test", ResolveOptions{})
	require.ErrorContains(t, err, "host and archive path")

	_, _, err = Resolve(context.Background(), "https://artifacts.example.test/%zz?sig=secret", ResolveOptions{})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret")
}
