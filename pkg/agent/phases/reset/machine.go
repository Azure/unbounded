// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package reset

import (
	"context"
	"log/slog"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation/nspawn"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// The machine lifecycle itself (disable, stop, terminate, remove, inventory)
// lives in the nspawn backend; these tasks only place it in the reset order.

type stopMachine struct {
	log         *slog.Logger
	machineName string
}

// StopMachine returns a task that stops the nspawn machine, waits for it to
// fully stop (up to 30 seconds), and force-terminates it if necessary.
func StopMachine(log *slog.Logger, machineName string) phases.Task {
	return &stopMachine{log: log, machineName: machineName}
}

func (t *stopMachine) Name() string { return "stop-machine" }

func (t *stopMachine) Do(ctx context.Context) error {
	return nspawn.Stop(ctx, t.log, t.machineName)
}

type removeMachine struct {
	log         *slog.Logger
	machineName string
}

// RemoveMachine returns a task that removes the machine rootfs and any
// directory the machine tooling leaves behind.
func RemoveMachine(log *slog.Logger, machineName string) phases.Task {
	return &removeMachine{log: log, machineName: machineName}
}

func (t *removeMachine) Name() string { return "remove-machine" }

func (t *removeMachine) Do(ctx context.Context) error {
	return nspawn.Remove(ctx, t.log, t.machineName)
}

// RegisteredMachine reports registration only after successful inventory.
// Bootstrap callers must not treat a missing inspection tool as a clean host.
func RegisteredMachine(ctx context.Context, log *slog.Logger, name string) (bool, error) {
	return nspawn.Registered(ctx, log, name)
}

// FirstRegisteredMachine returns the name of the first node slot that is
// registered, or the empty string if neither is. It inventories once rather
// than per slot, and like RegisteredMachine it reports an uninspectable host as
// an error rather than as a clean one.
//
// This is for questions about the host as a whole, where either slot being
// occupied is what matters: whether it is safe to flush a ruleset shared by
// every machine in the netns, for instance. A caller asking about the node it
// is building or tearing down wants RegisteredMachine with that slot's name,
// because a machine in the other slot answers a question it did not ask.
func FirstRegisteredMachine(ctx context.Context, log *slog.Logger) (string, error) {
	return nspawn.FirstRegistered(ctx, log, goalstates.NSpawnMachineKube1, goalstates.NSpawnMachineKube2)
}
