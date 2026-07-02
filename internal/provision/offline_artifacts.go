// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package provision

import "github.com/Azure/unbounded/pkg/agent/goalstates"

type (
	OfflineTemplateData      = goalstates.OfflineTemplateData
	resolvedOfflineArtifacts = goalstates.ResolvedOfflineArtifacts
)

func ResolveOfflineArtifacts(cfg *AgentConfig, offline *AgentOfflineArtifacts) (*resolvedOfflineArtifacts, error) {
	return goalstates.ResolveOfflineArtifacts(cfg, offline)
}
