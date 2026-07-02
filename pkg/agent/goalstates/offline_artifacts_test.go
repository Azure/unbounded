// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/config"
)

func TestResolveMachineUsesAgentConfigOfflineArtifacts(t *testing.T) {
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

	got, err := ResolveMachine(discardLogger(), &config.AgentConfig{
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
	}, "kube1", &DownloadOverrides{
		Runc: &DownloadSource{BaseURL: "https://ignored.example.test/runc"},
	})

	require.NoError(t, err)
	require.NotNil(t, got.RootFS.Downloads)
	require.Equal(t, "1.5.0", got.RootFS.Downloads.Runc.Version)
	require.Contains(t, got.RootFS.Downloads.Runc.URL, "file://")
	require.NotContains(t, got.RootFS.Downloads.Runc.URL, "ignored")
	require.Equal(t, SandboxImage, got.NodeStart.Containerd.SandboxImage)
	require.Len(t, got.NodeStart.Containerd.ContainerImageArchiveURLs, 2)
	require.Contains(t, got.NodeStart.Containerd.ContainerImageArchiveURLs[0], "file://")
}

func writeGoalStateOfflineBundle(t *testing.T, manifest OfflineArtifactManifest) string {
	t.Helper()

	root := t.TempDir()
	manifest.SchemaVersion = 1
	manifest.Versions.Kubernetes = normalizeKubernetesVersion(manifest.Versions.Kubernetes)
	manifest.Versions.Containerd = stripLeadingV(manifest.Versions.Containerd)
	manifest.Versions.Runc = stripLeadingV(manifest.Versions.Runc)
	manifest.Versions.CNI = stripLeadingV(manifest.Versions.CNI)
	manifest.Versions.Crictl = stripLeadingV(manifest.Versions.Crictl)

	data, err := json.Marshal(manifest)
	require.NoError(t, err)
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
