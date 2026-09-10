// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package bootstrap drives the agent's first-boot installation as a sequence of
// recorded stages, so that an interrupted attempt can be resumed rather than
// restarted or refused.
//
// The orchestration lives here, separate from the CLI wiring and from the task
// implementations, because the interesting behavior is which stage runs after
// a failure. That question is worth testing directly, with a real state store
// and stages that fail on demand, rather than only through a VM.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/Azure/unbounded/pkg/agent/installstate"
)

// Stages are the units of work the coordinator drives.
//
// Each one may be called again after a failure, and after a success whose
// checkpoint write did not land. Implementations therefore have to be safe to
// repeat; where that is not naturally true, the implementation reconciles
// rather than recreates.
type Stages interface {
	// EnsureHostClean runs the checks that must pass before a fresh
	// installation may mutate anything. It is not called when resuming, where
	// the artifacts it would find are the installation's own.
	EnsureHostClean(ctx context.Context) error

	// ResolveInputs obtains anything the later stages need that is not
	// persisted between runs, such as credentials fetched by attestation.
	//
	// Called on every run, including a resume, and before any stage. It is
	// deliberately outside the checkpoint sequence: these values live in
	// memory, so a resumed process has to obtain them again no matter how far
	// the previous attempt got. Putting attestation inside a checkpointed stage
	// meant a resume past that stage ran without a bootstrap token.
	ResolveInputs(ctx context.Context) error

	// PrepareHost performs host level preparation: packages, OS settings and
	// the firewall baseline. Only ever called before a node exists.
	PrepareHost(ctx context.Context) error

	// PrepareRootFS builds the machine rootfs and the binaries in it.
	// rebuildOwned says whether leftovers from this installation's own earlier
	// attempt may be discarded.
	PrepareRootFS(ctx context.Context, rebuildOwned bool) error

	// EnsureNodeStarted starts the machine and waits for the kubelet. It must
	// reconcile an already-running node rather than recreating it.
	EnsureNodeStarted(ctx context.Context) error

	// EnsureDaemonInstalled persists the applied config and installs, enables
	// and starts the daemon.
	EnsureDaemonInstalled(ctx context.Context) error

	// RepairDaemon restores daemon assets without modifying node configuration.
	RepairDaemon(ctx context.Context) error

	// VerifyInstalled reports whether the daemon is actually installed and
	// running. Used to decide whether a record that claims completion, or one
	// interrupted just before it, is telling the truth.
	VerifyInstalled(ctx context.Context) error
}

// Identity is what a bootstrap attempt is for.
type Identity struct {
	MachineName string
	// HostPrefix is the resolved installation prefix.
	HostPrefix string
	// ConfigFingerprint covers the whole bootstrap intent, so that a retry
	// carrying different inputs is recognized as a different intent.
	ConfigFingerprint string
}

// Reporter receives progress, so the caller can surface bootstrap status
// without the coordinator depending on how that is done.
type Reporter interface {
	StageStarted(ctx context.Context, checkpoint installstate.Checkpoint)
	StageFailed(ctx context.Context, checkpoint installstate.Checkpoint, err error)
}

// Coordinator runs an installation to completion, resuming one that was
// interrupted.
type Coordinator struct {
	log      *slog.Logger
	store    *installstate.Store
	stages   Stages
	reporter Reporter
}

// New returns a Coordinator. reporter may be nil.
func New(log *slog.Logger, store *installstate.Store, stages Stages, reporter Reporter) *Coordinator {
	return &Coordinator{log: log, store: store, stages: stages, reporter: reporter}
}

// Outcome describes what Run did.
type Outcome struct {
	// Installed is true when this call carried the installation to completion.
	Installed bool
	// AlreadyComplete is true when the host was already bootstrapped and
	// nothing needed doing.
	AlreadyComplete bool
	// Resumed is true when an earlier, unfinished attempt was continued.
	Resumed bool
}

// ErrLockHeld is returned when another bootstrap or reset holds the host lock.
var ErrLockHeld = installstate.ErrLockHeld

// Run brings the host to a completely installed state.
//
// The host lock is held for the whole call: bootstrap and reset mutate the same
// files and the same record, and the bootstrap unit retries on a timer, so two
// overlapping is a real possibility.
func (c *Coordinator) Run(ctx context.Context, id Identity) (Outcome, error) {
	lock, err := installstate.AcquireLock()
	if err != nil {
		return Outcome{}, err
	}

	defer func() {
		if err := lock.Release(); err != nil {
			c.log.Warn("releasing the installation lock", "error", err)
		}
	}()

	rec, decision, err := c.begin(ctx, id)
	if err != nil {
		return Outcome{}, err
	}

	if decision.Disposition == installstate.DispositionAlreadyComplete {
		// Trust the record only as far as the host agrees with it. A record can
		// outlive the thing it describes, and skipping bootstrap on a host
		// whose daemon is gone is the failure this whole mechanism exists to
		// prevent.
		if err := c.confirmComplete(ctx, rec); err == nil {
			return Outcome{AlreadyComplete: true}, nil
		} else {
			c.log.Warn("host is recorded as bootstrapped but does not look installed, finishing it",
				"error", err)

			rec, err = c.store.Advance(rec, installstate.CheckpointRepairingDaemon)
			if err != nil {
				return Outcome{}, err
			}
		}
	}

	resumed := decision.Disposition == installstate.DispositionResume

	// Before any stage, and on every run: these inputs are held in memory, so a
	// resumed process has to obtain them again regardless of how far the
	// previous attempt got.
	if rec.Checkpoint != installstate.CheckpointRepairingDaemon {
		if err := c.stages.ResolveInputs(ctx); err != nil {
			return Outcome{}, fmt.Errorf("resolving bootstrap inputs: %w", err)
		}
	}

	if err := c.drive(ctx, rec, resumed); err != nil {
		return Outcome{}, err
	}

	return Outcome{Installed: true, Resumed: resumed}, nil
}

