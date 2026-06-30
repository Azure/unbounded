// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package artifacts

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	containerd := artifactsByPath["containerd/v2.1.8/containerd-2.1.8-linux-amd64.tar.gz"]
	runc := artifactsByPath["runc/v1.5.0/runc.amd64"]
	cni := artifactsByPath["cni/v1.5.1/cni-plugins-linux-amd64-v1.5.1.tgz"]
	crictl := artifactsByPath["crictl/v1.34.0/crictl-v1.34.0-linux-amd64.tar.gz"]

	require.True(t, containerd.GenerateChecksum)
	require.True(t, runc.GenerateChecksum)
	require.True(t, cni.GenerateChecksum)
	require.True(t, crictl.GenerateChecksum)
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

func TestWriteManifestOmitsV1SchemaVersion(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, writeManifest(dir, Manifest{
		SchemaVersion: 1,
		Versions: Versions{
			Kubernetes: "v1.34.2",
			Containerd: "2.1.8",
			Runc:       "1.5.0",
			CNI:        "1.5.1",
			Crictl:     "1.34.0",
		},
	}))

	data, err := os.ReadFile(filepath.Join(dir, ManifestFileName))
	require.NoError(t, err)
	require.NotContains(t, string(data), "schemaVersion")
}

func TestValidatePulledBundle(t *testing.T) {
	dir := t.TempDir()
	manifest := Manifest{
		SchemaVersion: 1,
		Versions: Versions{
			Kubernetes: "v1.34.2",
			Containerd: "2.1.8",
			Runc:       "1.5.0",
			CNI:        "1.5.1",
			Crictl:     "1.34.0",
		},
	}
	require.NoError(t, writeManifest(dir, manifest))

	plan, err := NewPlan(Options{OutputDir: dir, Manifest: manifest, Architectures: []string{"amd64"}})
	require.NoError(t, err)

	for _, artifact := range plan.Artifacts {
		if strings.HasSuffix(artifact.Path, ".sha256") {
			continue
		}

		path := filepath.Join(dir, filepath.FromSlash(artifact.Path))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("content for "+artifact.Path), 0o644))

		if artifact.GenerateChecksum || artifact.Name == "kubelet" || artifact.Name == "kubectl" || artifact.Name == "kube-proxy" {
			require.NoError(t, writeGeneratedChecksum(path))
		}
	}

	require.NoError(t, validateBundle(dir))
}

func TestValidatePulledBundleDetectsContentMismatch(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, writeManifest(dir, Manifest{
		SchemaVersion: 1,
		Versions: Versions{
			Kubernetes: "v1.34.2",
			Containerd: "2.1.8",
			Runc:       "1.5.0",
			CNI:        "1.5.1",
			Crictl:     "1.34.0",
		},
	}))

	err := validateBundle(dir)
	require.ErrorContains(t, err, "read Kubernetes artifact architecture dir")
}

func TestWriteGeneratedChecksum(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "artifact.bin")
	content := []byte("artifact content")
	require.NoError(t, os.WriteFile(path, content, 0o644))

	require.NoError(t, writeGeneratedChecksum(path))

	checksum, err := os.ReadFile(path + ".sha256")
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("%x  artifact.bin\n", sha256.Sum256(content)), string(checksum))
}

func TestNewPlanRequiresManifestVersions(t *testing.T) {
	_, err := NewPlan(Options{OutputDir: t.TempDir()})
	require.ErrorContains(t, err, "manifest is missing required fields")
}
