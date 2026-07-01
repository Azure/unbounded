// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package artifacts

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
	"oras.land/oras-go/v2/content/file"

	"github.com/Azure/unbounded/internal/agentartifacts"
)

func TestNewPlan(t *testing.T) {
	plan, err := NewPlan(Options{
		OutputDir: t.TempDir(),
		Manifest: agentartifacts.Manifest{
			Versions: agentartifacts.Versions{
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

	require.Equal(t, agentartifacts.Manifest{
		SchemaVersion: 1,
		Versions: agentartifacts.Versions{
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
	require.Equal(t, agentartifacts.Manifest{
		SchemaVersion: 1,
		Versions: agentartifacts.Versions{
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
	require.NoError(t, writeManifest(dir, agentartifacts.Manifest{
		Versions: agentartifacts.Versions{
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
	require.NoError(t, writeManifest(dir, agentartifacts.Manifest{
		SchemaVersion: 1,
		Versions: agentartifacts.Versions{
			Kubernetes: "v1.34.2",
			Containerd: "2.1.8",
			Runc:       "1.5.0",
			CNI:        "1.5.1",
			Crictl:     "1.34.0",
		},
	}))

	data, err := os.ReadFile(filepath.Join(dir, agentartifacts.ManifestFileName))
	require.NoError(t, err)
	require.NotContains(t, string(data), "schemaVersion")
}

func TestValidatePulledBundle(t *testing.T) {
	dir := t.TempDir()
	manifest := agentartifacts.Manifest{
		SchemaVersion: 1,
		Versions: agentartifacts.Versions{
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
	require.NoError(t, writeManifest(dir, agentartifacts.Manifest{
		SchemaVersion: 1,
		Versions: agentartifacts.Versions{
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

func TestPackPlatformManifestIncludesOnlyOneArchitecture(t *testing.T) {
	dir := t.TempDir()
	manifest := agentartifacts.Manifest{
		SchemaVersion: 1,
		Versions: agentartifacts.Versions{
			Kubernetes: "v1.34.2",
			Containerd: "2.1.8",
			Runc:       "1.5.0",
			CNI:        "1.5.1",
			Crictl:     "1.34.0",
		},
	}
	require.NoError(t, writeManifest(dir, manifest))

	for _, arch := range []string{"amd64", "arm64"} {
		plan, err := NewPlan(Options{OutputDir: dir, Manifest: manifest, Architectures: []string{arch}})
		require.NoError(t, err)

		for _, artifact := range plan.Artifacts {
			path := filepath.Join(dir, filepath.FromSlash(artifact.Path))
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			require.NoError(t, os.WriteFile(path, []byte("content for "+artifact.Path), 0o644))

			if artifact.GenerateChecksum {
				require.NoError(t, writeGeneratedChecksum(path))
			}
		}
	}

	store, err := file.New(dir)
	require.NoError(t, err)

	defer store.Close() //nolint:errcheck // test cleanup

	desc, err := packPlatformManifest(t.Context(), store, manifest, "amd64", map[string]ocispec.Descriptor{})
	require.NoError(t, err)
	require.Equal(t, "linux", desc.Platform.OS)
	require.Equal(t, "amd64", desc.Platform.Architecture)

	rc, err := store.Fetch(t.Context(), desc)
	require.NoError(t, err)

	defer rc.Close() //nolint:errcheck // test cleanup

	var ociManifest ocispec.Manifest
	require.NoError(t, json.NewDecoder(rc).Decode(&ociManifest))

	layerTitles := map[string]struct{}{}
	for _, layer := range ociManifest.Layers {
		layerTitles[layer.Annotations[ocispec.AnnotationTitle]] = struct{}{}
	}

	require.Contains(t, layerTitles, "kubernetes/v1.34.2/bin/linux/amd64/kubelet")
	require.NotContains(t, layerTitles, "kubernetes/v1.34.2/bin/linux/arm64/kubelet")
	require.Contains(t, layerTitles, "containerd/v2.1.8/containerd-2.1.8-linux-amd64.tar.gz")
	require.NotContains(t, layerTitles, "containerd/v2.1.8/containerd-2.1.8-linux-arm64.tar.gz")
	require.Contains(t, layerTitles, ManifestFileName)
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
