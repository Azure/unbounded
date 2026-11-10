// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package migrate moves a live node from one isolation mode to another. The
// orchestration is written against isolation.Backend so it is independent of
// any particular mode, and every host or cluster side effect it needs is a
// hook supplied by the caller.
package migrate

import (
	"context"
	"errors"
	"fmt"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// Strategy is how a migration is carried out.
type Strategy string

const (
	// InPlace prepares the target beside the source on the same host and
	// switches over, rolling back to the source on failure.
	InPlace Strategy = "inPlace"
	// Reprovision resets and bootstraps again in the new mode.
	Reprovision Strategy = "reprovision"
)

// Plan returns the strategy to use to move from one mode to another, following
// the supported-pairs table in the design: in place when both modes run in a
// slot (nspawn and container), a reprovision when either is none. A move to the
// mode the node is already in is an error.
func Plan(from, to isolation.Mode) (Strategy, error) {
	for _, m := range []isolation.Mode{from, to} {
		if _, err := isolation.ParseMode(string(m)); err != nil {
			return "", err
		}
	}

	if from == to {
		return "", fmt.Errorf("node is already in isolation mode %q", to)
	}

	// In-place needs two backends that can run side by side in different
	// slots. ModeNone has no slot, so any move involving it is a reprovision.
	if from != isolation.ModeNone && to != isolation.ModeNone {
		return InPlace, nil
	}

	return Reprovision, nil
}

// InPlaceSpec describes an in-place migration. Hooks may be nil only where
// noted; a nil required hook is a programming error reported by Run.
type InPlaceSpec struct {
	Source isolation.Backend
	// SourceRoot names the slot the source runs in.
	SourceRoot isolation.SlotRef
	Target     isolation.Backend
	// TargetSpec names the alternate slot and the node to prepare.
	TargetSpec isolation.NodeSpec

	// Preflight checks the target backend's requirements before anything is
	// mutated.
	Preflight func(ctx context.Context, target isolation.Backend) error
	// Download fetches the artifacts the target needs. Optional.
	Download func(ctx context.Context) error
	// SnapshotConfig saves the host configuration files both stacks write, before
	// the first step that changes the host. Optional; when set, RestoreConfig
	// should be too. A failure ends the migration with nothing changed.
	SnapshotConfig func(ctx context.Context) error
	// RestoreConfig puts the saved files back during a rollback, after the target
	// is gone and before the source starts. It reports nil when there is no
	// snapshot to restore. The snapshot is kept when it fails.
	RestoreConfig func(ctx context.Context) error
	// DiscardConfig removes the snapshot once the migration is over, after a
	// success or a completed rollback.
	DiscardConfig func(ctx context.Context) error
	// CleanupNetwork removes host networking the source left behind, as a
	// repave does.
	CleanupNetwork func(ctx context.Context) error
	// PersistTarget records the new mode, slot and owning backend.
	PersistTarget func(ctx context.Context, target isolation.SlotRef) error
	// RestoreSource puts the source's applied config back after PersistTarget
	// when the migration rolls back. Required whenever PersistTarget is set.
	RestoreSource func(ctx context.Context) error
	// WaitReady blocks until the target node stack is up on the host or a
	// bounded timeout passes. Cordoning, draining and uncordoning the Node
	// belong to the operator, not to the migration.
	WaitReady func(ctx context.Context) error
	// RemoveSourceConfig removes the source's applied config after success.
	RemoveSourceConfig func(ctx context.Context) error
	// PrepareHost applies the host preparation a bootstrap in the target mode
	// would have, once the target is up: the mode's boot-time firewall flush
	// unit and package-manager guards. Optional. Like the source cleanup, a
	// failure does not undo the migration; it leaves the migration short of
	// StepComplete, so a resume runs it again.
	PrepareHost func(ctx context.Context) error

	// Record, when set, is called before each step that changes the host, with
	// the step about to run (and with StepReady and StepComplete after the
	// matching work). It is a write-ahead log: when Record returns an error the
	// step does not run and the migration is rolled back like any failure.
	// Recording is what lets ResumeInPlace decide what to do after a crash.
	Record func(ctx context.Context, step Step) error
	// TargetRoot names the target slot. Required by ResumeInPlace, which has no
	// Prepare result to take it from; RunInPlace uses it only to clean up after
	// a failed Prepare.
	TargetRoot isolation.SlotRef
}

// Step is how far an in-place migration got. Each value is written before the
// step it names starts, so after a crash it says what may have happened, not
// what finished.
type Step string

const (
	// StepPreparing precedes Prepare: a target slot may be partially built.
	StepPreparing Step = "preparing"
	// StepStoppingSource precedes the source stop: the source may be stopped.
	StepStoppingSource Step = "stopping-source"
	// StepStartingTarget precedes the target start: the target may be running.
	StepStartingTarget Step = "starting-target"
	// StepPersisting precedes PersistTarget: the target's applied config may
	// exist.
	StepPersisting Step = "persisting"
	// StepAwaitingReady follows PersistTarget: the target runs and is recorded.
	StepAwaitingReady Step = "awaiting-ready"
	// StepReady follows a successful WaitReady: only source cleanup is left.
	StepReady Step = "ready"
	// StepComplete follows source cleanup.
	StepComplete Step = "complete"
)

// ErrInterrupted is returned, joined with the outcome of the rollback, by
// ResumeInPlace when it undid a migration that a restart interrupted.
var ErrInterrupted = errors.New("isolation migration was interrupted")

func (s InPlaceSpec) record(ctx context.Context, step Step) error {
	if s.Record == nil {
		return nil
	}

	if err := s.Record(ctx, step); err != nil {
		return fmt.Errorf("record migration step %s: %w", step, err)
	}

	return nil
}

// Result reports how a migration ended.
type Result struct {
	// Target is the slot the node now runs in. Nil unless the migration
	// succeeded.
	Target isolation.SlotRef
	// RolledBack is true when a failure was followed by a restart of the source.
	RolledBack bool
}

// ErrRollbackFailed is returned, joined with the original failure, when a
// failed migration could not restore the source. The node needs attention.
var ErrRollbackFailed = errors.New("rollback to the source failed")

// RunInPlace performs the in-place steps from the design. Nothing is mutated
// when preflight or download fails. Once the target exists, any failure
// destroys it and, if the source was stopped, starts the source again. The
// source root is retained until WaitReady succeeds.
func RunInPlace(ctx context.Context, s InPlaceSpec) (Result, error) {
	if s.Source == nil || s.Target == nil || s.SourceRoot == nil || s.Preflight == nil ||
		s.CleanupNetwork == nil || s.PersistTarget == nil ||
		s.RestoreSource == nil || s.WaitReady == nil || s.RemoveSourceConfig == nil {
		return Result{}, errors.New("migrate: incomplete in-place spec")
	}

	// 1 and 2: nothing has been mutated yet, so these fail cleanly.
	if err := s.Preflight(ctx, s.Target); err != nil {
		return Result{}, fmt.Errorf("preflight %s: %w", s.Target.Mode(), err)
	}

	if s.Download != nil {
		if err := s.Download(ctx); err != nil {
			return Result{}, fmt.Errorf("download %s artifacts: %w", s.Target.Mode(), err)
		}
	}

	// Before the first change to the host, so a rollback has something to
	// restore and a crash after the next record finds it.
	if s.SnapshotConfig != nil {
		if err := s.SnapshotConfig(ctx); err != nil {
			// A partial snapshot is not worth keeping, and it may hold keys.
			(&state{spec: s}).discardConfig(ctx)

			return Result{}, fmt.Errorf("save shared configuration: %w", err)
		}
	}

	// 3: prepare beside the source.
	if err := s.record(ctx, StepPreparing); err != nil {
		// Nothing was changed and no journal exists to remove the snapshot later.
		(&state{spec: s}).discardConfig(ctx)

		return Result{}, err
	}

	targetRoot, err := s.Target.Prepare(ctx, s.TargetSpec)
	if err != nil {
		// Prepare may have left a partial slot. The source was not touched.
		st := &state{spec: s, targetRoot: s.TargetRoot}

		var rbErr error

		if s.TargetRoot != nil {
			_, rbErr = st.rollback(ctx)
		} else {
			// Prepare may have written shared files before it failed.
			rbErr = st.restoreConfig(context.WithoutCancel(ctx))
		}

		err = fmt.Errorf("prepare %s: %w", s.Target.Mode(), err)
		if rbErr != nil {
			// The caller must not record this as a clean rollback: shared files
			// or the partial target may remain.
			err = errors.Join(err, fmt.Errorf("%w: %w", ErrRollbackFailed, rbErr))
		}

		return Result{}, err
	}

	st := &state{spec: s, targetRoot: targetRoot}

	if err := st.run(ctx); err != nil {
		rolledBack, rbErr := st.rollback(ctx)
		if rbErr != nil {
			return Result{RolledBack: rolledBack}, errors.Join(err, fmt.Errorf("%w: %w", ErrRollbackFailed, rbErr))
		}

		return Result{RolledBack: rolledBack}, err
	}

	return st.finish(ctx)
}

// ResumeInPlace decides what to do with an in-place migration a restart
// interrupted, from the last step Record saw. Before the source was stopped it
// only removes the partial target. Once the source may be stopped it rolls back
// to the source and returns ErrInterrupted, since repeating a step that crashed
// the process could loop. When the target was already running and recorded it
// waits for the target as the original run would, then finishes or rolls back.
func ResumeInPlace(ctx context.Context, s InPlaceSpec, at Step) (Result, error) {
	if s.Source == nil || s.Target == nil || s.SourceRoot == nil || s.TargetRoot == nil ||
		s.RestoreSource == nil || s.WaitReady == nil || s.RemoveSourceConfig == nil {
		return Result{}, errors.New("migrate: incomplete resume spec")
	}

	st := &state{spec: s, targetRoot: s.TargetRoot}

	switch at {
	case StepComplete:
		return Result{Target: s.TargetRoot}, nil
	case StepReady:
		return st.finish(ctx)
	case StepAwaitingReady:
		st.sourceStopped, st.targetStarted, st.persisted = true, true, true

		if err := s.WaitReady(ctx); err != nil {
			return st.rollbackInterrupted(ctx, fmt.Errorf("wait for the target node stack: %w", err))
		}

		if err := s.record(ctx, StepReady); err != nil {
			return st.rollbackInterrupted(ctx, err)
		}

		return st.finish(ctx)
	case StepPreparing:
		return st.rollbackInterrupted(ctx, nil)
	case StepStoppingSource, StepStartingTarget:
		st.sourceStopped, st.targetStarted = true, at == StepStartingTarget

		return st.rollbackInterrupted(ctx, nil)
	case StepPersisting:
		st.sourceStopped, st.targetStarted, st.persisted = true, true, true

		return st.rollbackInterrupted(ctx, nil)
	default:
		return Result{}, fmt.Errorf("migrate: unknown step %q", at)
	}
}

func (st *state) rollbackInterrupted(ctx context.Context, cause error) (Result, error) {
	// A step that crashed or timed out may have started the target without
	// recording it, so stop it whenever the source may have been stopped.
	st.targetStarted = st.targetStarted || st.sourceStopped

	rolledBack, rbErr := st.rollback(ctx)

	err := ErrInterrupted
	if cause != nil {
		err = fmt.Errorf("%w: %w", ErrInterrupted, cause)
	}

	if rbErr != nil {
		return Result{RolledBack: rolledBack}, errors.Join(err, fmt.Errorf("%w: %w", ErrRollbackFailed, rbErr))
	}

	return Result{RolledBack: rolledBack}, err
}

// finish is the last step: the target is up. Failures from here do not undo the
// migration.
func (st *state) finish(ctx context.Context) (Result, error) {
	s := st.spec
	targetRoot := st.targetRoot

	var cleanup error

	if s.PrepareHost != nil {
		if err := s.PrepareHost(ctx); err != nil {
			cleanup = errors.Join(cleanup, fmt.Errorf("prepare the host for %s: %w", s.Target.Mode(), err))
		}
	}

	if err := s.Source.Destroy(ctx, s.SourceRoot); err != nil {
		cleanup = errors.Join(cleanup, fmt.Errorf("destroy source: %w", err))
	}

	if err := s.RemoveSourceConfig(ctx); err != nil {
		cleanup = errors.Join(cleanup, fmt.Errorf("remove source applied config: %w", err))
	}

	if s.DiscardConfig != nil {
		if err := s.DiscardConfig(ctx); err != nil {
			cleanup = errors.Join(cleanup, fmt.Errorf("discard configuration snapshot: %w", err))
		}
	}

	if cleanup == nil {
		if err := s.record(ctx, StepComplete); err != nil {
			cleanup = err
		}
	}

	return Result{Target: targetRoot}, cleanup
}

type state struct {
	spec          InPlaceSpec
	targetRoot    isolation.SlotRef
	sourceStopped bool
	targetStarted bool
	persisted     bool
}

func (st *state) run(ctx context.Context) error {
	s := st.spec

	// 4. Record, then mark, then stop: a failed stop may still have stopped
	// part of the source, but a failed record has stopped nothing.
	if err := s.record(ctx, StepStoppingSource); err != nil {
		return err
	}

	st.sourceStopped = true

	if err := s.Source.Stop(ctx, s.SourceRoot); err != nil {
		return fmt.Errorf("stop %s: %w", s.Source.Mode(), err)
	}

	// 5.
	if err := s.CleanupNetwork(ctx); err != nil {
		return fmt.Errorf("clean up host networking: %w", err)
	}

	// 6. Fresh kubelet credentials come from the normal bootstrap flow inside
	// Start; none are copied from the source.
	if err := s.record(ctx, StepStartingTarget); err != nil {
		return err
	}

	st.targetStarted = true

	if err := s.Target.Start(ctx, st.targetRoot); err != nil {
		return fmt.Errorf("start %s: %w", s.Target.Mode(), err)
	}

	// 7.
	if err := s.record(ctx, StepPersisting); err != nil {
		return err
	}

	st.persisted = true

	if err := s.PersistTarget(ctx, st.targetRoot); err != nil {
		return fmt.Errorf("persist applied config: %w", err)
	}

	if err := s.record(ctx, StepAwaitingReady); err != nil {
		return err
	}

	// 8.
	if err := s.WaitReady(ctx); err != nil {
		return fmt.Errorf("wait for the target node stack: %w", err)
	}

	return s.record(ctx, StepReady)
}

// rollback undoes whatever run did. It uses a context that outlives
// cancellation of the migration's own, so a timed-out migration still restores
// the source.
//
// The source starts again only once the target is known to be gone: stopped,
// destroyed and no longer reported running. Two node stacks on one host would
// fight over the kubelet's identity and the host's networking, so when any of
// that cannot be confirmed the source stays stopped and the rollback fails,
// which leaves the node for an operator rather than running it twice.
func (st *state) rollback(ctx context.Context) (bool, error) {
	s := st.spec
	ctx = context.WithoutCancel(ctx)

	var errs []error

	targetGone := true

	if st.targetStarted {
		if err := s.Target.Stop(ctx, st.targetRoot); err != nil {
			targetGone = false

			errs = append(errs, fmt.Errorf("stop %s: %w", s.Target.Mode(), err))
		}
	}

	if st.persisted {
		if err := s.RestoreSource(ctx); err != nil {
			errs = append(errs, fmt.Errorf("restore source applied config: %w", err))
		}
	}

	if err := s.Target.Destroy(ctx, st.targetRoot); err != nil {
		targetGone = false

		errs = append(errs, fmt.Errorf("destroy %s: %w", s.Target.Mode(), err))
	}

	if targetGone && st.targetStarted {
		if err := st.confirmTargetStopped(ctx); err != nil {
			targetGone = false

			errs = append(errs, err)
		}
	}

	// The target is gone, so nothing writes the shared files any more.
	if err := st.restoreConfig(ctx); err != nil {
		errs = append(errs, err)
	}

	restarted := false

	if st.sourceStopped {
		switch {
		case !targetGone:
			errs = append(errs, fmt.Errorf("%s was not restarted because %s may still be running", s.Source.Mode(), s.Target.Mode()))
		default:
			if err := s.Source.Start(ctx, s.SourceRoot); err != nil {
				errs = append(errs, fmt.Errorf("start %s: %w", s.Source.Mode(), err))
			} else {
				restarted = true
			}
		}
	}

	return restarted, errors.Join(errs...)
}

// confirmTargetStopped asks the target backend whether its node stack is still
// up after it was stopped and destroyed.
func (st *state) confirmTargetStopped(ctx context.Context) error {
	s := st.spec

	status, err := s.Target.Status(ctx, st.targetRoot)
	if err != nil {
		return fmt.Errorf("confirm %s stopped: %w", s.Target.Mode(), err)
	}

	if status.Running {
		return fmt.Errorf("%s is still running after it was stopped and destroyed", s.Target.Mode())
	}

	return nil
}

// discardConfig drops the configuration snapshot, ignoring a failure: it is
// called where the migration is already ending with a more important error.
func (st *state) discardConfig(ctx context.Context) {
	if st.spec.DiscardConfig == nil {
		return
	}

	_ = st.spec.DiscardConfig(context.WithoutCancel(ctx)) //nolint:errcheck // best effort
}

// restoreConfig puts the shared configuration back and, when that worked,
// drops the snapshot. A failure keeps the snapshot for another attempt.
func (st *state) restoreConfig(ctx context.Context) error {
	s := st.spec
	if s.RestoreConfig == nil {
		return nil
	}

	if err := s.RestoreConfig(ctx); err != nil {
		return fmt.Errorf("restore shared configuration: %w", err)
	}

	if s.DiscardConfig != nil {
		if err := s.DiscardConfig(ctx); err != nil {
			return fmt.Errorf("discard configuration snapshot: %w", err)
		}
	}

	return nil
}
