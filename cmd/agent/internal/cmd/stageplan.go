// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"fmt"
	"sync"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// stagePlan is the part of bootstrap that depends on the isolation mode: how
// the host is prepared, how the node root is materialized, and how the node is
// started. Everything else, including credentials, the reporter, the daemon
// install and verification, is mode independent and stays on agentStages.
//
// The coordinator (cmd/agent/internal/bootstrap) drives agentStages, whose
// mode-dependent stages delegate to the plan for the configured mode.
//
// Adding a mode. Implement stagePlan in its own file in this package, for
// example stageplan_container.go, and register it from that file with a single
// line in an init function:
//
//	func init() { registerStagePlan(isolation.ModeContainer, func() stagePlan { return &containerPlan{} }) }
//
// Nothing else in this package changes: not this file, not bootstrap.go, not
// start.go. A factory is called once per bootstrap run, so a plan may keep
// state between its stages (the none plan keeps its Node). A mode with no plan
// fails bootstrap before anything is mutated, with a message naming the mode.
// The matching isolation backend registers in pkg/agent/isolation, and the
// daemon's node operator through registerNodeOperator; see nodeoperator_dispatch.go.
type stagePlan interface {
	// EnsureHostClean fails when the host already holds a deployment of this
	// mode, before any bootstrap task mutates the host.
	EnsureHostClean(ctx context.Context, s *agentStages) error
	// ResolveInputs resolves the goal state into s.gs and the offline archives
	// into s.archives.
	ResolveInputs(ctx context.Context, s *agentStages) error
	// PrepareHost installs host packages and applies host configuration.
	PrepareHost(ctx context.Context, s *agentStages) error
	// PrepareNodeRoot materializes the node root: the unpacked rootfs for
	// nspawn, the versioned artifacts for none.
	PrepareNodeRoot(ctx context.Context, s *agentStages) error
	// EnsureNodeStarted configures and starts the node and, when this run built
	// it, records the applied config.
	EnsureNodeStarted(ctx context.Context, s *agentStages) error
}

var (
	stagePlanMu        sync.RWMutex
	stagePlanFactories = map[isolation.Mode]func() stagePlan{}
)

// registerStagePlan makes factory the bootstrap plan for mode. Registering a
// mode twice panics, since that is a programming error.
func registerStagePlan(mode isolation.Mode, factory func() stagePlan) {
	if factory == nil {
		panic(fmt.Sprintf("cmd: bootstrap plan for isolation mode %q registered with a nil factory", mode))
	}

	stagePlanMu.Lock()
	defer stagePlanMu.Unlock()

	if _, dup := stagePlanFactories[mode]; dup {
		panic(fmt.Sprintf("cmd: bootstrap plan for isolation mode %q registered twice", mode))
	}

	stagePlanFactories[mode] = factory
}

// newStagePlan builds the plan for mode. The empty mode is the default mode.
func newStagePlan(mode isolation.Mode) (stagePlan, error) {
	if mode == "" {
		mode = isolation.DefaultMode
	}

	stagePlanMu.RLock()

	factory, ok := stagePlanFactories[mode]

	stagePlanMu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("bootstrap is not implemented for isolation mode %q", mode)
	}

	plan := factory()
	if plan == nil {
		return nil, fmt.Errorf("bootstrap is not implemented for isolation mode %q: its plan factory built no plan", mode)
	}

	return plan, nil
}

// plan returns the bootstrap plan for the configured isolation mode, building
// it on first use. A nil configuration, as in tests that exercise only the
// mode independent stages, is the default mode.
func (s *agentStages) plan() (stagePlan, error) {
	if s.stagePlan != nil {
		return s.stagePlan, nil
	}

	mode := isolation.DefaultMode

	if s.cfg != nil {
		var err error

		mode, err = s.cfg.IsolationMode()
		if err != nil {
			return nil, err
		}
	}

	p, err := newStagePlan(mode)
	if err != nil {
		return nil, err
	}

	s.stagePlan = p

	return p, nil
}

func (s *agentStages) EnsureHostClean(ctx context.Context) error {
	p, err := s.plan()
	if err != nil {
		return err
	}

	return p.EnsureHostClean(ctx, s)
}

func (s *agentStages) ResolveInputs(ctx context.Context) error {
	p, err := s.plan()
	if err != nil {
		return err
	}

	return p.ResolveInputs(ctx, s)
}

func (s *agentStages) PrepareHost(ctx context.Context) error {
	p, err := s.plan()
	if err != nil {
		return err
	}

	return p.PrepareHost(ctx, s)
}

func (s *agentStages) PrepareRootFS(ctx context.Context) error {
	p, err := s.plan()
	if err != nil {
		return err
	}

	return p.PrepareNodeRoot(ctx, s)
}

func (s *agentStages) EnsureNodeStarted(ctx context.Context) error {
	p, err := s.plan()
	if err != nil {
		return err
	}

	return p.EnsureNodeStarted(ctx, s)
}
