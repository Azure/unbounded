// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package host

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

func validPreflightConfig() *config.AgentConfig {
	return &config.AgentConfig{
		MachineName: "machine-1",
		NodeName:    "node-1",
		Cluster: config.AgentClusterConfig{
			CaCertBase64: "Y2E=",
			ClusterDNS:   "10.0.0.10",
			Version:      "1.34.0",
		},
		Kubelet: config.AgentKubeletConfig{
			ApiServer: "https://api.example.com:443",
			Auth: config.KubeletAuthInfo{
				BootstrapToken: "abc123.secret456",
			},
		},
	}
}

func TestCheckAgentConfigValid(t *testing.T) {
	results := CheckAgentConfig(slog.New(slog.DiscardHandler), validPreflightConfig()).Check(context.Background())

	assert.Equal(t, preflight.ResultsOK(checkAgentConfigName, "agent config", "agent config is valid"), results)
}

func TestCheckAgentConfigInvalid(t *testing.T) {
	cfg := validPreflightConfig()
	cfg.MachineName = ""

	results := CheckAgentConfig(slog.New(slog.DiscardHandler), cfg).Check(context.Background())

	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Equal(t, checkAgentConfigName, results[0].Name)
	assert.Equal(t, "agent config", results[0].Target)
	assert.Equal(t, "agent config is invalid", results[0].Message)
}
