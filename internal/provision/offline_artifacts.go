// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package provision

import "github.com/Azure/unbounded/pkg/agent/goalstates"

type (
	OfflineTemplateData      = goalstates.OfflineTemplateData
	resolvedOfflineArtifacts = goalstates.ResolvedOfflineArtifacts
)

// ResolveBootstrapDownloads returns rootfs binary download sources for the
// agent config. OfflineArtifacts, when configured, is a complete artifact set
// and takes precedence over Downloads.
func ResolveBootstrapDownloads(cfg *UnboundedAgentConfig) (*goalstates.DownloadOverrides, error) {
	if cfg == nil {
		return nil, nil
	}

	downloads, sandboxImage, err := goalstates.ResolveBootstrapDownloads(&cfg.AgentConfig, ResolveDownloadOverrides(cfg.Downloads))
	if err != nil {
		return nil, err
	}

	if cfg.CRI.Containerd.SandboxImage == "" && sandboxImage != "" {
		cfg.CRI.Containerd.SandboxImage = sandboxImage
	}

	return downloads, nil
}

func ResolveOfflineArtifacts(cfg *AgentConfig, offline *AgentOfflineArtifacts) (*resolvedOfflineArtifacts, error) {
	return goalstates.ResolveOfflineArtifacts(cfg, offline)
}

func DownloadOverridesFromOfflineArtifacts(sourceRoot string, manifest goalstates.OfflineArtifactManifest) *goalstates.DownloadOverrides {
	return goalstates.DownloadOverridesFromOfflineArtifacts(sourceRoot, manifest)
}
