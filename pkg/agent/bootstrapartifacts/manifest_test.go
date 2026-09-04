// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package bootstrapartifacts

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeManifest(t *testing.T) {
	t.Parallel()

	got, err := NormalizeManifest(Manifest{
		Versions: Versions{
			Kubernetes: "1.34.2",
			Containerd: "v2.1.8",
			Runc:       "v1.5.0",
			CNI:        "v1.5.1",
			Crictl:     "v1.34.0",
		},
	})
	require.NoError(t, err)
	require.Equal(t, Manifest{
		SchemaVersion: 1,
		Versions: Versions{
			Kubernetes: "v1.34.2",
			Containerd: "2.1.8",
			Runc:       "1.5.0",
			CNI:        "1.5.1",
			Crictl:     "1.34.0",
		},
		ContainerImages: []string{},
	}, got)
}

func TestNormalizeManifestPreservesContainerImages(t *testing.T) {
	t.Parallel()

	got, err := NormalizeManifest(Manifest{
		Versions: Versions{
			Kubernetes: "v1.34.2",
			Containerd: "2.1.8",
			Runc:       "1.5.0",
			CNI:        "1.5.1",
			Crictl:     "1.34.0",
		},
		ContainerImages: []string{" registry.example.com/pause:3.9 ", "", "registry.example.com/kube-proxy:v1.34.2", "registry.example.com/pause:3.9"},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"registry.example.com/kube-proxy:v1.34.2", "registry.example.com/pause:3.9"}, got.ContainerImages)
}

func TestNormalizeManifestRejectsUnsupportedSchema(t *testing.T) {
	t.Parallel()

	_, err := NormalizeManifest(Manifest{SchemaVersion: 2})
	require.ErrorContains(t, err, "unsupported manifest schemaVersion 2")
}

func TestNormalizeManifestRequiresVersions(t *testing.T) {
	t.Parallel()

	_, err := NormalizeManifest(Manifest{})
	require.ErrorContains(t, err, "manifest is missing required fields")
	require.ErrorContains(t, err, "versions.kubernetes")
	require.ErrorContains(t, err, "versions.containerd")
	require.ErrorContains(t, err, "versions.runc")
	require.ErrorContains(t, err, "versions.cni")
	require.ErrorContains(t, err, "versions.crictl")
}
