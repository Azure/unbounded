// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/cmd/agent/internal/containerops"
	"github.com/Azure/unbounded/cmd/agent/internal/hostnode"
	"github.com/Azure/unbounded/cmd/agent/internal/hostnode/hostnodetest"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const containerTestRuntime = "/opt/unbounded/runtime/runc"

// containerHost stands in for systemd and the runtime.
type containerHost struct {
	mu     sync.Mutex
	calls  []string
	active map[string]bool
	fail   map[string]error // substring of the call, consumed on first match
}

func (h *containerHost) run(_ context.Context, _ *slog.Logger, name string, args ...string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	call := strings.TrimSpace(name + " " + strings.Join(args, " "))
	h.calls = append(h.calls, call)

	for sub, err := range h.fail {
		if strings.Contains(call, sub) {
			delete(h.fail, sub)
			return "", err
		}
	}

	switch {
	case name == "systemctl" && len(args) > 1:
		unit := args[len(args)-1]

		switch args[0] {
		case "enable", "restart":
			h.active[unit] = true
		case "is-active":
			if !h.active[unit] {
				return "", errors.New("inactive")
			}
		}
	case name == containerTestRuntime && args[0] == "state":
		if !h.active[container.UnitName("kube1")] {
			return "", errors.New("no container")
		}

		return `{"status":"running"}`, nil
	case name == containerTestRuntime && args[0] == "exec" && strings.Contains(call, "SystemState"):
		return "running", nil
	}

	return "", nil
}

func newContainerPlan(t *testing.T, running bool) (*containerPlan, *agentStages, *containerHost, *[]string, string) {
	t.Helper()

	h := &containerHost{active: map[string]bool{}, fail: map[string]error{}}
	if running {
		h.active[container.UnitName("kube1")] = true
	}

	dir := t.TempDir()
	gs := hostnodetest.MinimalGoalState()

	var persisted []string

	p := &containerPlan{hostSeams: hostSeams{
		sync: func(...string) error { return nil },
		archives: func(*slog.Logger, *goalstates.ContainerImageArchiveStaging) phases.Task {
			return hostnode.Func("archives", func(context.Context) error { return nil })
		},
		persist: func(_ *slog.Logger, slot string, _ *config.AgentConfig) phases.Task {
			return hostnode.Func("persist-applied-config", func(context.Context) error {
				persisted = append(persisted, slot)
				return nil
			})
		},
	}}
	p.node = containerops.New(containerops.Params{
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		GoalState: gs,
		Overrides: containerops.Overrides{Layout: container.Layout{Base: filepath.Join(dir, "containers")}, SystemdDir: filepath.Join(dir, "units"), RuntimePath: containerTestRuntime, HelperPath: "/usr/local/bin/unbounded-agent", Run: h.run, MountInfoPath: filepath.Join(dir, "mountinfo"), Host: &container.HostFS{SELinuxEnforce: filepath.Join(dir, "absent")}, PollInterval: time.Millisecond, Unpack: func(_ context.Context, d string) error { return os.MkdirAll(filepath.Join(d, "etc"), 0o755) }, InstallRuntime: func(context.Context) error {
			h.calls = append(h.calls, "install-runtime")
			return nil
		}, InstallHelper: func(context.Context) error { return nil }, ProvisionRootfs: func(context.Context) error { return nil }},
		Hooks: hostnode.Hooks{HostRoot: filepath.Join(dir, "host"), SetupNetwork: func(context.Context) error { return nil }, WaitLocalDNS: func(context.Context) error { return nil }},
	})

	s := &agentStages{
		log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:              &provision.UnboundedAgentConfig{},
		gs:               gs,
		credentialsReady: true,
		stagePlan:        p,
	}

	return p, s, h, &persisted, dir
}

func TestContainerPlanIsRegisteredAndSelectedByMode(t *testing.T) {
	t.Parallel()

	_, err := newStagePlan(isolation.ModeContainer)
	require.NoError(t, err)

	s := &agentStages{cfg: &provision.UnboundedAgentConfig{AgentConfig: config.AgentConfig{Isolation: "container"}}}
	p, err := s.plan()
	require.NoError(t, err)
	require.IsType(t, &containerPlan{}, p)
}

