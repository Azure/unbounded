// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agentartifacts

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/bootstrapartifacts"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestKubernetesBinary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		override *goalstates.DownloadSource
		version  string
		arch     string
		binary   string
		want     string
	}{
		{
			name:    "default",
			version: "1.33.5",
			arch:    "amd64",
			binary:  "kubelet",
			want:    "https://dl.k8s.io/v1.33.5/bin/linux/amd64/kubelet",
		},
		{
			name:     "base url override",
			override: &goalstates.DownloadSource{BaseURL: "https://mirror.example.com/k8s"},
			version:  "1.33.5",
			arch:     "arm64",
			binary:   "kubectl",
			want:     "https://mirror.example.com/k8s/v1.33.5/bin/linux/arm64/kubectl",
		},
		{
			name:     "base url override strips trailing slash",
			override: &goalstates.DownloadSource{BaseURL: "https://mirror.example.com/k8s/"},
			version:  "1.33.5",
			arch:     "amd64",
			binary:   "kube-proxy",
			want:     "https://mirror.example.com/k8s/v1.33.5/bin/linux/amd64/kube-proxy",
		},
		{
			name:     "url override",
			override: &goalstates.DownloadSource{URL: "https://mirror.example.com/%s/%s/%s"},
			version:  "1.33.5",
			arch:     "amd64",
			binary:   "kubelet",
			want:     "https://mirror.example.com/1.33.5/amd64/kubelet",
		},
		{
			name: "url override wins over base url",
			override: &goalstates.DownloadSource{
				BaseURL: "https://base.example.com/k8s",
				URL:     "https://url.example.com/%s/%s/%s",
			},
			version: "1.33.5",
			arch:    "amd64",
			binary:  "kubelet",
			want:    "https://url.example.com/1.33.5/amd64/kubelet",
		},
	}

	for i := range tests {
		testCase := tests[i]
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := KubernetesBinary(testCase.override, testCase.version, testCase.arch, testCase.binary)
			require.Equal(t, testCase.want, got)
		})
	}
}

func TestContainerdArchive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		override *goalstates.DownloadSource
		version  string
		arch     string
		want     string
	}{
		{
			name:    "default",
			version: "2.0.4",
			arch:    "amd64",
			want:    "https://github.com/containerd/containerd/releases/download/v2.0.4/containerd-2.0.4-linux-amd64.tar.gz",
		},
		{
			name:     "base url override",
			override: &goalstates.DownloadSource{BaseURL: "https://mirror.example.com/containerd/"},
			version:  "2.0.4",
			arch:     "arm64",
			want:     "https://mirror.example.com/containerd/v2.0.4/containerd-2.0.4-linux-arm64.tar.gz",
		},
		{
			name:     "url override",
			override: &goalstates.DownloadSource{URL: "https://mirror.example.com/containerd-%s-%s-%s.tar.gz"},
			version:  "2.0.4",
			arch:     "amd64",
			want:     "https://mirror.example.com/containerd-2.0.4-2.0.4-amd64.tar.gz",
		},
	}

	for i := range tests {
		testCase := tests[i]
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := ContainerdArchive(testCase.override, testCase.version, testCase.arch)
			require.Equal(t, testCase.want, got)
		})
	}
}

func TestRuncBinary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		override *goalstates.DownloadSource
		version  string
		arch     string
		want     string
	}{
		{
			name:    "default",
			version: "1.1.12",
			arch:    "amd64",
			want:    "https://github.com/opencontainers/runc/releases/download/v1.1.12/runc.amd64",
		},
		{
			name:     "base url override",
			override: &goalstates.DownloadSource{BaseURL: "https://mirror.example.com/runc/"},
			version:  "1.1.12",
			arch:     "arm64",
			want:     "https://mirror.example.com/runc/v1.1.12/runc.arm64",
		},
		{
			name:     "url override",
			override: &goalstates.DownloadSource{URL: "https://mirror.example.com/runc/%s/runc.%s"},
			version:  "1.1.12",
			arch:     "amd64",
			want:     "https://mirror.example.com/runc/1.1.12/runc.amd64",
		},
	}

	for i := range tests {
		testCase := tests[i]
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := RuncBinary(testCase.override, testCase.version, testCase.arch)
			require.Equal(t, testCase.want, got)
		})
	}
}

func TestCNIPluginsArchive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		override *goalstates.DownloadSource
		version  string
		arch     string
		want     string
	}{
		{
			name:    "default",
			version: "1.5.1",
			arch:    "amd64",
			want:    "https://github.com/containernetworking/plugins/releases/download/v1.5.1/cni-plugins-linux-amd64-v1.5.1.tgz",
		},
		{
			name:     "base url override",
			override: &goalstates.DownloadSource{BaseURL: "https://mirror.example.com/cni/"},
			version:  "1.5.1",
			arch:     "arm64",
			want:     "https://mirror.example.com/cni/v1.5.1/cni-plugins-linux-arm64-v1.5.1.tgz",
		},
		{
			name:     "url override",
			override: &goalstates.DownloadSource{URL: "https://mirror.example.com/cni-%s-%s-%s.tgz"},
			version:  "1.5.1",
			arch:     "amd64",
			want:     "https://mirror.example.com/cni-1.5.1-amd64-1.5.1.tgz",
		},
	}

	for i := range tests {
		testCase := tests[i]
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := CNIPluginsArchive(testCase.override, testCase.version, testCase.arch)
			require.Equal(t, testCase.want, got)
		})
	}
}