// begin resolves what to do with the host and returns the record to work from.
func (c *Coordinator) begin(ctx context.Context, id Identity) (installstate.Record, installstate.Decision, error) {
	loaded, loadErr := c.store.Load()
	decision := installstate.Decide(loaded, loadErr, id.MachineName, id.ConfigFingerprint)

	switch decision.Disposition {
	case installstate.DispositionRefuse:
		return installstate.Record{}, decision, fmt.Errorf("cannot bootstrap this host: %s", decision.Reason)

	case installstate.DispositionResume, installstate.DispositionAlreadyComplete:
		c.log.Info("continuing an existing installation",
			"installID", decision.Record.InstallID,
			"machine", decision.Record.MachineName,
			"checkpoint", decision.Record.Checkpoint)

		return decision.Record, decision, nil

	case installstate.DispositionFresh:
		// Nothing of ours is recorded, so anything on this host belongs to
		// something else and must not be built over.
		if err := c.stages.EnsureHostClean(ctx); err != nil {
			return installstate.Record{}, decision, err
		}

		installID, err := installstate.NewInstallID()
		if err != nil {
			return installstate.Record{}, decision, err
		}

		rec := installstate.Record{
			InstallID:         installID,
			MachineName:       id.MachineName,
			HostPrefix:        id.HostPrefix,
			ConfigFingerprint: id.ConfigFingerprint,
			Checkpoint:        installstate.CheckpointPreparingHost,
		}

		// Written before the first mutation, so an interruption at any later
		// point is still identifiable as ours.
		if err := c.store.Save(rec); err != nil {
			return installstate.Record{}, decision, err
		}

		return rec, decision, nil

	default:
		return installstate.Record{}, decision, fmt.Errorf("unhandled disposition %d", decision.Disposition)
	}
}

// drive runs stages from the record's checkpoint until the install completes.
func (c *Coordinator) drive(ctx context.Context, rec installstate.Record, resumed bool) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		current := rec.Checkpoint

		if current == installstate.CheckpointComplete {
			return nil
		}

		next, err := c.runStage(ctx, current, resumed)
		if err != nil {
			c.reportFailed(ctx, current, err)

			// The checkpoint is deliberately left where it is, so the next
			// attempt re-enters this stage rather than skipping past it.
			return fmt.Errorf("%s: %w", current, err)
		}

		if next == installstate.CheckpointComplete {
			if _, err := c.store.MarkComplete(rec); err != nil {
				return err
			}

			return nil
		}

		rec, err = c.store.Advance(rec, next)
		if err != nil {
			return err
		}
	}
}

// runStage performs one stage and returns the checkpoint that follows it.
func (c *Coordinator) runStage(
	ctx context.Context,
	current installstate.Checkpoint,
	resumed bool,
) (installstate.Checkpoint, error) {
	c.reportStarted(ctx, current)

	switch current {
	case installstate.CheckpointPreparingHost:
		if err := c.stages.PrepareHost(ctx); err != nil {
			return "", err
		}

		return installstate.CheckpointPreparingRootFS, nil

	case installstate.CheckpointPreparingRootFS:
		// Only a resume may discard existing content, and only at this
		// checkpoint: reaching it means no node has been started, so leftovers
		// are this installation's own unfinished extraction.
		if err := c.stages.PrepareRootFS(ctx, resumed); err != nil {
			return "", err
		}

		return installstate.CheckpointStartingNode, nil

	case installstate.CheckpointStartingNode:
		if err := c.stages.EnsureNodeStarted(ctx); err != nil {
			return "", err
		}

		return installstate.CheckpointInstallingDaemon, nil

	case installstate.CheckpointInstallingDaemon:
		if err := c.stages.EnsureDaemonInstalled(ctx); err != nil {
			return "", err
		}

		return installstate.CheckpointComplete, nil

	case installstate.CheckpointRepairingDaemon:
		if err := c.stages.RepairDaemon(ctx); err != nil {
			return "", err
		}

		if err := c.stages.VerifyInstalled(ctx); err != nil {
			return "", err
		}

		return installstate.CheckpointComplete, nil

	case installstate.CheckpointResetting:
		return "", errors.New("a previous reset did not finish")

	default:
		return "", fmt.Errorf("unknown checkpoint %q", current)
	}
}

// confirmComplete checks that a record claiming completion matches the host.
func (c *Coordinator) confirmComplete(ctx context.Context, rec installstate.Record) error {
	matches, err := c.store.CompletionMatches(rec)
	if err != nil {
		return err
	}

	if !matches {
		return errors.New("the completion marker belongs to a different installation")
	}

	return c.stages.VerifyInstalled(ctx)
}

func (c *Coordinator) reportStarted(ctx context.Context, checkpoint installstate.Checkpoint) {
	c.log.Info("bootstrap stage starting", "checkpoint", checkpoint)

	if c.reporter != nil {
		c.reporter.StageStarted(ctx, checkpoint)
	}
}

func (c *Coordinator) reportFailed(ctx context.Context, checkpoint installstate.Checkpoint, err error) {
	c.log.Error("bootstrap stage failed", "checkpoint", checkpoint, "error", err)

	if c.reporter != nil {
		c.reporter.StageFailed(ctx, checkpoint, err)
	}
}
