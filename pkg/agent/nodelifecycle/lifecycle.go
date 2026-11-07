// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package nodelifecycle provides the pre-start, post-start and reconcile phases
// that a node supervising unit runs around the node stack. The Controller
// interface is keyed by slot, but the only implementation is the container
// isolation mode's, used by the hidden node-lifecycle command.
//
// It is intentionally separate from pkg/agent/nspawnlifecycle, which is tied to
// nspawn config regeneration and NVIDIA rewiring and must stay unchanged for
// rollback stability. The overlap is small (the three phase names and a systemd
// reload), so it is duplicated rather than shared.
package nodelifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Controller is what the lifecycle needs from an isolation backend, keyed by
// slot so this package does not depend on a concrete backend.
type Controller interface {
	// Regenerate re-renders the backend's host-side configuration for a slot.
	Regenerate(ctx context.Context, slot string) error
	// Running reports whether the node stack for the slot is up, with a
	// human readable detail when it is not.
	Running(ctx context.Context, slot string) (running bool, detail string, err error)
	// Restart restarts the node stack for the slot (never the host).
	Restart(ctx context.Context, slot string) error
}

// The default bounds of PostStart. The supervising unit's start timeout is
// derived from PostStartBudget, so they are one constant, not two numbers that
// have to be kept equal.
const (
	// DefaultReadyTimeout bounds PostStart's wait for the stack to be running.
	DefaultReadyTimeout = 2 * time.Minute
	// DefaultAfterStartTimeout bounds AfterStart on its own, apart from the
	// readiness wait, so it is not limited to what remained of it.
	DefaultAfterStartTimeout = 3 * time.Minute
	// PostStartBudget is the longest PostStart takes with the defaults.
	PostStartBudget = DefaultReadyTimeout + DefaultAfterStartTimeout
)

// Lifecycle implements the three phases.
type Lifecycle struct {
	Log        *slog.Logger
	Controller Controller
	// ReloadSystemd reloads systemd units after configuration is regenerated.
	ReloadSystemd func(ctx context.Context) error
	// AfterStart, when set, runs at the end of PostStart once the stack is
	// running, with the slot. The container mode uses it to wire GPUs, which
	// needs the stack up. Its failure fails PostStart.
	AfterStart func(ctx context.Context, slot string) error
	// ReadyTimeout bounds PostStart's wait for the stack to be running. Zero
	// is DefaultReadyTimeout.
	ReadyTimeout time.Duration
	// AfterStartTimeout bounds AfterStart. Zero is DefaultAfterStartTimeout.
	AfterStartTimeout time.Duration
	// Peer, when set, returns the slot whose node stack must not run at the
	// same time as slot's. PreStart refuses while it runs.
	Peer func(slot string) string
	// PollInterval is the wait between readiness checks.
	PollInterval time.Duration
}

func (l *Lifecycle) validate() error {
	if l.Log == nil {
		return errors.New("logger is required")
	}

	if l.Controller == nil {
		return errors.New("controller is required")
	}

	return nil
}

// PreStart refuses to start a slot while its peer slot's node stack is still
// alive, so a failed stop can never leave two node stacks on one host. It then
// refreshes host-side configuration and reloads systemd so changed units take
// effect.
func (l *Lifecycle) PreStart(ctx context.Context, slot string) error {
	if err := l.validate(); err != nil {
		return err
	}

	if l.Peer != nil {
		peer := l.Peer(slot)

		running, detail, err := l.Controller.Running(ctx, peer)
		if err != nil {
			return fmt.Errorf("refusing to start %s: cannot tell whether %s is still running: %w", slot, peer, err)
		}

		if running {
			return fmt.Errorf("refusing to start %s while %s is still running", slot, peer)
		}

		l.Log.Debug("peer slot is not running", "slot", slot, "peer", peer, "detail", detail)
	}

	if err := l.Controller.Regenerate(ctx, slot); err != nil {
		return fmt.Errorf("regenerate configuration for %s: %w", slot, err)
	}

	if l.ReloadSystemd != nil {
		if err := l.ReloadSystemd(ctx); err != nil {
			return fmt.Errorf("reload systemd after regenerating configuration for %s: %w", slot, err)
		}
	}

	return nil
}

// PostStart waits for the node stack to be running after the supervising unit
// starts, then runs AfterStart. It fails when the stack is not running within
// ReadyTimeout or AfterStart fails.
func (l *Lifecycle) PostStart(ctx context.Context, slot string) error {
	if err := l.validate(); err != nil {
		return err
	}

	timeout := l.ReadyTimeout
	if timeout <= 0 {
		timeout = DefaultReadyTimeout
	}

	interval := l.PollInterval
	if interval <= 0 {
		interval = time.Second
	}

	parent := ctx

	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	var last string

	for {
		running, detail, err := l.Controller.Running(ctx, slot)
		if err == nil && running {
			l.Log.Info("node stack is running", "slot", slot)

			// The hook gets the caller's context and its own bound, not the
			// readiness context, which may be nearly spent.
			return l.afterStart(parent, slot)
		}

		last = detail
		if err != nil {
			last = err.Error()
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("node stack %s not running after %s: %s", slot, timeout, last)
		case <-time.After(interval):
		}
	}
}

func (l *Lifecycle) afterStart(ctx context.Context, slot string) error {
	if l.AfterStart == nil {
		return nil
	}

	timeout := l.AfterStartTimeout
	if timeout <= 0 {
		timeout = DefaultAfterStartTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := l.AfterStart(ctx, slot); err != nil {
		return fmt.Errorf("after start of %s: %w", slot, err)
	}

	return nil
}

// Reconcile restarts the node stack, which runs PreStart and PostStart through
// the supervising unit's hooks.
func (l *Lifecycle) Reconcile(ctx context.Context, slot string) error {
	if err := l.validate(); err != nil {
		return err
	}

	if err := l.Controller.Restart(ctx, slot); err != nil {
		return fmt.Errorf("restart %s: %w", slot, err)
	}

	return nil
}
