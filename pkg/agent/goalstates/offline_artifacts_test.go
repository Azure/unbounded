// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/config"
)

func TestResolveDownloadOverridesWithOfflineArtifacts(t *testing.T) {
	root := writeGoalStateOfflineBundle(t, OfflineArtifactManifest{
		Versions: OfflineArtifactVersions{
			Kubernetes: "v1.34.2",
			Containerd: "2.1.8",
			Runc:       "1.5.0",
			CNI:        "1.5.1",
			Crictl:     "1.34.0",
		},
		ContainerImages: []string{SandboxImage, KubeProxyImage("v1.34.2")},
	})

	cfg := &config.AgentConfig{
		MachineName: "machine-1",
		NodeName:    "node-1",
		Cluster: config.AgentClusterConfig{
			CaCertBase64: "Y2EtYnl0ZXM=",
			ClusterDNS:   "10.96.0.10",
			Version:      "1.34.2",
		},
		Kubelet: config.AgentKubeletConfig{
			ApiServer: "https://api.example.com",
		},
		OfflineArtifacts: &config.AgentOfflineArtifacts{
			Source: root,
		},
	}

	downloads, containerImageArchives, err := ResolveDownloadOverridesWithOfflineArtifacts(context.Background(), cfg, &DownloadOverrides{
		Runc: &DownloadSource{BaseURL: "https://ignored.example.test/runc"},
	})
	require.NoError(t, err)
	require.NotNil(t, downloads)
	assertOfflineArtifactDownloads(t, downloads)
	require.NotNil(t, containerImageArchives)
	require.Contains(t, containerImageArchives.HostDir, ContainerImageArchiveHostSourceDir)
	require.Len(t, containerImageArchives.URLs, 2)
	require.Contains(t, containerImageArchives.URLs[0], "file://")

	got, err := ResolveMachine(discardLogger(), cfg, "kube1", downloads)
	require.NoError(t, err)
	require.Equal(t, SandboxImage, got.NodeStart.Containerd.SandboxImage)
	require.Same(t, downloads, got.RootFS.Downloads)
}

func assertOfflineArtifactDownloads(t *testing.T, downloads *DownloadOverrides) {
	t.Helper()

	require.Equal(t, "1.5.0", downloads.Runc.Version)
	require.Contains(t, downloads.Runc.URL, "file://")
	require.NotContains(t, downloads.Runc.URL, "ignored")
}

func TestResolveDownloadOverridesWithOfflineArtifactsNoopWithoutOfflineConfig(t *testing.T) {
	t.Parallel()

	input := &DownloadOverrides{Runc: &DownloadSource{BaseURL: "https://example.test/runc"}}

	got, containerImageArchives, err := ResolveDownloadOverridesWithOfflineArtifacts(context.Background(), &config.AgentConfig{}, input)
	require.NoError(t, err)
	require.Same(t, input, got)
	require.NotNil(t, containerImageArchives)
	require.Equal(t, filepath.Join(ContainerImageArchiveHostSourceDir, "empty"), containerImageArchives.HostDir)
	require.Empty(t, containerImageArchives.URLs)
}

func TestResolveOfflineArtifacts(t *testing.T) {
	root := writeGoalStateOfflineBundle(t, OfflineArtifactManifest{
		Versions: OfflineArtifactVersions{
			Kubernetes: "v1.34.2",
			Containerd: "2.1.8",
			Runc:       "1.5.0",
			CNI:        "1.5.1",
			Crictl:     "1.34.0",
		},
		ContainerImages: []string{"mcr.microsoft.com/oss/kubernetes/pause:3.9"},
	})

	resolved, err := resolveOfflineArtifacts(
		context.Background(),
		&config.AgentConfig{Cluster: config.AgentClusterConfig{Version: "1.34.2"}},
		&config.AgentOfflineArtifacts{Source: root},
	)
	require.NoError(t, err)
	require.Equal(t, root, resolved.SourceRoot)
	require.Equal(t, "v1.34.2", resolved.Manifest.Versions.Kubernetes)
	require.Equal(t, []string{"mcr.microsoft.com/oss/kubernetes/pause:3.9"}, resolved.Manifest.ContainerImages)
}

