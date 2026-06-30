// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package artifacts

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewPlan(t *testing.T) {
	plan, err := NewPlan(Options{
		OutputDir: t.TempDir(),
		Manifest: Manifest{
			Versions: Versions{
				Kubernetes: "1.34.2",
				Containerd: "2.1.8",
				Runc:       "1.5.0",
				CNI:        "1.5.1",
				Crictl:     "1.34.0",
			},
		},
		Architectures: []string{"amd64"},
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
	}, plan.Manifest)

	artifactsByPath := map[string]Artifact{}
	for _, artifact := range plan.Artifacts {
		artifactsByPath[artifact.Path] = artifact
	}

	require.Contains(t, artifactsByPath, "kubernetes/v1.34.2/bin/linux/amd64/kubelet")
	require.Contains(t, artifactsByPath, "kubernetes/v1.34.2/bin/linux/amd64/kubelet.sha256")
	require.Contains(t, artifactsByPath, "containerd/v2.1.8/containerd-2.1.8-linux-amd64.tar.gz")
	require.Contains(t, artifactsByPath, "runc/v1.5.0/runc.amd64")
	require.Contains(t, artifactsByPath, "cni/v1.5.1/cni-plugins-linux-amd64-v1.5.1.tgz")
	require.Contains(t, artifactsByPath, "crictl/v1.34.0/crictl-v1.34.0-linux-amd64.tar.gz")
	require.Equal(t,
		"https://dl.k8s.io/v1.34.2/bin/linux/amd64/kubelet",
		artifactsByPath["kubernetes/v1.34.2/bin/linux/amd64/kubelet"].URL,
	)
}

func TestNewPlanUsesDefaultManifestFromKubernetesVersion(t *testing.T) {
	plan, err := NewPlan(Options{
		OutputDir:         t.TempDir(),
		KubernetesVersion: "1.34.2",
		Architectures:     []string{"amd64"},
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
	}, plan.Manifest)
}

func TestNewPlanLoadsManifestFile(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.json")
	require.NoError(t, writeManifest(dir, Manifest{
		Versions: Versions{
			Kubernetes: "v1.34.2",
			Containerd: "2.1.8",
			Runc:       "1.5.0",
			CNI:        "1.5.1",
			Crictl:     "1.34.0",
		},
	}))

	plan, err := NewPlan(Options{
		OutputDir:     t.TempDir(),
		ManifestPath:  manifestPath,
		Architectures: []string{"amd64"},
	})
	require.NoError(t, err)
	require.Equal(t, "v1.34.2", plan.Manifest.Versions.Kubernetes)
}

func TestNewPlanRequiresManifestVersions(t *testing.T) {
	_, err := NewPlan(Options{OutputDir: t.TempDir()})
	require.ErrorContains(t, err, "manifest is missing required fields")
}
