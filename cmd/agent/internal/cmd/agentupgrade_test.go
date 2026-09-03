// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

type preflightOnlyDaemonService struct{}

func (preflightOnlyDaemonService) Preflight(context.Context, string) (agentbinary.ServicePlan, error) {
	return agentbinary.ServicePlan{UpdateRequired: true, Description: "update test service"}, nil
}

func (preflightOnlyDaemonService) Prepare(context.Context, string) error { return nil }
func (preflightOnlyDaemonService) Reload(context.Context) error          { return nil }
func (preflightOnlyDaemonService) Restart(context.Context) error         { return nil }
func (preflightOnlyDaemonService) WaitHealthy(context.Context, string) error {
	return nil
}

func TestHostAgentUpgradePreflight(t *testing.T) {
	dir := t.TempDir()
	paths := goalstates.AgentUpgradePaths{
		BinaryPath:   filepath.Join(dir, "unbounded-agent"),
		BluePath:     filepath.Join(dir, "unbounded-agent-blue"),
		GreenPath:    filepath.Join(dir, "unbounded-agent-green"),
		CurrentPath:  filepath.Join(dir, "unbounded-agent-current"),
		LastGoodPath: filepath.Join(dir, "unbounded-agent-last-good"),
		SignalPath:   filepath.Join(dir, "agent-upgrade-signal"),
	}
	require.NoError(t, os.WriteFile(paths.BinaryPath, []byte("#!/bin/sh\nexit 0\n"), 0o755))

	candidatePath := filepath.Join(dir, "candidate")
	require.NoError(t, os.WriteFile(candidatePath, []byte("#!/bin/sh\nexit 0\n# candidate\n"), 0o755))

	var output bytes.Buffer

	handler := &hostAgentUpgradeHandler{
		cmdCtx:       &CommandContext{LogFormat: "text"},
		preflight:    true,
		writer:       &output,
		executable:   func() (string, error) { return candidatePath, nil },
		resolvedPath: func() (goalstates.AgentUpgradePaths, error) { return paths, nil },
		newService:   func(goalstates.AgentUpgradePaths) agentbinary.DaemonService { return preflightOnlyDaemonService{} },
		geteuid:      func() int { return 1000 },
	}

	require.NoError(t, handler.execute(context.Background()))
	assert.Contains(t, output.String(), "Agent upgrade mode: host-driven")
	assert.Contains(t, output.String(), "Kubernetes MachineOperation: not created")
	assert.Contains(t, output.String(), "Install target: "+paths.GreenPath)
	assert.Contains(t, output.String(), "Preflight: no changes applied")

	for _, path := range []string{paths.BluePath, paths.GreenPath, paths.CurrentPath, paths.LastGoodPath} {
		_, err := os.Lstat(path)
		assert.ErrorIs(t, err, os.ErrNotExist)
	}
}

func TestWriteHostAgentUpgradePlanOmitsUnchangedLastGood(t *testing.T) {
	var output bytes.Buffer

	plan := agentbinary.ActivationPlan{
		CurrentLinkPath:  "/usr/local/bin/unbounded-agent-current",
		LastGoodLinkPath: "/usr/local/bin/unbounded-agent-last-good",
		RollbackPath:     "/usr/local/bin/unbounded-agent-blue",
	}

	require.NoError(t, writeHostAgentUpgradePlan(&output, plan))
	assert.NotContains(t, output.String(), "Last-good link:")
}

func TestRecordAgentUpgradeFailureSignalCommand(t *testing.T) {
	dir := t.TempDir()
	signalPath := filepath.Join(dir, "agent-upgrade-signal")
	t.Setenv(goalstates.EnvDaemonAgentUpgradeSignalPath, signalPath)
	require.NoError(t, os.WriteFile(signalPath, []byte(`{"operationName":"op-1"}`+"\n"), 0o600))

	cmd := newCmdRecordAgentUpgradeFailureSignal()
	cmd.SetArgs([]string{
		"--message", "rolled back to last good",
	})
	require.NoError(t, cmd.Execute())

	data, err := os.ReadFile(signalPath)
	require.NoError(t, err)
	assert.JSONEq(t, `{"operationName":"op-1","failureMessage":"rolled back to last good"}`, string(data))
}
