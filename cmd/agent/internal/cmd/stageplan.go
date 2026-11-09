// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"

	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// stagePlan is the part of bootstrap that depends on the isolation mode: how
// the host is prepared, how the node root is materialized, and how the node is
// started. Everything else, including credentials, the reporter, the daemon
// install and verification, is mode independent and stays on agentStages.
//
// The coordinator (cmd/agent/internal/bootstrap) drives agentStages, whose
// mode-dependent stages delegate to the plan for the configured mode. A mode's
// plan is built by its entry in the mode composition table (modes.go), once
// per bootstrap run, so a plan may keep state between its stages.
type stagePlan interface {
	// EnsureHostClean fails when the host already holds a deployment of this
	// mode, before any bootstrap task mutates the host.
	EnsureHostClean(ctx context.Context, s *agentStages) error
	// ResolveInputs resolves the goal state into s.gs and the offline archives
	// into s.archives.
	ResolveInputs(ctx context.Context, s *agentStages) error
	// PrepareHost installs host packages and applies host configuration.
	PrepareHost(ctx context.Context, s *agentStages) error
	// PrepareNodeRoot materializes the node root, such as the unpacked rootfs
	// of an nspawn machine.
	PrepareNodeRoot(ctx context.Context, s *agentStages) error
	// EnsureNodeStarted configures and starts the node and, when this run built
	// it, records the applied config.
	EnsureNodeStarted(ctx context.Context, s *agentStages) error
}

// configuredMode is the isolation mode a bootstrap configuration selects. A nil
// configuration, as in tests that exercise only the mode independent stages,
// is the default mode. A mode this agent cannot operate is an error, so the
// bootstrap refuses it before it touches the host.
func configuredMode(cfg *provision.UnboundedAgentConfig) (isolation.Mode, error) {
	if cfg == nil {
		return isolation.DefaultMode, nil
	}

	return cfg.IsolationMode()
}

// plan returns the bootstrap plan for the configured isolation mode, building
// it from the mode composition table on first use.
func (s *agentStages) plan() (stagePlan, error) {
	if s.stagePlan != nil {
		return s.stagePlan, nil
	}

	mode, err := configuredMode(s.cfg)
	if err != nil {
		return nil, err
	}

	m, err := agentModes().lookup(mode)
	if err != nil {
		return nil, err
	}

	s.stagePlan = m.newPlan()

	return s.stagePlan, nil
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
