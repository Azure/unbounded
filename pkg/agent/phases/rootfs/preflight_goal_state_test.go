// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

func validGoalStateConfig() *config.AgentConfig {
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
		OCIImage: "registry.example.com/unbounded/rootfs:v1",
	}
}

func TestCheckGoalStateOK(t *testing.T) {
	results := CheckGoalState(slog.New(slog.DiscardHandler), validGoalStateConfig(), nil).Check(context.Background())

	assert.Equal(t, []preflight.Result{preflight.OK(CheckGoalStateName, "goal state", "goal state resolved")}, results)
}

func TestCheckGoalStateResolveError(t *testing.T) {
	cfg := validGoalStateConfig()
	cfg.Cluster.CaCertBase64 = "not-base64"

	results := CheckGoalState(slog.New(slog.DiscardHandler), cfg, nil).Check(context.Background())

	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Equal(t, CheckGoalStateName, results[0].Name)
	assert.Equal(t, "goal state could not be resolved", results[0].Message)
}
