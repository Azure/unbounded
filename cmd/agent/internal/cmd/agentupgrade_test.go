// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
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

func TestHostAgentUpgradeTakesInstallationLockBeforeActivation(t *testing.T) {
	dir := t.TempDir()
	store := installstate.NewStore(filepath.Join(dir, "state"), filepath.Join(dir, "lock"))
	lock, err := store.AcquireLock()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, lock.Release()) })

	handler := &hostAgentUpgradeHandler{
		cmdCtx: &CommandContext{LogFormat: "text"}, installation: store,
		executable:   func() (string, error) { return filepath.Join(dir, "candidate"), nil },
		resolvedPath: func() (goalstates.AgentUpgradePaths, error) { return goalstates.AgentUpgradePaths{}, nil },
		newService:   func(goalstates.AgentUpgradePaths) agentbinary.DaemonService { return preflightOnlyDaemonService{} },
		geteuid:      func() int { return 0 },
	}
	require.ErrorIs(t, handler.execute(t.Context()), installstate.ErrLockHeld)
}

// The handler tests are not parallel: execute sets up the process-wide logger.

// TestHostAgentUpgradePreflightLeavesTheHostRootAlone covers a legacy host,
// where the resolved paths are not where the installation is until the
// migration has run. Preflight must not run it, so it plans against where the
// migration will put things instead.
func TestHostAgentUpgradePreflightLeavesTheHostRootAlone(t *testing.T) {
	dir := t.TempDir()
	paths := goalstates.AgentUpgradePaths{
		BinaryPath:   filepath.Join(dir, "unbounded-agent"),
		BluePath:     filepath.Join(dir, "unbounded-agent-blue"),
		GreenPath:    filepath.Join(dir, "unbounded-agent-green"),
		CurrentPath:  filepath.Join(dir, "unbounded-agent-current"),
		LastGoodPath: filepath.Join(dir, "unbounded-agent-last-good"),
	}
	require.NoError(t, os.WriteFile(paths.BinaryPath, []byte("#!/bin/sh\nexit 0\n"), 0o755))

	candidatePath := filepath.Join(dir, "candidate")
	require.NoError(t, os.WriteFile(candidatePath, []byte("#!/bin/sh\nexit 0\n# candidate\n"), 0o755))

	var output bytes.Buffer

	handler := &hostAgentUpgradeHandler{
		cmdCtx:     &CommandContext{LogFormat: "text"},
		preflight:  true,
		writer:     &output,
		executable: func() (string, error) { return candidatePath, nil },
		resolvedPath: func() (goalstates.AgentUpgradePaths, error) {
			return goalstates.AgentUpgradePaths{}, errors.New("preflight must not use the resolved paths")
		},
		plannedPath: func() (goalstates.AgentUpgradePaths, error) { return paths, nil },
		newService:  func(goalstates.AgentUpgradePaths) agentbinary.DaemonService { return preflightOnlyDaemonService{} },
		geteuid:     func() int { return 1000 },
		migrate: func(*slog.Logger) error {
			t.Error("preflight must not migrate the host root")
			return nil
		},
	}

	require.NoError(t, handler.execute(t.Context()))
	assert.Contains(t, output.String(), "Install target: "+paths.GreenPath)
}

// TestHostAgentUpgradeMigratesBeforeResolvingPaths pins the order the rest of
// the upgrade depends on. Paths resolved before the migration name the new
// root on a legacy host, where the running daemon's slots are not.
func TestHostAgentUpgradeMigratesBeforeResolvingPaths(t *testing.T) {
	var calls []string

	handler := &hostAgentUpgradeHandler{
		cmdCtx:     &CommandContext{LogFormat: "text"},
		executable: func() (string, error) { return filepath.Join(t.TempDir(), "candidate"), nil },
		resolvedPath: func() (goalstates.AgentUpgradePaths, error) {
			calls = append(calls, "resolve")
			return goalstates.AgentUpgradePaths{}, errors.New("stop here")
		},
		geteuid: func() int { return 0 },
		migrate: func(*slog.Logger) error {
			calls = append(calls, "migrate")
			return nil
		},
	}

	require.ErrorContains(t, handler.execute(t.Context()), "stop here")
	assert.Equal(t, []string{"migrate", "resolve"}, calls)
}

func TestHostAgentUpgradeStopsOnAFailedMigration(t *testing.T) {
	handler := &hostAgentUpgradeHandler{
		cmdCtx:     &CommandContext{LogFormat: "text"},
		executable: func() (string, error) { return filepath.Join(t.TempDir(), "candidate"), nil },
		resolvedPath: func() (goalstates.AgentUpgradePaths, error) {
			t.Error("paths must not be resolved after a failed migration")
			return goalstates.AgentUpgradePaths{}, nil
		},
		geteuid: func() int { return 0 },
		migrate: func(*slog.Logger) error { return errors.New("installed under both") },
	}

	require.ErrorContains(t, handler.execute(t.Context()), "installed under both")
}

// TestHostAgentUpgradeRequiresRootBeforeMigrating keeps the error an operator
// sees plain. Without root the migration fails too, but on a permission error
// that does not say what is wrong.
func TestHostAgentUpgradeRequiresRootBeforeMigrating(t *testing.T) {
	handler := &hostAgentUpgradeHandler{
		cmdCtx:     &CommandContext{LogFormat: "text"},
		executable: func() (string, error) { return filepath.Join(t.TempDir(), "candidate"), nil },
		geteuid:    func() int { return 1000 },
		migrate: func(*slog.Logger) error {
			t.Error("a non-root upgrade must be refused before it migrates")
			return nil
		},
	}

	require.ErrorContains(t, handler.execute(t.Context()), "requires root privileges")
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
