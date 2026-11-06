// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

func containerTestBackend(t *testing.T) *container.Backend {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cgroup.controllers"), []byte("cpu\n"), 0o644))

	return &container.Backend{
		RuntimeProvisioned: true,
		RuntimePath:        filepath.Join(dir, "runc"),
		WritableCheck:      func(...string) error { return nil },
		Run: func(_ context.Context, _ *slog.Logger, name string, args ...string) (string, error) {
			if name == "systemctl" && len(args) == 1 && args[0] == "--version" {
				return "systemd 255 (255.4-1ubuntu8)\n", nil
			}

			return "", fmt.Errorf("unexpected command %s %v", name, args)
		},
		Host:               &container.HostFS{CgroupControllers: filepath.Join(dir, "cgroup.controllers"), SELinuxEnforce: filepath.Join(dir, "absent")},
	}
}

func TestPreflightForContainerUsesTheContainerBackendCheck(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)
	got := checkNames(Preflight(log, config.AgentConfig{Isolation: "container"}, nil))

	assert.Contains(t, got, checkContainerBackendName)
	assert.NotContains(t, got, checkNSpawnRuntimeName, "systemd-container is not required")
	assert.NotContains(t, got, checkNoneBackendName)
	assert.Contains(t, got, CheckExistingDeploymentName)
	assert.Equal(t, checkContainerBackendName, CheckContainerBackend(log).Name())
}

func TestCheckContainerBackendPassesWithoutTheRuntimeBecauseTheAgentInstallsIt(t *testing.T) {
	t.Parallel()

	results := checkContainerBackend(slog.New(slog.DiscardHandler), containerTestBackend(t)).Check(context.Background())

	require.Len(t, results, 1)
	assert.Equal(t, preflight.SeverityOK, results[0].Severity)
	assert.Contains(t, results[0].Message, "cgroup v2 is available")
}

func TestCheckContainerBackendFailures(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)

	noCgroup := containerTestBackend(t)
	require.NoError(t, os.Remove(noCgroup.Host.CgroupControllers))

	results := checkContainerBackend(log, noCgroup).Check(context.Background())
	require.Len(t, results, 1)
	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Contains(t, results[0].Message, "cgroup v2")

	readOnly := containerTestBackend(t)
	readOnly.WritableCheck = func(...string) error { return errors.New("/opt is read only") }

	results = checkContainerBackend(log, readOnly).Check(context.Background())
	require.Len(t, results, 1)
	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Contains(t, results[0].Message, "read only")

	notExecutable := containerTestBackend(t)
	require.NoError(t, os.WriteFile(notExecutable.RuntimePath, []byte("x"), 0o644))

	results = checkContainerBackend(log, notExecutable).Check(context.Background())
	require.Len(t, results, 1)
	assert.Contains(t, results[0].Message, "not executable")

	oldSystemd := containerTestBackend(t)
	oldSystemd.Run = func(context.Context, *slog.Logger, string, ...string) (string, error) {
		return "systemd 249 (249.11-0ubuntu3.12)\n", nil
	}

	results = checkContainerBackend(log, oldSystemd).Check(context.Background())
	require.Len(t, results, 1)
	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Contains(t, results[0].Message, "systemd 249 is too old")
}

func TestExistingDeploymentForContainerFindsContainerArtifacts(t *testing.T) {
	t.Parallel()

	for _, path := range []string{container.DefaultBase, "/etc/systemd/system/" + container.UnitName("kube1"), "/etc/systemd/system/" + container.UnitName("kube2")} {
		deps := defaultHostCheckDeps()
		deps.stat = statOnlyExists(path)
		deps.outputCmd = outputWith("", errors.New("not found"))

		results := checkExistingDeployment(slog.New(slog.DiscardHandler), deps, isolation.ModeContainer).Check(context.Background())
		require.Len(t, results, 1, path)
		assert.Equal(t, preflight.SeverityError, results[0].Severity)
		assert.Equal(t, path, results[0].Target)

		// Neither the default mode nor none looks for them.
		for _, mode := range []isolation.Mode{isolation.ModeNSpawn, isolation.ModeNone} {
			results = checkExistingDeployment(slog.New(slog.DiscardHandler), deps, mode).Check(context.Background())
			assert.Equal(t, preflight.SeverityOK, results[0].Severity, "%s %s", mode, path)
		}
	}

	deps := defaultHostCheckDeps()
	deps.stat = statNotExist()
	deps.outputCmd = outputWith("", errors.New("not found"))
	require.NoError(t, ensureNoExistingDeployment(context.Background(), slog.New(slog.DiscardHandler), deps, isolation.ModeContainer))
}
