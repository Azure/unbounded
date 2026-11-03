// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/cmd/agent/internal/hostnode"
	"github.com/Azure/unbounded/cmd/agent/internal/hostnode/hostnodetest"
	"github.com/Azure/unbounded/cmd/agent/internal/noneops"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type recordingPlan struct {
	calls []string
	err   error
}

func (r *recordingPlan) rec(name string) error { r.calls = append(r.calls, name); return r.err }

func (r *recordingPlan) EnsureHostClean(context.Context, *agentStages) error { return r.rec("clean") }
func (r *recordingPlan) ResolveInputs(context.Context, *agentStages) error   { return r.rec("resolve") }
func (r *recordingPlan) PrepareHost(context.Context, *agentStages) error     { return r.rec("host") }
func (r *recordingPlan) PrepareNodeRoot(context.Context, *agentStages) error { return r.rec("root") }
func (r *recordingPlan) EnsureNodeStarted(context.Context, *agentStages) error {
	return r.rec("start")
}

func TestStagesDelegateEveryModeDependentStageToThePlan(t *testing.T) {
	t.Parallel()

	plan := &recordingPlan{}
	s := &agentStages{log: slog.New(slog.DiscardHandler), stagePlan: plan}
	ctx := context.Background()

	require.NoError(t, s.EnsureHostClean(ctx))
	require.NoError(t, s.ResolveInputs(ctx))
	require.NoError(t, s.PrepareHost(ctx))
	require.NoError(t, s.PrepareRootFS(ctx))
	require.NoError(t, s.EnsureNodeStarted(ctx))
	require.Equal(t, []string{"clean", "resolve", "host", "root", "start"}, plan.calls)

	plan.err = errors.New("boom")

	require.ErrorContains(t, s.PrepareHost(ctx), "boom")
}

func TestPlanResolvesFromConfiguredMode(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		cfg       *provision.UnboundedAgentConfig
		wantNSpwn bool
		wantNone  bool
	}{
		{"no config is the default mode", nil, true, false},
		{"empty mode is nspawn", &provision.UnboundedAgentConfig{}, true, false},
		{"explicit nspawn", &provision.UnboundedAgentConfig{AgentConfig: config.AgentConfig{Isolation: "nspawn"}}, true, false},
		{"none", &provision.UnboundedAgentConfig{AgentConfig: config.AgentConfig{Isolation: "none"}}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := &agentStages{cfg: tc.cfg}
			p, err := s.plan()
			require.NoError(t, err)

			_, isNSpawn := p.(nspawnPlan)
			_, isNone := p.(*nonePlan)

			require.Equal(t, tc.wantNSpwn, isNSpawn)
			require.Equal(t, tc.wantNone, isNone)

			again, err := s.plan()
			require.NoError(t, err)
			require.Equal(t, p, again, "one plan per bootstrap run, so a plan can keep state between stages")
			require.Equal(t, s.stagePlan, p)
		})
	}
}

func TestPlanRejectsUnknownAndUnimplementedModes(t *testing.T) {
	t.Parallel()

	s := &agentStages{cfg: &provision.UnboundedAgentConfig{AgentConfig: config.AgentConfig{Isolation: "bogus"}}}
	_, err := s.plan()
	require.ErrorContains(t, err, "bogus")

	// A mode this agent knows but has no bootstrap plan for fails before any
	// stage mutates the host, naming the mode. container gets a plan from its
	// own file; this uses a mode nothing registers so the test does not depend
	// on that.
	_, err = newStagePlan("unplanned")
	require.ErrorContains(t, err, `isolation mode "unplanned"`)

	s = &agentStages{cfg: &provision.UnboundedAgentConfig{AgentConfig: config.AgentConfig{Isolation: "bogus"}}}
	require.Error(t, s.EnsureHostClean(context.Background()))
	require.Error(t, s.ResolveInputs(context.Background()))
	require.Error(t, s.PrepareHost(context.Background()))
	require.Error(t, s.PrepareRootFS(context.Background()))
	require.Error(t, s.EnsureNodeStarted(context.Background()))
}

func TestRegisterStagePlanAddsAModeWithoutEditingTheDispatcher(t *testing.T) {
	const mode isolation.Mode = "seamtest"

	registerStagePlan(mode, func() stagePlan { return &recordingPlan{} })

	t.Cleanup(func() {
		stagePlanMu.Lock()
		delete(stagePlanFactories, mode)
		stagePlanMu.Unlock()
	})

	p, err := newStagePlan(mode)
	require.NoError(t, err)
	require.IsType(t, &recordingPlan{}, p)

	require.PanicsWithValue(t, `cmd: bootstrap plan for isolation mode "seamtest" registered twice`, func() {
		registerStagePlan(mode, func() stagePlan { return nil })
	})
}

func TestStagePlanFactoriesMustBuildAPlan(t *testing.T) {
	const mode isolation.Mode = "nilplantest"

	require.Panics(t, func() { registerStagePlan(mode, nil) })

	registerStagePlan(mode, func() stagePlan { return nil })

	t.Cleanup(func() {
		stagePlanMu.Lock()
		delete(stagePlanFactories, mode)
		stagePlanMu.Unlock()
	})

	p, err := newStagePlan(mode)
	require.ErrorContains(t, err, `isolation mode "nilplantest"`)
	require.Nil(t, p)
}

func TestBuiltInPlansAreRegistered(t *testing.T) {
	t.Parallel()

	for _, mode := range []isolation.Mode{"", isolation.ModeNSpawn, isolation.ModeNone} {
		_, err := newStagePlan(mode)
		require.NoError(t, err, string(mode))
	}
}

