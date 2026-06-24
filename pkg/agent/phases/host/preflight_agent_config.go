// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package host

import (
	"context"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

// CheckAgentConfigName is the stable name for the agent config validation check.
const CheckAgentConfigName = "agent-config"

type agentConfigChecker struct {
	config *config.AgentConfig
}

// CheckAgentConfig returns a checker that validates the shared agent config
// shape. Product-specific credential requirements are validated by separate
// checks.
func CheckAgentConfig(cfg *config.AgentConfig) preflight.Checker {
	return agentConfigChecker{config: cfg}
}

// Name returns the stable check name used in reports and ignore rules.
func (c agentConfigChecker) Name() string { return CheckAgentConfigName }

// Check validates the shared agent config without mutating it.
func (c agentConfigChecker) Check(context.Context) []preflight.Result {
	if err := c.config.Validate(); err != nil {
		return preflight.ResultsError(CheckAgentConfigName, "agent config", "agent config is invalid")
	}

	return preflight.ResultsOK(CheckAgentConfigName, "agent config", "agent config is valid")
}
