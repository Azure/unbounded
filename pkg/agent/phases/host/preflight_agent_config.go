// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package host

import (
	"context"
	"log/slog"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

const checkAgentConfigName = "agent-config"

type agentConfigChecker struct {
	log    *slog.Logger
	config *config.AgentConfig
}

// CheckAgentConfig verifies the shared agent config has been normalized and is
// internally consistent. Product-specific credential requirements are validated
// by separate checks.
func CheckAgentConfig(log *slog.Logger, cfg *config.AgentConfig) preflight.Checker {
	return agentConfigChecker{log: log, config: cfg}
}

// Name returns the stable check name used in reports and ignore rules.
func (c agentConfigChecker) Name() string { return checkAgentConfigName }

// Check validates the shared agent config without mutating it.
func (c agentConfigChecker) Check(context.Context) []preflight.Result {
	if err := c.config.Validate(); err != nil {
		return preflight.ResultsError(checkAgentConfigName, "agent config", "agent config is invalid")
	}

	return preflight.ResultsOK(checkAgentConfigName, "agent config", "agent config is valid")
}
