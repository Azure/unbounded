// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodestart

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation/nspawn"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// execFunc runs a command in the node stack and returns its stdout.
type execFunc func(ctx context.Context, log *slog.Logger, args ...string) (string, error)

type waitForKubelet struct {
	log     *slog.Logger
	machine string
	// exec runs commands in the node stack. Nil means the nspawn machine.
	exec execFunc
	// unit is the kubelet unit to poll. Empty means the nspawn machine's.
	unit string
}

type waitForKubeletBootstrap struct {
	log     *slog.Logger
	machine string
	exec    execFunc
}

// execInNode runs a command in the node stack: through exec when set, in the
// nspawn machine otherwise.
func execInNode(ctx context.Context, log *slog.Logger, exec execFunc, machine string, args ...string) (string, error) {
	if exec != nil {
		return exec(ctx, log, args...)
	}

	return nspawn.Exec(ctx, log, machine, args...)
}

func (w *waitForKubelet) run(ctx context.Context, args ...string) (string, error) {
	return execInNode(ctx, w.log, w.exec, w.machine, args...)
}

func (w *waitForKubeletBootstrap) run(ctx context.Context, args ...string) (string, error) {
	return execInNode(ctx, w.log, w.exec, w.machine, args...)
}

// WaitForKubeletOnHost is WaitForKubelet for a host-native node stack: it
// polls unit through the goal state's executor.
func WaitForKubeletOnHost(log *slog.Logger, gs *goalstates.NodeStart, unit string) phases.Task {
	return &waitForKubelet{log: log, machine: gs.MachineName, exec: gs.Exec, unit: unit}
}

// WaitForKubeletBootstrapOnHost is WaitForKubeletBootstrap for a host-native
// node stack.
func WaitForKubeletBootstrapOnHost(log *slog.Logger, gs *goalstates.NodeStart) phases.Task {
	return &waitForKubeletBootstrap{log: log, machine: gs.MachineName, exec: gs.Exec}
}

// WaitForKubelet returns a task that polls the kubelet systemd service inside
// the nspawn machine until it reports as active.
func WaitForKubelet(log *slog.Logger, machineName string) phases.Task {
	return &waitForKubelet{log: log, machine: machineName}
}

// WaitForKubeletBootstrap returns a task that waits for kubelet TLS bootstrap
// to complete by polling for the generated client kubeconfig inside the nspawn
// machine.
func WaitForKubeletBootstrap(log *slog.Logger, machineName string) phases.Task {
	return &waitForKubeletBootstrap{log: log, machine: machineName}
}

func (w *waitForKubelet) Name() string { return "wait-for-kubelet" }

func (w *waitForKubeletBootstrap) Name() string { return "wait-for-kubelet-bootstrap" }

func (w *waitForKubelet) Do(ctx context.Context) error {
	const (
		pollInterval = 2 * time.Second
		timeout      = 60 * time.Second
	)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		unit := w.unit
		if unit == "" {
			unit = goalstates.SystemdUnitKubelet
		}

		out, err := w.run(ctx, "systemctl", "is-active", unit)
		if err == nil && strings.TrimSpace(out) == "active" {
			w.log.Info("kubelet is active", "machine", w.machine)
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("kubelet not active in %s after %s: %w", w.machine, timeout, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

func (w *waitForKubeletBootstrap) Do(ctx context.Context) error {
	const (
		pollInterval = 2 * time.Second
		timeout      = 2 * time.Minute
	)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		if _, err := w.run(ctx, "test", "-s", goalstates.KubeletKubeconfigPath); err == nil {
			w.log.Info("kubelet bootstrap completed", "machine", w.machine, "kubeconfig", goalstates.KubeletKubeconfigPath)
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("kubelet did not bootstrap in %s after %s: %w", w.machine, timeout, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}
