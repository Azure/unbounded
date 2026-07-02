// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package provision

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/agentartifacts"
)

func TestResolveOfflineArtifacts(t *testing.T) {
	root := writeOfflineBundle(t, agentartifacts.Manifest{
		Versions: agentartifacts.Versions{
			Kubernetes: "v1.34.2",
			Containerd: "2.1.8",
			Runc:       "1.5.0",
			CNI:        "1.5.1",
			Crictl:     "1.34.0",
		},
		ContainerImages: []string{"mcr.microsoft.com/oss/kubernetes/pause:3.9"},
	})

	resolved, err := ResolveOfflineArtifacts(
		&AgentConfig{Cluster: AgentClusterConfig{Version: "1.34.2"}},
		&AgentOfflineArtifacts{Source: root},
	)
	require.NoError(t, err)
	require.Equal(t, root, resolved.SourceRoot)
	require.Equal(t, "v1.34.2", resolved.Manifest.Versions.Kubernetes)
	require.Equal(t, []string{"mcr.microsoft.com/oss/kubernetes/pause:3.9"}, resolved.Manifest.ContainerImages)
}

func TestResolveOfflineArtifactsRendersStrictTemplate(t *testing.T) {
	root := writeOfflineBundle(t, agentartifacts.Manifest{Versions: agentartifacts.Versions{
		Kubernetes: "v1.34.2",
		Containerd: "2.1.8",
		Runc:       "1.5.0",
		CNI:        "1.5.1",
		Crictl:     "1.34.0",
	}})
	parent := filepath.Dir(root)

	cfg := &AgentConfig{Cluster: AgentClusterConfig{Version: "1.34.2"}}
	resolved, err := ResolveOfflineArtifacts(cfg, &AgentOfflineArtifacts{Source: filepath.Join(parent, "{{ .KubernetesVersion }}")})
	require.NoError(t, err)
	require.Equal(t, root, resolved.SourceRoot)

	_, err = ResolveOfflineArtifacts(cfg, &AgentOfflineArtifacts{Source: filepath.Join(parent, "{{ .Typo }}")})
	require.ErrorContains(t, err, "render OfflineArtifacts.Source template")
}

func TestResolveOfflineArtifactsRejectsVersionMismatch(t *testing.T) {
	root := writeOfflineBundle(t, agentartifacts.Manifest{Versions: agentartifacts.Versions{
		Kubernetes: "v1.34.2",
		Containerd: "2.1.8",
		Runc:       "1.5.0",
		CNI:        "1.5.1",
		Crictl:     "1.34.0",
	}})

	_, err := ResolveOfflineArtifacts(
		&AgentConfig{Cluster: AgentClusterConfig{Version: "1.35.0"}},
		&AgentOfflineArtifacts{Source: root},
	)
	require.ErrorContains(t, err, "does not match Cluster.Version")
}

func TestResolveOfflineArtifactsRejectsRuntimeConflict(t *testing.T) {
	root := writeOfflineBundle(t, agentartifacts.Manifest{Versions: agentartifacts.Versions{
		Kubernetes: "v1.34.2",
		Containerd: "2.1.8",
		Runc:       "1.5.0",
		CNI:        "1.5.1",
		Crictl:     "1.34.0",
	}})

	_, err := ResolveOfflineArtifacts(
		&AgentConfig{
			Cluster: AgentClusterConfig{Version: "1.34.2"},
			CRI:     CRIConfig{Containerd: ContainerdConfig{Version: "2.1.9"}},
		},
		&AgentOfflineArtifacts{Source: root},
	)
	require.ErrorContains(t, err, "conflicts with offline manifest")
}

func TestResolveOfflineArtifactsRequiresExistingFiles(t *testing.T) {
	root := writeOfflineBundle(t, agentartifacts.Manifest{Versions: agentartifacts.Versions{
		Kubernetes: "v1.34.2",
		Containerd: "2.1.8",
		Runc:       "1.5.0",
		CNI:        "1.5.1",
		Crictl:     "1.34.0",
	}})
	require.NoError(t, os.Remove(filepath.Join(root, "runc", "v1.5.0", "runc."+runtime.GOARCH)))

	_, err := ResolveOfflineArtifacts(
		&AgentConfig{Cluster: AgentClusterConfig{Version: "1.34.2"}},
		&AgentOfflineArtifacts{Source: root},
	)
	require.ErrorContains(t, err, "required offline artifact")
}

func writeOfflineBundle(t *testing.T, manifest agentartifacts.Manifest) string {
	t.Helper()

	manifest, err := agentartifacts.NormalizeManifest(manifest)
	require.NoError(t, err)

	root := filepath.Join(t.TempDir(), manifest.Versions.Kubernetes)
	data, err := json.Marshal(manifest)
	require.NoError(t, err)
	writeFile(t, root, agentartifacts.ManifestFileName, data)

	arch := runtime.GOARCH
	for _, binary := range agentartifacts.KubernetesBinaries {
		path := agentartifacts.KubernetesArtifactPath(manifest.Versions.Kubernetes, arch, binary)
		content := []byte(binary + "\n")
		writeFile(t, root, path, content)
		sum := sha256.Sum256(content)
		writeFile(t, root, path+".sha256", []byte(hex.EncodeToString(sum[:])))
	}

	writeFile(t, root, agentartifacts.ContainerdArtifactPath(manifest.Versions.Containerd, arch), []byte("containerd"))
	writeFile(t, root, agentartifacts.RuncArtifactPath(manifest.Versions.Runc, arch), []byte("runc"))
	writeFile(t, root, agentartifacts.CNIArtifactPath(manifest.Versions.CNI, arch), []byte("cni"))
	writeFile(t, root, agentartifacts.CrictlArtifactPath(manifest.Versions.Crictl, "linux", arch), []byte("crictl"))

	for _, imageTag := range manifest.ContainerImages {
		path := agentartifacts.ContainerImageArchivePath(arch, imageTag)
		content := []byte("image")
		writeFile(t, root, path, content)

		sum := sha256.Sum256(content)
		writeFile(t, root, path+".sha256", []byte(hex.EncodeToString(sum[:])))
	}

	return root
}

func writeFile(t *testing.T, root, rel string, content []byte) {
	t.Helper()

	path := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, content, 0o644))
}
