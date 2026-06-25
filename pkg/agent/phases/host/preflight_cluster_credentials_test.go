// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package host

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

func TestCheckClusterCredentialsValid(t *testing.T) {
	results := CheckClusterCredentials(slog.New(slog.DiscardHandler), validUnboundedPreflightConfig()).Check(context.Background())

	assert.Equal(t, preflight.ResultsOK(checkClusterCredentialsName, "cluster credentials", "cluster credentials are valid"), results)
}

func TestCheckClusterCredentialsAllowsAttestation(t *testing.T) {
	cfg := validUnboundedPreflightConfig()
	cfg.Kubelet.Auth.BootstrapToken = ""
	cfg.Attest = &provision.AgentAttestConfig{URL: "http://metalman.example.com:8880"}

	results := CheckClusterCredentials(slog.New(slog.DiscardHandler), cfg).Check(context.Background())

	assert.Equal(t, preflight.SeverityOK, results[0].Severity)
}

func TestCheckClusterCredentialsRequiresAuthWhenNoAttestation(t *testing.T) {
	cfg := validUnboundedPreflightConfig()
	cfg.Kubelet.Auth.BootstrapToken = ""

	results := CheckClusterCredentials(slog.New(slog.DiscardHandler), cfg).Check(context.Background())

	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Equal(t, checkClusterCredentialsName, results[0].Name)
	assert.Equal(t, "cluster credentials", results[0].Target)
	assert.Equal(t, "bootstrap credential is invalid", results[0].Message)
}

func TestCheckClusterCredentialsInvalidCA(t *testing.T) {
	cfg := validUnboundedPreflightConfig()
	cfg.Cluster.CaCertBase64 = "not-base64"

	results := CheckClusterCredentials(slog.New(slog.DiscardHandler), cfg).Check(context.Background())

	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Contains(t, results[0].Message, "cluster CA data is invalid")
}

func validUnboundedPreflightConfig() *provision.UnboundedAgentConfig {
	return &provision.UnboundedAgentConfig{
		AgentConfig: *validPreflightConfig(),
	}
}
