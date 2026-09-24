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
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
)

// defaultLockWait bounds how long Run waits for another lifecycle operation to
// release the installation lock. On a reboot the daemon holds it briefly while
// it migrates the host on startup, and the first-boot unit runs start then.
const (
	defaultLockWait  = 30 * time.Second
	lockPollInterval = 250 * time.Millisecond
)

// Identity is what makes one installation distinguishable from another.
//
// HostPrefix is the resolved installation prefix. It is carried here so the
// record written before the first host mutation knows where this installation
// puts its files, which is the only thing teardown can consult after a
// bootstrap that failed before the node started.
type Identity struct {
	MachineName       string
	ConfigFingerprint string
	HostPrefix        string
}

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
	lockWait time.Duration
	lockPoll time.Duration
}

func New(log *slog.Logger, store *installstate.Store, stages Stages, reporter Reporter) *Coordinator {
	return &Coordinator{
		log:      log,
		store:    store,
		stages:   stages,
		reporter: reporter,
		lockWait: defaultLockWait,
		lockPoll: lockPollInterval,
	}
}

type Outcome struct{ AlreadyComplete bool }

func (c *Coordinator) Run(ctx context.Context, id Identity) (Outcome, error) {
	lock, err := c.acquireLock(ctx)
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

		r, err = installstate.NewRecord(id.MachineName, id.ConfigFingerprint, id.HostPrefix)
		if err != nil {
			return Outcome{}, err
		}

		if err := c.store.Save(r); err != nil {
			return Outcome{}, err
		}
	}

	if disposition == installstate.AlreadyComplete {
		verifyErr := c.stages.VerifyInstalled(ctx)
		if verifyErr != nil {
			if err := c.stages.RepairDaemon(ctx); err != nil {
				return Outcome{}, fmt.Errorf("repair daemon after %w: %w", verifyErr, err)
			}

			if err := c.stages.VerifyInstalled(ctx); err != nil {
				return Outcome{}, err
			}

			// Only a repair can have changed anything, so only a repair needs
			// to be committed. The record already says complete: rewriting it
			// on a healthy host would be a durable write for no change, on
			// every boot of every Ignition-provisioned node, since that unit
			// has no completion condition and runs each time.
			if err := c.store.MarkComplete(r); err != nil {
				return Outcome{}, err
			}
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
		name Stage
		run  func(context.Context) error
	}{
		{StagePrepareHost, c.stages.PrepareHost},
		{StagePrepareRootFS, c.stages.PrepareRootFS},
		{StageStartNode, c.stages.EnsureNodeStarted},
		{StageInstallDaemon, c.stages.EnsureDaemonInstalled},
	} {
		if err := ctx.Err(); err != nil {
			return Outcome{}, err
		}

		if c.reporter != nil {
			c.reporter.StageStarted(ctx, stage.name)
		}

		if err := stage.run(ctx); err != nil {
			if c.reporter != nil {
				c.reporter.StageFailed(ctx, stage.name, err)
			}

			return Outcome{}, fmt.Errorf("%s: %w", stage.name, err)
		}
	}

	if err := c.store.MarkComplete(r); err != nil {
		return Outcome{}, err
	}

	return Outcome{}, nil
}

// acquireLock waits up to lockWait for the installation lock, and returns
// installstate.ErrLockHeld if it is still held after that.
func (c *Coordinator) acquireLock(ctx context.Context) (*installstate.Lock, error) {
	deadline := time.Now().Add(c.lockWait)
	logged := false

	for {
		lock, err := c.store.AcquireLock()
		if !errors.Is(err, installstate.ErrLockHeld) || !time.Now().Before(deadline) {
			return lock, err
		}

		if !logged {
			c.log.Info("waiting for another lifecycle operation to release the installation lock", "timeout", c.lockWait)

			logged = true
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(c.lockPoll):
		}
	}
}
