// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodestart

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation/nspawn"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type startNSpawnMachine struct {
	log       *slog.Logger
	goalState *goalstates.NodeStart

	// runner is the machine control driver. Tests inject a fake.
	runner nspawn.Runner

	// wasRunning records whether the machine was already up before this task
	// touched it. A reapply against a live node has to restart any service
	// whose configuration it changed, because those are read at start; on a
	// machine this task boots, the services read the new files anyway.
	wasRunning bool
}

// StartNSpawnMachine returns a task that starts the systemd-nspawn machine and
// waits until D-Bus is responsive inside the machine so that subsequent phases
// can safely use nspawn.Exec().
//
// The agent reaches this work through StartNode, which builds the task directly
// so it can tell whether the configuration it wrote differed. This entry point
// remains for callers outside the agent that compose phases themselves.
func StartNSpawnMachine(log *slog.Logger, goalState *goalstates.NodeStart) phases.Task {
	return &startNSpawnMachine{
		log:       log,
		goalState: goalState,
		runner:    nspawn.NewRunner(log),
	}
}

func (s *startNSpawnMachine) Name() string { return "start-nspawn-machine" }

func (s *startNSpawnMachine) Do(ctx context.Context) error {
	name := s.goalState.MachineName

	if err := nspawn.Enable(ctx, s.runner, name); err != nil {
		return err
	}

	if err := s.startWithRecovery(ctx, name); err != nil {
		return err
	}

	if err := WaitForMachine(ctx, s.log, name); err != nil {
		return fmt.Errorf("wait for machine %s: %w", name, err)
	}

	return nil
}

// startWithRecovery starts the machine, recovering from a stale registration
// (see nspawn.StartWithRecovery), and records whether it was already running.
func (s *startNSpawnMachine) startWithRecovery(ctx context.Context, name string) error {
	running, err := nspawn.StartWithRecovery(ctx, s.log, s.runner, name)
	if err != nil {
		return err
	}

	s.wasRunning = running

	return nil
}

// WaitForMachine polls the machine until it is responsive to systemd-run
// commands. See nspawn.WaitForMachine.
func WaitForMachine(ctx context.Context, log *slog.Logger, machine string) error {
	return nspawn.WaitForMachine(ctx, log, machine)
}
