// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package bootstrap reapplies the stages of an owned initial installation.
//
// Every stage runs on every attempt. Each decides what to do by looking at the
// host rather than at a record of what a previous attempt claimed to have done,
// so a host that changed in between converges instead of being skipped.
package bootstrap

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
)

type Identity struct{ MachineName, ConfigFingerprint string }

type Stages interface {
	EnsureHostClean(context.Context) error
	ResolveInputs(context.Context) error
	PrepareHost(context.Context) error
	PrepareRootFS(context.Context) error
	EnsureNodeStarted(context.Context) error
	EnsureDaemonInstalled(context.Context) error
	RepairDaemon(context.Context) error
	VerifyInstalled(context.Context) error
}

// Stage names the work being reported on. It is a label for status reporting
// and logs, deliberately not persisted: writing down which stage was reached is
// what lets a record disagree with the host.
type Stage string

const (
	StagePrepareHost   Stage = "preparing-host"
	StagePrepareRootFS Stage = "preparing-rootfs"
	StageStartNode     Stage = "starting-node"
	StageInstallDaemon Stage = "installing-daemon"
)

type Reporter interface {
	StageStarted(context.Context, Stage)
	StageFailed(context.Context, Stage, error)
}

type Coordinator struct {
	log      *slog.Logger
	store    *installstate.Store
	stages   Stages
	reporter Reporter
}

func New(log *slog.Logger, store *installstate.Store, stages Stages, reporter Reporter) *Coordinator {
	return &Coordinator{log: log, store: store, stages: stages, reporter: reporter}
}

type Outcome struct{ AlreadyComplete bool }

func (c *Coordinator) Run(ctx context.Context, id Identity) (Outcome, error) {
	lock, err := c.store.AcquireLock()
	if err != nil {
		return Outcome{}, err
	}
	defer func() {
		if err := lock.Release(); err != nil {
			c.log.Error("release installation lock", "error", err)
		}
	}()

	r, disposition, err := installstate.Admit(c.store, id.MachineName, id.ConfigFingerprint)
	if err != nil {
		return Outcome{}, err
	}

	if disposition == installstate.Fresh {
		if err := c.stages.EnsureHostClean(ctx); err != nil {
			return Outcome{}, err
		}

		r, err = installstate.NewRecord(id.MachineName, id.ConfigFingerprint)
		if err != nil {
			return Outcome{}, err
		}

		if err := c.store.Save(r); err != nil {
			return Outcome{}, err
		}
	}

	if disposition == installstate.AlreadyComplete {
		if err := c.stages.VerifyInstalled(ctx); err != nil {
			if err := c.stages.RepairDaemon(ctx); err != nil {
				return Outcome{}, err
			}

			if err := c.stages.VerifyInstalled(ctx); err != nil {
				return Outcome{}, err
			}
		}

		if err := c.store.MarkComplete(r); err != nil {
			return Outcome{}, err
		}

		return Outcome{AlreadyComplete: true}, nil
	}

	if err := c.stages.ResolveInputs(ctx); err != nil {
		return Outcome{}, fmt.Errorf("resolve bootstrap inputs: %w", err)
	}

	// Every stage runs, in order, on every attempt. Each one decides from the
	// host what it still has to do: host preparation leaves a live nftables
	// ruleset alone, the rootfs is left in place when a machine is registered
	// from it, an already running machine is not restarted, and node services
	// are restarted only when their configuration actually changed.
	for _, stage := range []struct {
		name string
		run  func(context.Context) error
	}{
		{string(StagePrepareHost), c.stages.PrepareHost},
		{string(StagePrepareRootFS), c.stages.PrepareRootFS},
		{string(StageStartNode), c.stages.EnsureNodeStarted},
		{string(StageInstallDaemon), c.stages.EnsureDaemonInstalled},
	} {
		if err := ctx.Err(); err != nil {
			return Outcome{}, err
		}

		if c.reporter != nil {
			c.reporter.StageStarted(ctx, Stage(stage.name))
		}

		if err := stage.run(ctx); err != nil {
			if c.reporter != nil {
				c.reporter.StageFailed(ctx, Stage(stage.name), err)
			}

			return Outcome{}, fmt.Errorf("%s: %w", stage.name, err)
		}
	}

	if err := c.store.MarkComplete(r); err != nil {
		return Outcome{}, err
	}

	return Outcome{}, nil
}
