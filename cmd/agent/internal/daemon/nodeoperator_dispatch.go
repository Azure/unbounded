// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
)

// nodeOperatorFactories holds one node operator per isolation mode. A mode's
// operator is added by calling registerNodeOperator from an init in the file
// that implements it, so adding a mode never edits the dispatcher or the code
// that uses it.
var (
	nodeOperatorMu        sync.RWMutex
	nodeOperatorFactories = map[isolation.Mode]func() nodeOperator{}
)

func init() {
	registerNodeOperator(isolation.ModeNSpawn, func() nodeOperator { return nspawnNodeOperator{} })
}

// registerNodeOperator makes the operator built by factory the one that
// handles machines applied in mode. Registering a mode twice panics, since that
// is a programming error. Registering a mode here does not make the agent
// accept it: activeMode also requires a registered isolation backend.
func registerNodeOperator(mode isolation.Mode, factory func() nodeOperator) {
	nodeOperatorMu.Lock()
	defer nodeOperatorMu.Unlock()

	if _, dup := nodeOperatorFactories[mode]; dup {
		panic(fmt.Sprintf("daemon: node operator for isolation mode %q registered twice", mode))
	}

	nodeOperatorFactories[mode] = factory
}

// newNodeOperator builds the dispatching operator from every registered mode.
// It is the Daemon default; tests inject their own operator through run
// options instead.
func newNodeOperator() nodeOperator {
	nodeOperatorMu.RLock()
	defer nodeOperatorMu.RUnlock()

	ops := make(map[isolation.Mode]nodeOperator, len(nodeOperatorFactories))
	for mode, factory := range nodeOperatorFactories {
		ops[mode] = factory()
	}

	return newDispatchOperator(ops)
}

// dispatchOperator is a nodeOperator that hands each operation on an active
// machine to the operator registered for the machine's isolation mode.
//
// Operations that are not about one active machine, or that act on host-global
// state that does not depend on the mode (invariant 2 of the isolation design),
// are handled by the default mode's operator: discovery of the applied config,
// reset, stopping the daemon, staging an agent upgrade, reading the boot ID
// and rebooting the host.
type dispatchOperator struct {
	ops map[isolation.Mode]nodeOperator
}

var _ nodeOperator = (*dispatchOperator)(nil)

func newDispatchOperator(ops map[isolation.Mode]nodeOperator) *dispatchOperator {
	return &dispatchOperator{ops: ops}
}

// forMode returns the operator for mode. The empty mode is the default mode, so
// a machine built without a recorded mode is operated as nspawn.
func (d *dispatchOperator) forMode(mode isolation.Mode) (nodeOperator, error) {
	if mode == "" {
		mode = isolation.DefaultMode
	}

	op, ok := d.ops[mode]
	if !ok {
		return nil, fmt.Errorf("no node operator for isolation mode %q", mode)
	}

	return op, nil
}

func (d *dispatchOperator) forActive(active *ActiveMachine) (nodeOperator, error) {
	if active == nil {
		return nil, fmt.Errorf("no active machine")
	}

	return d.forMode(active.Mode)
}

func (d *dispatchOperator) hostOperator() (nodeOperator, error) {
	return d.forMode(isolation.DefaultMode)
}

func (d *dispatchOperator) FindActiveMachine(log *slog.Logger) (*ActiveMachine, error) {
	op, err := d.hostOperator()
	if err != nil {
		return nil, err
	}

	return op.FindActiveMachine(log)
}

func (d *dispatchOperator) EnsureLifecycleMigration(ctx context.Context, log *slog.Logger, active *ActiveMachine) error {
	op, err := d.forActive(active)
	if err != nil {
		return err
	}

	return op.EnsureLifecycleMigration(ctx, log, active)
}

func (d *dispatchOperator) RestartNode(ctx context.Context, log *slog.Logger, active *ActiveMachine) error {
	op, err := d.forActive(active)
	if err != nil {
		return err
	}

	return op.RestartNode(ctx, log, active)
}

func (d *dispatchOperator) ResetAgentResources(ctx context.Context, log *slog.Logger) error {
	op, err := d.hostOperator()
	if err != nil {
		return err
	}

	return op.ResetAgentResources(ctx, log)
}

func (d *dispatchOperator) StopDaemon(ctx context.Context, log *slog.Logger) error {
	op, err := d.hostOperator()
	if err != nil {
		return err
	}

	return op.StopDaemon(ctx, log)
}

func (d *dispatchOperator) BootID() (string, error) {
	op, err := d.hostOperator()
	if err != nil {
		return "", err
	}

	return op.BootID()
}

func (d *dispatchOperator) RebootHost(ctx context.Context, log *slog.Logger) error {
	op, err := d.hostOperator()
	if err != nil {
		return err
	}

	return op.RebootHost(ctx, log)
}

func (d *dispatchOperator) RepaveNode(ctx context.Context, log *slog.Logger, active *ActiveMachine, newCfg *provision.UnboundedAgentConfig) error {
	op, err := d.forActive(active)
	if err != nil {
		return err
	}

	return op.RepaveNode(ctx, log, active, newCfg)
}

func (d *dispatchOperator) MigrateIsolation(ctx context.Context, log *slog.Logger, active *ActiveMachine, desired *provision.UnboundedAgentConfig, strategy migrate.Strategy) error {
	op, err := d.forActive(active)
	if err != nil {
		return err
	}

	return op.MigrateIsolation(ctx, log, active, desired, strategy)
}

func (d *dispatchOperator) StageAgentUpgrade(ctx context.Context, log *slog.Logger, request agentUpgradeRequest) error {
	op, err := d.hostOperator()
	if err != nil {
		return err
	}

	return op.StageAgentUpgrade(ctx, log, request)
}

func (d *dispatchOperator) ReconcileHostRoot(ctx context.Context, log *slog.Logger, active *ActiveMachine) (bool, error) {
	op, err := d.forActive(active)
	if err != nil {
		return false, err
	}

	return op.ReconcileHostRoot(ctx, log, active)
}
