// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

// DownloadOverrides optionally overrides the upstream download sources for
// binaries the agent installs into the nspawn rootfs. Each field is
// optional; nil or zero-value fields fall back to the compiled-in
// defaults.
type DownloadOverrides struct {
	// Kubernetes overrides the source for kubelet/kubectl/kube-proxy.
	Kubernetes *DownloadSource
	// Containerd overrides the source for the containerd release tarball.
	Containerd *DownloadSource
	// Runc overrides the source for the runc binary.
	Runc *DownloadSource
	// CNI overrides the source for the CNI plugins release tarball.
	CNI *DownloadSource
	// Crictl overrides the source for the crictl release tarball.
	Crictl *DownloadSource
}

// DownloadSource configures the override for a single binary download.
// BaseURL replaces the upstream host + path prefix; URL replaces the
// entire URL template. When both are unset the default template is used.
// Version overrides the version that would otherwise be derived from the
// cluster Kubernetes version or compiled-in defaults.
type DownloadSource struct {
	BaseURL string
	URL     string
	Version string
}
