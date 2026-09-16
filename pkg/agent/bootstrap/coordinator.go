// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package bootstrap coordinates replay of owned initial installation stages.
package bootstrap

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/unbounded/pkg/agent/installstate"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
)

type Identity struct{ MachineName, ConfigFingerprint string }

type Stages interface {
	EnsureHostClean(context.Context) error
	ResolveInputs(context.Context) error
	PrepareHost(context.Context) error
	PrepareRootFS(context.Context, bool) error
	EnsureNodeStarted(context.Context) error
	EnsureDaemonInstalled(context.Context) error
	RepairDaemon(context.Context) error
	VerifyInstalled(context.Context) error
}

type Reporter interface {
	StageStarted(context.Context, installstate.Checkpoint)
	StageFailed(context.Context, installstate.Checkpoint, error)
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

type Outcome struct{ AlreadyComplete, Resumed bool }

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

	r, loadErr := c.store.Load()

	disposition, err := installstate.Decide(r, loadErr, id.MachineName, id.ConfigFingerprint)
	if err != nil {
		return Outcome{}, err
	}

	resumed := disposition != installstate.Fresh
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
	} else {
		if _, err := c.store.CheckMarker(r); err != nil {
			return Outcome{}, err
		}
	}

	if disposition == installstate.AlreadyComplete {
		if err := c.stages.VerifyInstalled(ctx); err == nil {
			if err := c.store.MarkComplete(r); err != nil {
				return Outcome{}, err
			}

			return Outcome{AlreadyComplete: true}, nil
		}

		r.Checkpoint = installstate.RepairingDaemon
		if err := c.store.Save(r); err != nil {
			return Outcome{}, err
		}
	}

	if r.Checkpoint != installstate.RepairingDaemon {
		if err := c.stages.ResolveInputs(ctx); err != nil {
			return Outcome{}, fmt.Errorf("resolve bootstrap inputs: %w", err)
		}
	}

	for r.Checkpoint != installstate.Complete {
		if err := ctx.Err(); err != nil {
			return Outcome{}, err
		}

		current := r.Checkpoint
		if c.reporter != nil {
			c.reporter.StageStarted(ctx, current)
		}

		next, err := c.runStage(ctx, current, resumed)
		if err != nil {
			if c.reporter != nil {
				c.reporter.StageFailed(ctx, current, err)
			}

			return Outcome{}, fmt.Errorf("%s: %w", current, err)
		}

		r.Checkpoint = next
		if next == installstate.Complete {
			if err := c.store.MarkComplete(r); err != nil {
				return Outcome{}, err
			}
		} else if err := c.store.Save(r); err != nil {
			return Outcome{}, err
		}
	}

	return Outcome{Resumed: resumed}, nil
}

func (c *Coordinator) runStage(ctx context.Context, stage installstate.Checkpoint, resumed bool) (installstate.Checkpoint, error) {
	switch stage {
	case installstate.PreparingHost:
		return installstate.PreparingRootFS, c.stages.PrepareHost(ctx)
	case installstate.PreparingRootFS:
		return installstate.StartingNode, c.stages.PrepareRootFS(ctx, resumed)
	case installstate.StartingNode:
		return installstate.InstallingDaemon, c.stages.EnsureNodeStarted(ctx)
	case installstate.InstallingDaemon:
		if err := c.stages.EnsureDaemonInstalled(ctx); err != nil {
			return "", err
		}

		return installstate.Complete, c.stages.VerifyInstalled(ctx)
	case installstate.RepairingDaemon:
		if err := c.stages.RepairDaemon(ctx); err != nil {
			return "", err
		}

		return installstate.Complete, c.stages.VerifyInstalled(ctx)
	default:
		return "", fmt.Errorf("unsupported checkpoint %s", stage)
	}
}

func SyncFilesystems(paths ...string) error {
	for _, path := range paths {
		if err := utilio.SyncFilesystem(path); err != nil {
			return fmt.Errorf("sync %s: %w", path, err)
		}
	}

	return nil
}
