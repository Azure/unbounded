// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

// launchEvents records the order of the guard's steps and the first unit
// change the host sees. Status queries (systemctl is-active) only read.
type launchEvents struct {
	mu     sync.Mutex
	events []string
}

func (e *launchEvents) add(event string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.events = append(e.events, event)
}

func (e *launchEvents) list() []string {
	e.mu.Lock()
	defer e.mu.Unlock()

	return slices.Clone(e.events)
}

func (e *launchEvents) unitChanges() []string {
	var changes []string

	for _, ev := range e.list() {
		if strings.HasPrefix(ev, "systemctl ") {
			changes = append(changes, ev)
		}
	}

	return changes
}

// newAllocationNonePlan is newNonePlan for an allocation host, with the guard's
// seams and the host's systemctl calls recorded in one sequence.
func newAllocationNonePlan(t *testing.T, running bool, verifyErr error) (*agentStages, *launchEvents, *[]string) {
	t.Helper()

	p, s, h, persisted := newNonePlan(t, running)
	s.cfg.Kubelet.Configuration = map[string]any{"providerID": v1alpha3.AllocationProviderIDPrefix + "6d3f0d6e"}

	ev := &launchEvents{}

	p.sync = func(paths ...string) error {
		ev.add("sync " + strings.Join(paths, " "))

		return nil
	}
	p.persistLaunch = func(*config.AgentConfig) error {
		ev.add("persist-launch")

		return nil
	}
	p.verifyLaunch = func(context.Context, *slog.Logger, *config.AgentConfig) error {
		ev.add("verify-launch")

		return verifyErr
	}

	b, err := p.node.Backend()
	require.NoError(t, err)

	b.Run = func(ctx context.Context, l *slog.Logger, name string, args ...string) (string, error) {
		if name == "systemctl" && (len(args) == 0 || args[0] != "is-active") {
			ev.add(strings.TrimSpace(name + " " + strings.Join(args, " ")))
		}

		return h.run(ctx, l, name, args...)
	}

	return s, ev, persisted
}

func TestNonePlanEnsureNodeStarted_AllocationLaunch(t *testing.T) {
	t.Run("record, sync and check come before the first unit change", func(t *testing.T) {
		s, ev, _ := newAllocationNonePlan(t, false, nil)

		require.NoError(t, s.EnsureNodeStarted(t.Context()))

		events := ev.list()
		require.GreaterOrEqual(t, len(events), 4)
		require.Equal(t, []string{"persist-launch", "sync " + goalstates.AgentConfigDir, "verify-launch"}, events[:3])
		require.True(t, strings.HasPrefix(events[3], "systemctl "), "the first unit change follows the check: %v", events)
	})

	t.Run("a refusal starts nothing and records nothing", func(t *testing.T) {
		refusal := fmt.Errorf("%w: machine is fenced", daemon.ErrAllocationLaunchRefused)
		s, ev, persisted := newAllocationNonePlan(t, false, refusal)

		err := s.EnsureNodeStarted(t.Context())
		require.ErrorIs(t, err, daemon.ErrAllocationLaunchRefused)
		require.Empty(t, ev.unitChanges(), "no unit is configured or started")
		require.Empty(t, *persisted, "persist-applied-config does not run")
		require.Equal(t, "AllocationLaunchRefused", classifyNodeStartFailure(err))
	})

	t.Run("a running node is still checked", func(t *testing.T) {
		s, ev, _ := newAllocationNonePlan(t, true, nil)

		require.NoError(t, s.EnsureNodeStarted(t.Context()))
		require.Contains(t, ev.list(), "verify-launch")
	})

	// The check allowed, then the fence landed and the launch-check unit
	// refused the kubelet's start.
	t.Run("fence after the check", func(t *testing.T) {
		s, _, persisted := newAllocationNonePlan(t, false, nil)

		p := s.stagePlan.(*nonePlan)
		b, err := p.node.Backend()
		require.NoError(t, err)

		run := b.Run
		b.Run = func(ctx context.Context, l *slog.Logger, name string, args ...string) (string, error) {
			if name == "systemctl" && strings.Join(args, " ") == "enable --now "+none.UnitKubelet {
				return "", errors.New("Job for unbounded-kubelet.service failed because a dependency failed")
			}

			return run(ctx, l, name, args...)
		}

		err = s.EnsureNodeStarted(t.Context())
		require.Error(t, err)
		require.NotErrorIs(t, err, daemon.ErrAllocationLaunchRefused)
		require.Empty(t, *persisted, "persist-applied-config does not run")
		require.NotEqual(t, "AllocationLaunchRefused", classifyNodeStartFailure(err), "the refusal happened in another process")
	})

	t.Run("a non-allocation host takes no new path", func(t *testing.T) {
		p, s, _, persisted := newNonePlan(t, false)
		require.NoError(t, s.PrepareRootFS(t.Context()))

		p.persistLaunch = func(*config.AgentConfig) error {
			t.Error("the launch record is written for a non-allocation host")

			return nil
		}
		p.verifyLaunch = func(context.Context, *slog.Logger, *config.AgentConfig) error {
			t.Error("the launch is checked on a non-allocation host")

			return nil
		}

		require.NoError(t, s.EnsureNodeStarted(t.Context()))
		require.Equal(t, []string{"none-kube1"}, *persisted)
	})
}