func TestContainerPlanPrepareNodeRootBuildsTheSlot(t *testing.T) {
	_, s, h, _, dir := newContainerPlan(t, false)

	require.NoError(t, s.PrepareRootFS(t.Context()))

	require.DirExists(t, filepath.Join(dir, "containers", "kube1", "etc"))
	require.FileExists(t, filepath.Join(dir, "containers", "bundles", "kube1", "config.json"))
	require.FileExists(t, filepath.Join(dir, "units", container.UnitName("kube1")))
	require.Contains(t, h.calls, "install-runtime")

	for _, c := range h.calls {
		require.NotContains(t, c, "enable --now", "preparing the root starts nothing")
	}
}

func TestContainerPlanPrepareNodeRootLeavesARunningNodeAlone(t *testing.T) {
	_, s, h, _, dir := newContainerPlan(t, true)

	require.NoError(t, s.PrepareRootFS(t.Context()))
	require.NoDirExists(t, filepath.Join(dir, "containers", "kube1"), "a running node's rootfs is not rebuilt")
	require.NotContains(t, h.calls, "install-runtime")

	for _, c := range h.calls {
		require.NotContains(t, c, "daemon-reload")
	}
}

func TestContainerPlanPrepareNodeRootFailsWhenTheUnpackFails(t *testing.T) {
	p, s, _, _, _ := newContainerPlan(t, false)
	p.node = containerops.New(containerops.Params{
		Log:       slog.New(slog.DiscardHandler),
		GoalState: s.gs,
		Overrides: containerops.Overrides{Layout: container.Layout{Base: filepath.Join(t.TempDir(), "c")}, SystemdDir: t.TempDir(), Run: (&containerHost{active: map[string]bool{}, fail: map[string]error{}}).run, MountInfoPath: filepath.Join(t.TempDir(), "mi"), Unpack: func(context.Context, string) error { return errors.New("registry unreachable") }, InstallRuntime: func(context.Context) error { return nil }, InstallHelper: func(context.Context) error { return nil }},
		Hooks:     hostnode.Hooks{HostRoot: t.TempDir()},
	})

	err := s.PrepareRootFS(t.Context())
	require.ErrorContains(t, err, "prepare-container-node")
	require.ErrorContains(t, err, "registry unreachable")
}

func TestContainerPlanEnsureNodeStartedPersistsAppliedConfigOnlyForANodeItBuilt(t *testing.T) {
	_, s, h, persisted, _ := newContainerPlan(t, false)
	require.NoError(t, s.PrepareRootFS(t.Context()))

	require.NoError(t, s.EnsureNodeStarted(t.Context()))
	require.Equal(t, []string{"kube1"}, *persisted, "recorded under kube1 so discovery needs no change")
	require.True(t, h.active[container.UnitName("kube1")])

	// A retry that finds the container standing did not build it.
	_, s2, _, persisted2, _ := newContainerPlan(t, true)
	require.NoError(t, s2.EnsureNodeStarted(t.Context()))
	require.Empty(t, *persisted2)
}

func TestContainerPlanEnsureNodeStartedFailureNamesTheTaskAndRecordsNothing(t *testing.T) {
	_, s, h, persisted, _ := newContainerPlan(t, false)
	require.NoError(t, s.PrepareRootFS(t.Context()))

	h.fail["enable --now kubelet.service"] = errors.New("exec format error")

	err := s.EnsureNodeStarted(t.Context())
	require.ErrorContains(t, err, "start-container-node")
	require.ErrorContains(t, err, "start-kubelet")
	require.Empty(t, *persisted, "a node that never started is not recorded as built")
	require.Equal(t, "KubeletBootstrapFailed", classifyNodeStartFailure(err))

	_, s, h, persisted, _ = newContainerPlan(t, false)
	require.NoError(t, s.PrepareRootFS(t.Context()))

	h.fail["enable --now "+container.UnitName("kube1")] = errors.New("unit failed")

	err = s.EnsureNodeStarted(t.Context())
	require.ErrorContains(t, err, "start-container")
	require.Empty(t, *persisted)
}

func TestContainerPlanEnsureHostCleanLooksForContainerArtifacts(t *testing.T) {
	// The real check inspects the real host; on a clean test host it passes,
	// and the check itself is covered with fakes in pkg/agent/phases/host.
	s := &agentStages{log: slog.New(slog.DiscardHandler), cfg: &provision.UnboundedAgentConfig{AgentConfig: config.AgentConfig{Isolation: "container"}}}
	if err := s.EnsureHostClean(t.Context()); err != nil {
		require.ErrorContains(t, err, "existing node deployment detected")
	}
}