func TestResolveOfflineArtifactsRendersStrictTemplate(t *testing.T) {
	root := writeGoalStateOfflineBundle(t, OfflineArtifactManifest{Versions: OfflineArtifactVersions{
		Kubernetes: "v1.34.2",
		Containerd: "2.1.8",
		Runc:       "1.5.0",
		CNI:        "1.5.1",
		Crictl:     "1.34.0",
	}})
	parent := filepath.Dir(root)

	cfg := &config.AgentConfig{Cluster: config.AgentClusterConfig{Version: "1.34.2"}}
	resolved, err := resolveOfflineArtifacts(context.Background(), cfg, &config.AgentOfflineArtifacts{Source: filepath.Join(parent, "{{ .KubernetesVersion }}")})
	require.NoError(t, err)
	require.Equal(t, root, resolved.SourceRoot)

	_, err = resolveOfflineArtifacts(context.Background(), cfg, &config.AgentOfflineArtifacts{Source: filepath.Join(parent, "{{ .Typo }}")})
	require.ErrorContains(t, err, "render OfflineArtifacts.Source template")
}

func TestResolveOfflineArtifactsRejectsVersionMismatch(t *testing.T) {
	root := writeGoalStateOfflineBundle(t, OfflineArtifactManifest{Versions: OfflineArtifactVersions{
		Kubernetes: "v1.34.2",
		Containerd: "2.1.8",
		Runc:       "1.5.0",
		CNI:        "1.5.1",
		Crictl:     "1.34.0",
	}})

	_, err := resolveOfflineArtifacts(
		context.Background(),
		&config.AgentConfig{Cluster: config.AgentClusterConfig{Version: "1.35.0"}},
		&config.AgentOfflineArtifacts{Source: root},
	)
	require.ErrorContains(t, err, "does not match Cluster.Version")
}

func TestResolveOfflineArtifactsRejectsRuntimeConflict(t *testing.T) {
	root := writeGoalStateOfflineBundle(t, OfflineArtifactManifest{Versions: OfflineArtifactVersions{
		Kubernetes: "v1.34.2",
		Containerd: "2.1.8",
		Runc:       "1.5.0",
		CNI:        "1.5.1",
		Crictl:     "1.34.0",
	}})

	_, err := resolveOfflineArtifacts(
		context.Background(),
		&config.AgentConfig{
			Cluster: config.AgentClusterConfig{Version: "1.34.2"},
			CRI:     config.CRIConfig{Containerd: config.ContainerdConfig{Version: "2.1.9"}},
		},
		&config.AgentOfflineArtifacts{Source: root},
	)
	require.ErrorContains(t, err, "conflicts with offline manifest")
}

func TestResolveOfflineArtifactsRequiresExistingFiles(t *testing.T) {
	root := writeGoalStateOfflineBundle(t, OfflineArtifactManifest{Versions: OfflineArtifactVersions{
		Kubernetes: "v1.34.2",
		Containerd: "2.1.8",
		Runc:       "1.5.0",
		CNI:        "1.5.1",
		Crictl:     "1.34.0",
	}})
	require.NoError(t, os.Remove(filepath.Join(root, "runc", "v1.5.0", "runc."+runtime.GOARCH)))

	_, err := resolveOfflineArtifacts(
		context.Background(),
		&config.AgentConfig{Cluster: config.AgentClusterConfig{Version: "1.34.2"}},
		&config.AgentOfflineArtifacts{Source: root},
	)
	require.ErrorContains(t, err, "required offline artifact")
}

func writeGoalStateOfflineBundle(t *testing.T, manifest OfflineArtifactManifest) string {
	t.Helper()

	root := filepath.Join(t.TempDir(), normalizeKubernetesVersion(manifest.Versions.Kubernetes))
	manifest.SchemaVersion = 1
	manifest.Versions.Kubernetes = normalizeKubernetesVersion(manifest.Versions.Kubernetes)
	manifest.Versions.Containerd = stripLeadingV(manifest.Versions.Containerd)
	manifest.Versions.Runc = stripLeadingV(manifest.Versions.Runc)
	manifest.Versions.CNI = stripLeadingV(manifest.Versions.CNI)
	manifest.Versions.Crictl = stripLeadingV(manifest.Versions.Crictl)

	data, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, OfflineArtifactManifestFileName), data, 0o644))

	paths := offlineArtifactPaths(manifest, runtime.GOARCH)
	for _, path := range paths {
		if path == OfflineArtifactManifestFileName {
			continue
		}

		fullPath := filepath.Join(root, filepath.FromSlash(path))
		require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o755))
		require.NoError(t, os.WriteFile(fullPath, []byte("test"), 0o644))
	}

	return root
}
