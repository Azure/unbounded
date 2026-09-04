// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodestart

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type waitForKubelet struct {
	log     *slog.Logger
	machine string
}

type waitForKubeletBootstrap struct {
	log     *slog.Logger
	machine string
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
		out, err := executil.MachineRun(ctx, w.log, w.machine,
			"systemctl", "is-active", goalstates.SystemdUnitKubelet,
		)
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
		if _, err := executil.MachineRun(ctx, w.log, w.machine, "test", "-s", goalstates.KubeletKubeconfigPath); err == nil {
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
