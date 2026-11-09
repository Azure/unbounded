// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// dispatchOperator is the daemon's nodeOperator. Operations about the host go
// to the host operator whatever mode the node runs in. Operations on the
// active node go to the ModeOperator of the mode the node was applied with,
// and a mode with no operator is an error: it never falls back to another
// mode's operator.
type dispatchOperator struct {
	host  hostRootOperations
	modes ModeOperators
}

// hostRootOperations are the host operations plus the host root move, which is
// host level but rewrites the active node's lifecycle files through its mode.
type hostRootOperations interface {
	hostOperations
	// ReconcileHostRoot moves the agent's files to the host root; see
	// nodeOperator. lifecycle is the active node's mode operator, which
	// rewrites the node's lifecycle files once the move is made.
	ReconcileHostRoot(ctx context.Context, log *slog.Logger, active *ActiveMachine, lifecycle lifecycleMigrator) (bool, error)
}

// lifecycleMigrator rewrites the active node's lifecycle files.
type lifecycleMigrator interface {
	EnsureLifecycleMigration(context.Context, *slog.Logger, *ActiveMachine) error
}

var _ nodeOperator = (*dispatchOperator)(nil)

// newDispatchOperator returns the operator that routes host operations to host
// and node operations to the operator of the node's mode in modes.
func newDispatchOperator(host hostRootOperations, modes ModeOperators) *dispatchOperator {
	return &dispatchOperator{host: host, modes: modes}
}

// forMode returns the operator for mode. The empty mode is the default mode, so
// a machine built without a recorded mode is operated as nspawn.
func (d *dispatchOperator) forMode(mode isolation.Mode) (ModeOperator, error) {
	if mode == "" {
		mode = isolation.DefaultMode
	}

	op, ok := d.modes[mode]
	if !ok || op == nil {
		return nil, fmt.Errorf("this agent cannot operate isolation mode %q: it has no node operator for it", mode)
	}

	return op, nil
}

func (d *dispatchOperator) forActive(active *ActiveMachine) (ModeOperator, error) {
	if active == nil {
		return nil, errors.New("no active machine")
	}

	return d.forMode(active.Mode)
}

// FindActiveMachine returns the active machine, and fails for one applied in a
// mode this agent has no operator for, so the daemon stops before it operates
// a node it does not understand.
func (d *dispatchOperator) FindActiveMachine(log *slog.Logger) (*ActiveMachine, error) {
	active, err := d.host.FindActiveMachine(log)
	if err != nil {
		return nil, err
	}

	if _, err := d.forActive(active); err != nil {
		return nil, fmt.Errorf("active machine %s: %w", active.Name, err)
	}

	return active, nil
}

func (d *dispatchOperator) ResetAgentResources(ctx context.Context, log *slog.Logger) error {
	return d.host.ResetAgentResources(ctx, log)
}

func (d *dispatchOperator) StopDaemon(ctx context.Context, log *slog.Logger) error {
	return d.host.StopDaemon(ctx, log)
}

func (d *dispatchOperator) BootID() (string, error) {
	return d.host.BootID()
}

func (d *dispatchOperator) RebootHost(ctx context.Context, log *slog.Logger) error {
	return d.host.RebootHost(ctx, log)
}

func (d *dispatchOperator) StageAgentUpgrade(ctx context.Context, log *slog.Logger, request agentUpgradeRequest) error {
	return d.host.StageAgentUpgrade(ctx, log, request)
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

func (d *dispatchOperator) RepaveNode(ctx context.Context, log *slog.Logger, active *ActiveMachine, newCfg *provision.UnboundedAgentConfig) error {
	op, err := d.forActive(active)
	if err != nil {
		return err
	}

	return op.RepaveNode(ctx, log, active, newCfg)
}

func (d *dispatchOperator) ReconcileHostRoot(ctx context.Context, log *slog.Logger, active *ActiveMachine) (bool, error) {
	op, err := d.forActive(active)
	if err != nil {
		return false, err
	}

	return d.host.ReconcileHostRoot(ctx, log, active, op)
}
