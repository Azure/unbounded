// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import "github.com/Azure/unbounded/pkg/agent/goalstates"

func kubernetesDownloadSource(rootFS *goalstates.RootFS) *goalstates.DownloadSource {
	if rootFS.Downloads == nil {
		return nil
	}

	return rootFS.Downloads.Kubernetes
}

func containerdDownloadSource(rootFS *goalstates.RootFS) *goalstates.DownloadSource {
	if rootFS.Downloads == nil {
		return nil
	}

	return rootFS.Downloads.Containerd
}

func runcDownloadSource(rootFS *goalstates.RootFS) *goalstates.DownloadSource {
	if rootFS.Downloads == nil {
		return nil
	}

	return rootFS.Downloads.Runc
}

func cniDownloadSource(rootFS *goalstates.RootFS) *goalstates.DownloadSource {
	if rootFS.Downloads == nil {
		return nil
	}

	return rootFS.Downloads.CNI
}

func crictlDownloadSource(rootFS *goalstates.RootFS) *goalstates.DownloadSource {
	if rootFS.Downloads == nil {
		return nil
	}

	return rootFS.Downloads.Crictl
}

func downloadSourceVersion(defaultVersion string, source *goalstates.DownloadSource) string {
	if source != nil && source.Version != "" {
		return source.Version
	}

	return defaultVersion
}