// ---- none plan ----

type fakeHost struct {
	mu     sync.Mutex
	calls  []string
	active map[string]bool
}

func (h *fakeHost) run(_ context.Context, _ *slog.Logger, name string, args ...string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.calls = append(h.calls, strings.TrimSpace(name+" "+strings.Join(args, " ")))

	if name == "systemctl" && len(args) > 1 {
		unit := args[len(args)-1]

		switch args[0] {
		case "enable", "restart":
			h.active[unit] = true
		case "is-active":
			if !h.active[unit] {
				return "", errors.New("inactive")
			}
		}
	}

	return "", nil
}

func newNonePlan(t *testing.T, running bool) (*nonePlan, *agentStages, *fakeHost, *[]string) {
	t.Helper()

	h := &fakeHost{active: map[string]bool{}}
	if running {
		h.active[none.UnitContainerd] = true
		h.active[none.UnitKubelet] = true
	}

	root := t.TempDir()
	gs := hostnodetest.MinimalGoalState()

	var persisted []string

	p := &nonePlan{hostSeams: hostSeams{
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
	p.node = noneops.New(noneops.Params{
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		GoalState: gs,
		Overrides: noneops.Overrides{Layout: none.Layout{Base: filepath.Join(root, "node")}, SystemdDir: filepath.Join(root, "units"), Run: h.run, Proc: filepath.Join(root, "noproc"), Download: func(context.Context) error { return nil }, WaitKubelet: func(context.Context) error { return nil }},
		Hooks:     hostnode.Hooks{HostRoot: filepath.Join(root, "host"), SetupNetwork: func(context.Context) error { return nil }},
	})

	s := &agentStages{
		log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:              &provision.UnboundedAgentConfig{},
		gs:               gs,
		credentialsReady: true,
		stagePlan:        p,
	}

	return p, s, h, &persisted
}

func TestNonePlanPrepareNodeRootInstallsAndActivates(t *testing.T) {
	p, s, _, _ := newNonePlan(t, false)

	require.NoError(t, s.PrepareRootFS(t.Context()))

	v, err := p.node.Version()
	require.NoError(t, err)

	b, err := p.node.Backend()
	require.NoError(t, err)

	cur, err := b.Layout.Current()
	require.NoError(t, err)
	require.Equal(t, v, cur, "bootstrap activates the version it installed")
	require.FileExists(t, filepath.Join(b.SystemdDir, none.UnitKubelet))
}

func TestNonePlanPrepareNodeRootLeavesARunningNodeAlone(t *testing.T) {
	p, s, h, _ := newNonePlan(t, true)

	require.NoError(t, s.PrepareRootFS(t.Context()))

	b, err := p.node.Backend()
	require.NoError(t, err)
	require.NoDirExists(t, b.Layout.VersionDir("anything"))
	require.NoFileExists(t, filepath.Join(b.SystemdDir, none.UnitKubelet), "nothing was rewritten")

	for _, c := range h.calls {
		require.NotContains(t, c, "daemon-reload")
	}
}

func TestNonePlanEnsureNodeStartedPersistsAppliedConfigOnlyForANodeItBuilt(t *testing.T) {
	_, s, h, persisted := newNonePlan(t, false)
	require.NoError(t, s.PrepareRootFS(t.Context()))

	require.NoError(t, s.EnsureNodeStarted(t.Context()))
	require.Equal(t, []string{"kube1"}, *persisted, "recorded under kube1 so discovery needs no change")
	require.True(t, h.active[none.UnitKubelet])

	// A retry that finds the node standing did not build it.
	_, s2, _, persisted2 := newNonePlan(t, true)
	require.NoError(t, s2.EnsureNodeStarted(t.Context()))
	require.Empty(t, *persisted2)
}

func TestNonePlanEnsureNodeStartedFailureNamesTheTaskAndRecordsNothing(t *testing.T) {
	_, s, h, persisted := newNonePlan(t, false)
	require.NoError(t, s.PrepareRootFS(t.Context()))

	// Fail the kubelet start; the units before it started.
	p := s.stagePlan.(*nonePlan)
	b, err := p.node.Backend()
	require.NoError(t, err)

	b.Run = func(ctx context.Context, l *slog.Logger, name string, args ...string) (string, error) {
		if name == "systemctl" && len(args) == 3 && args[2] == none.UnitKubelet && args[0] == "enable" {
			return "", errors.New("exec format error")
		}

		return h.run(ctx, l, name, args...)
	}

	err = s.EnsureNodeStarted(t.Context())
	require.ErrorContains(t, err, "start-none-node")
	require.ErrorContains(t, err, "exec format error")
	require.Empty(t, *persisted, "a node that never started is not recorded as built")
	require.Equal(t, "Failed", classifyNodeStartFailure(err))

	// A node that starts but whose kubelet never bootstraps is the common real
	// failure and keeps its dedicated reason.
	require.Equal(t, "KubeletBootstrapFailed", classifyNodeStartFailure(
		errors.New("start-none-node: wait-for-kubelet-bootstrap: kubelet did not bootstrap")))
}

func TestNonePlanEnsureHostCleanRefusesExistingNodeArtifacts(t *testing.T) {
	// The real check inspects the real host; on a clean test host it passes,
	// and the check itself is covered with fakes in pkg/agent/phases/host.
	s := &agentStages{log: slog.New(slog.DiscardHandler), cfg: &provision.UnboundedAgentConfig{AgentConfig: config.AgentConfig{Isolation: "none"}}}
	if err := s.EnsureHostClean(t.Context()); err != nil {
		require.ErrorContains(t, err, "existing node deployment detected")
	}
}
