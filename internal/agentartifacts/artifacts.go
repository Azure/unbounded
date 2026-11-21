// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package agentartifacts defines the agent bootstrap artifact manifest, paths,
// and source URL resolution used by the agent and offline artifact tooling.
package agentartifacts

import (
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"

	"github.com/Azure/unbounded/pkg/agent/bootstrapartifacts"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

const (
	// KubernetesDefaultBaseURL is the upstream base URL for Kubernetes
	// binary releases. Mirrors must preserve the <base>/v<ver>/bin/linux/<arch>/
	// layout used by dl.k8s.io.
	KubernetesDefaultBaseURL = "https://dl.k8s.io"

	// ContainerdDefaultBaseURL is the upstream base URL for containerd releases.
	// Mirrors must preserve the <base>/v<ver>/<asset> layout.
	ContainerdDefaultBaseURL = "https://github.com/containerd/containerd/releases/download"

	// RuncDefaultBaseURL is the upstream base URL for runc releases.
	// Mirrors must preserve the <base>/v<ver>/<asset> layout.
	RuncDefaultBaseURL = "https://github.com/opencontainers/runc/releases/download"

	// CNIDefaultBaseURL is the upstream base URL for CNI plugin releases.
	// Mirrors must preserve the <base>/v<ver>/<asset> layout.
	CNIDefaultBaseURL = "https://github.com/containernetworking/plugins/releases/download"

	// CrictlDefaultBaseURL is the upstream base URL for cri-tools releases.
	CrictlDefaultBaseURL = "https://github.com/kubernetes-sigs/cri-tools/releases/download"

	// CoreDNSDefaultBaseURL is the upstream base URL for CoreDNS releases.
	CoreDNSDefaultBaseURL = "https://github.com/coredns/coredns/releases/download"

	// ACRProviderDefaultBaseURL is the upstream base URL for the AKS
	// acr-credential-provider releases. Mirrors must preserve the
	// <base>/v<ver>/azure-acr-credential-provider-linux-<arch> layout used by
	// cloud-provider-azure GitHub releases.
	ACRProviderDefaultBaseURL = "https://github.com/kubernetes-sigs/cloud-provider-azure/releases/download"

	// ACRProviderVersion is the cloud-provider-azure release that ships the
	// stock acr-credential-provider the managed ACR authentication installs.
	ACRProviderVersion = "v1.36.0"
)

// ACRProviderSHA256 maps each architecture to the SHA256 of that
// architecture's acr-credential-provider binary, pinned to ACRProviderVersion.
// The hashes match the ones the unbounded-operator image verifies against.
var ACRProviderSHA256 = map[string]string{
	"amd64": "b8f9a8137640685ab06cf5ffc4d37f0525534654c057973630f73485442ea21b",
	"arm64": "8097badaccae6cd9e3f90ea5520a2a2343e2484c0dc6aa7835f5d33f8a6b2d5d",
}

func DefaultContainerImages(kubernetesVersion string) []string {
	return bootstrapartifacts.NormalizeContainerImages([]string{
		goalstates.SandboxImage,
		goalstates.KubeProxyImage(kubernetesVersion),
	})
}

// KubernetesBinary resolves the download URL for a Kubernetes binary
// (kubelet, kubectl, kube-proxy) honoring the optional override.
func KubernetesBinary(override *goalstates.DownloadSource, version, arch, binary string) string {
	if override != nil && override.URL != "" {
		return fmt.Sprintf(override.URL, version, arch, binary)
	}

	base := KubernetesDefaultBaseURL
	if override != nil && override.BaseURL != "" {
		base = strings.TrimRight(override.BaseURL, "/")
	}

	return fmt.Sprintf("%s/v%s/bin/linux/%s/%s", base, bootstrapartifacts.StripLeadingV(version), arch, binary)
}

// ContainerdArchive resolves the containerd release tarball URL, honoring
// BaseURL / URL overrides. The upstream path-and-filename layout
// (containerd-<ver>-linux-<arch>.tar.gz) is preserved so mirrors must publish
// under the same structure.
func ContainerdArchive(override *goalstates.DownloadSource, version, arch string) string {
	if override != nil && override.URL != "" {
		return fmt.Sprintf(override.URL, version, version, arch)
	}

	version = bootstrapartifacts.StripLeadingV(version)

	base := ContainerdDefaultBaseURL
	if override != nil && override.BaseURL != "" {
		base = strings.TrimRight(override.BaseURL, "/")
	}

	return fmt.Sprintf("%s/v%s/containerd-%s-linux-%s.tar.gz", base, version, version, arch)
}

// RuncBinary resolves the runc binary URL, honoring BaseURL / URL overrides.
// The upstream filename (runc.<arch>) is preserved.
func RuncBinary(override *goalstates.DownloadSource, version, arch string) string {
	if override != nil && override.URL != "" {
		return fmt.Sprintf(override.URL, version, arch)
	}

	version = bootstrapartifacts.StripLeadingV(version)

	base := RuncDefaultBaseURL
	if override != nil && override.BaseURL != "" {
		base = strings.TrimRight(override.BaseURL, "/")
	}

	return fmt.Sprintf("%s/v%s/runc.%s", base, version, arch)
}

// CNIPluginsArchive resolves the CNI plugins tarball URL honoring the optional
// override. Mirrors must publish under <base>/v<ver>/<asset>.
func CNIPluginsArchive(override *goalstates.DownloadSource, version, arch string) string {
	if override != nil && override.URL != "" {
		return fmt.Sprintf(override.URL, version, arch, version)
	}

	version = bootstrapartifacts.StripLeadingV(version)

	base := CNIDefaultBaseURL
	if override != nil && override.BaseURL != "" {
		base = strings.TrimRight(override.BaseURL, "/")
	}

	return fmt.Sprintf("%s/v%s/cni-plugins-linux-%s-v%s.tgz", base, version, arch, version)
}

// CrictlArchive resolves the cri-tools crictl tarball URL honoring the
// optional override. Mirrors must publish assets under the same
// <base>/v<ver>/<asset> layout as GitHub releases.
func CrictlArchive(override *goalstates.DownloadSource, version, hostOS, hostArch string) string {
	if override != nil && override.URL != "" {
		return fmt.Sprintf(override.URL, version, version, hostOS, hostArch)
	}

	version = bootstrapartifacts.StripLeadingV(version)

	base := CrictlDefaultBaseURL
	if override != nil && override.BaseURL != "" {
		base = strings.TrimRight(override.BaseURL, "/")
	}

	return fmt.Sprintf("%s/v%s/crictl-v%s-%s-%s.tar.gz", base, version, version, hostOS, hostArch)
}

// CoreDNSArchive resolves the CoreDNS release archive URL.
func CoreDNSArchive(override *goalstates.DownloadSource, version, arch string) string {
	if override != nil && override.URL != "" {
		return fmt.Sprintf(override.URL, version, arch)
	}

	version = bootstrapartifacts.StripLeadingV(version)

	base := CoreDNSDefaultBaseURL
	if override != nil && override.BaseURL != "" {
		base = strings.TrimRight(override.BaseURL, "/")
	}

	return fmt.Sprintf("%s/v%s/coredns_%s_linux_%s.tgz", base, version, version, arch)
}

// ACRProviderBinary resolves the download URL for the stock ACR credential
// provider binary, honoring the optional override. The asset is a single
// upstream release (ACRProviderVersion) rather than a versioned node binary, so
// the version does not follow the cluster Kubernetes version.
func ACRProviderBinary(override *goalstates.DownloadSource, arch string) string {
	return ACRProviderBinaryForVersion(override, ACRProviderVersion, arch)
}

// ACRProviderBinaryForVersion resolves the download URL for the stock ACR
// credential provider binary at an explicit version (with or without a leading
// "v"), honoring the optional override. The offline bundle producer uses this
// to materialize the provider at the version the bundle manifest records, rather
// than the agent's default pinned release.
func ACRProviderBinaryForVersion(override *goalstates.DownloadSource, version, arch string) string {
	if override != nil && override.URL != "" {
		return fmt.Sprintf(override.URL, arch)
	}

	base := ACRProviderDefaultBaseURL
	if override != nil && override.BaseURL != "" {
		base = strings.TrimRight(override.BaseURL, "/")
	}

	version = "v" + bootstrapartifacts.StripLeadingV(version)

	return fmt.Sprintf("%s/%s/azure-acr-credential-provider-linux-%s", base, version, arch)
}

// ACRProviderSHA256For returns the pinned SHA256 of the ACR credential provider
// binary for the architecture, or an error for an unsupported architecture.
func ACRProviderSHA256For(arch string) (string, error) {
	hash, ok := ACRProviderSHA256[arch]
	if !ok {
		return "", fmt.Errorf("no ACR credential provider binary hash for architecture %q", arch)
	}

	return hash, nil
}

// CrictlVersionForKubernetesVersion returns the cri-tools version for the
// Kubernetes major.minor release. cri-tools releases are published as
// v<major>.<minor>.0.
func CrictlVersionForKubernetesVersion(kubernetesVersion string) (string, error) {
	version, err := semver.NewVersion(strings.TrimSpace(kubernetesVersion))
	if err != nil {
		return "", fmt.Errorf("parse kubernetes version %q: %w", kubernetesVersion, err)
	}

	return fmt.Sprintf("%d.%d.0", version.Major(), version.Minor()), nil
}
