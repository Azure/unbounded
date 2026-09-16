// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package bootstrap coordinates replay of owned initial installation stages.
package bootstrap

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"golang.org/x/sys/unix"

	"github.com/Azure/unbounded/pkg/agent/installstate"
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

	r, loadErr := c.store.Load()

	disposition, err := installstate.Decide(r, loadErr, id.MachineName, id.ConfigFingerprint)
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
	} else {
		if _, err := c.store.CheckMarker(r); err != nil {
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

	for r.Checkpoint != installstate.Complete {
		if err := ctx.Err(); err != nil {
			return Outcome{}, err
		}

		current := r.Checkpoint
		if c.reporter != nil {
			c.reporter.StageStarted(ctx, current)
		}

		next, err := c.runStage(ctx, current)
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

	return Outcome{}, nil
}

func (c *Coordinator) runStage(ctx context.Context, stage installstate.Checkpoint) (installstate.Checkpoint, error) {
	switch stage {
	case installstate.PreparingHost:
		return installstate.PreparingRootFS, c.stages.PrepareHost(ctx)
	case installstate.PreparingRootFS:
		return installstate.StartingNode, c.stages.PrepareRootFS(ctx)
	case installstate.StartingNode:
		return installstate.InstallingDaemon, c.stages.EnsureNodeStarted(ctx)
	case installstate.InstallingDaemon:
		return installstate.Complete, c.stages.EnsureDaemonInstalled(ctx)
	default:
		return "", fmt.Errorf("unsupported checkpoint %s", stage)
	}
}

func SyncFilesystems(paths ...string) error {
	var files []*os.File
	defer func() {
		for _, f := range files {
			_ = f.Close() //nolint:errcheck // Read-only handle; sync errors are returned.
		}
	}() //nolint:errcheck // Read-only handles; sync errors are returned.

	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return err
		}

		files = append(files, f)
	}

	return SyncOpenFilesystems(files, unix.Syncfs)
}

// SyncOpenFilesystems synchronizes each filesystem once per barrier, using
// open handles that remain valid after teardown removes their paths.
func SyncOpenFilesystems(files []*os.File, syncfs func(int) error) error {
	seen := map[uint64]bool{}

	for _, f := range files {
		var stat unix.Stat_t
		if err := unix.Fstat(int(f.Fd()), &stat); err != nil {
			return err
		}

		if seen[uint64(stat.Dev)] {
			continue
		}

		seen[uint64(stat.Dev)] = true

		if err := syncfs(int(f.Fd())); err != nil {
			return fmt.Errorf("sync %s: %w", f.Name(), err)
		}
	}

	return nil
}