func TestCrictlArchive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		override *goalstates.DownloadSource
		version  string
		hostOS   string
		hostArch string
		want     string
	}{
		{
			name:     "linux amd64",
			version:  "1.30.4",
			hostOS:   "linux",
			hostArch: "amd64",
			want:     "https://github.com/kubernetes-sigs/cri-tools/releases/download/v1.30.4/crictl-v1.30.4-linux-amd64.tar.gz",
		},
		{
			name:     "darwin arm64",
			version:  "1.30.4",
			hostOS:   "darwin",
			hostArch: "arm64",
			want:     "https://github.com/kubernetes-sigs/cri-tools/releases/download/v1.30.4/crictl-v1.30.4-darwin-arm64.tar.gz",
		},
		{
			name:     "windows amd64",
			version:  "1.30.4",
			hostOS:   "windows",
			hostArch: "amd64",
			want:     "https://github.com/kubernetes-sigs/cri-tools/releases/download/v1.30.4/crictl-v1.30.4-windows-amd64.tar.gz",
		},
		{
			name:     "base url override",
			override: &goalstates.DownloadSource{BaseURL: "https://mirror.example.com/cri-tools"},
			version:  "1.30.4",
			hostOS:   "linux",
			hostArch: "amd64",
			want:     "https://mirror.example.com/cri-tools/v1.30.4/crictl-v1.30.4-linux-amd64.tar.gz",
		},
		{
			name:     "base url override strips trailing slash",
			override: &goalstates.DownloadSource{BaseURL: "https://mirror.example.com/cri-tools/"},
			version:  "1.30.4",
			hostOS:   "linux",
			hostArch: "amd64",
			want:     "https://mirror.example.com/cri-tools/v1.30.4/crictl-v1.30.4-linux-amd64.tar.gz",
		},
		{
			name:     "full url override",
			override: &goalstates.DownloadSource{URL: "https://mirror.example.com/crictl/%s/crictl-v%s-%s-%s.tgz"},
			version:  "1.30.4",
			hostOS:   "linux",
			hostArch: "amd64",
			want:     "https://mirror.example.com/crictl/1.30.4/crictl-v1.30.4-linux-amd64.tgz",
		},
	}

	for i := range tests {
		testCase := tests[i]
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := CrictlArchive(testCase.override, testCase.version, testCase.hostOS, testCase.hostArch)
			require.Equal(t, testCase.want, got)
		})
	}
}

func TestContainerImageArchivePath(t *testing.T) {
	t.Parallel()

	got := bootstrapartifacts.ContainerImageArchivePath("amd64", "mcr.microsoft.com/oss/v2/kubernetes/pause:3.9")
	require.Equal(t, "container-images/amd64/mcr.microsoft.com_oss_v2_kubernetes_pause_3.9-a68ffa05fa78.tar", got)
}

func TestNormalizeManifest(t *testing.T) {
	t.Parallel()

	got, err := bootstrapartifacts.NormalizeManifest(bootstrapartifacts.Manifest{
		Versions: bootstrapartifacts.Versions{
			Kubernetes: "1.34.2",
			Containerd: "v2.1.8",
			Runc:       "v1.5.0",
			CNI:        "v1.5.1",
			Crictl:     "v1.34.0",
		},
	})
	require.NoError(t, err)
	require.Equal(t, bootstrapartifacts.Manifest{
		SchemaVersion: 1,
		Versions: bootstrapartifacts.Versions{
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

	got, err := bootstrapartifacts.NormalizeManifest(bootstrapartifacts.Manifest{
		Versions: bootstrapartifacts.Versions{
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

	_, err := bootstrapartifacts.NormalizeManifest(bootstrapartifacts.Manifest{SchemaVersion: 2})
	require.ErrorContains(t, err, "unsupported manifest schemaVersion 2")
}

func TestNormalizeManifestRequiresVersions(t *testing.T) {
	t.Parallel()

	_, err := bootstrapartifacts.NormalizeManifest(bootstrapartifacts.Manifest{})
	require.ErrorContains(t, err, "manifest is missing required fields")
	require.ErrorContains(t, err, "versions.kubernetes")
	require.ErrorContains(t, err, "versions.containerd")
	require.ErrorContains(t, err, "versions.runc")
	require.ErrorContains(t, err, "versions.cni")
	require.ErrorContains(t, err, "versions.crictl")
}

func TestCrictlVersionForKubernetesVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		kubernetesVersion string
		want              string
		wantErr           bool
	}{
		{
			name:              "exact semver maps to minor patch zero",
			kubernetesVersion: "1.30.4",
			want:              "1.30.0",
		},
		{
			name:              "leading v prefix maps to minor patch zero",
			kubernetesVersion: "v1.31.2",
			want:              "1.31.0",
		},
		{
			name:              "prerelease suffix",
			kubernetesVersion: "1.32.0-rc.1",
			want:              "1.32.0",
		},
		{
			name:              "non zero patch maps to zero",
			kubernetesVersion: "1.33.1",
			want:              "1.33.0",
		},
		{
			name:              "missing patch defaults to zero",
			kubernetesVersion: "1.32",
			want:              "1.32.0",
		},
		{
			name:              "invalid version",
			kubernetesVersion: "abc",
			wantErr:           true,
		},
	}

	for i := range tests {
		testCase := tests[i]
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got, err := CrictlVersionForKubernetesVersion(testCase.kubernetesVersion)
			if testCase.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, testCase.want, got)
		})
	}
}
