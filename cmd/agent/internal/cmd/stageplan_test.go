// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/provision"
)

type recordingPlan struct {
	calls []string
	err   error
}

func (r *recordingPlan) rec(name string) error { r.calls = append(r.calls, name); return r.err }

func (r *recordingPlan) EnsureHostClean(context.Context, *agentStages) error { return r.rec("clean") }

func (r *recordingPlan) ResolveInputs(context.Context, *agentStages) error { return r.rec("resolve") }

func (r *recordingPlan) PrepareHost(context.Context, *agentStages) error { return r.rec("host") }

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

func TestPlanComesFromTheModeTableAndIsBuiltOnce(t *testing.T) {
	t.Parallel()

	for _, cfg := range []*provision.UnboundedAgentConfig{nil, {}} {
		s := &agentStages{cfg: cfg}

		p, err := s.plan()
		require.NoError(t, err)
		require.IsType(t, nspawnPlan{}, p, "every configuration is the default mode")

		again, err := s.plan()
		require.NoError(t, err)
		require.Equal(t, p, again, "one plan per bootstrap run, so a plan can keep state between stages")
		require.Equal(t, s.stagePlan, p)
	}
}

// The nspawn host preparation must stay exactly what the agent ran before the
// stage plans existed: the same tasks, in the same order and grouping. The
// task tree's name records both.
func TestNSpawnPlanHostStepsAreUnchanged(t *testing.T) {
	t.Parallel()

	got := nspawnHostSteps(&agentStages{log: slog.New(slog.DiscardHandler)}).Name()

	require.Equal(t,
		"serial(install-packages, parallel(configure-os, configure-nftables, serial(disable-docker, configure-docker), "+
			"disable-containerd, disable-kubelet, disable-swap, harden-apt))",
		got)
}
