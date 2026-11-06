// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/confsnap"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
)

// An in-place change builds the target in the alternate slot beside the
// running source and switches over (design, section 10.3). It is the strategy
// for a pair of modes that both run in a slot; see migrate.Plan. The backends
// come from migrationEnv.NewBackend, which knows the modes that can take part.

// cleanTargetBackend removes whatever an earlier attempt left in the target
// slot before preparing it, so a slot is never reused with a stale node. The
// slot is the alternate one, which holds no running node.
type cleanTargetBackend struct{ isolation.Backend }

func (b cleanTargetBackend) Prepare(ctx context.Context, n isolation.NodeSpec) (isolation.NodeRoot, error) {
	if err := b.Destroy(ctx, isolation.SlotName(n.Slot)); err != nil {
		return nil, fmt.Errorf("remove leftovers of slot %s: %w", n.Slot, err)
	}

	return b.Backend.Prepare(ctx, n)
}

// inPlaceMigration carries one in-place migration, fresh or resumed.
type inPlaceMigration struct {
	log     *slog.Logger
	store   migrate.JournalStore
	journal migrate.Journal
	saved   bool
	spec    migrate.InPlaceSpec
}

// record is the write-ahead hook: the step is on disk before it runs.
func (m *inPlaceMigration) record(_ context.Context, step migrate.Step) error {
	m.journal.Step = step
	if err := m.store.Save(m.journal); err != nil {
		return err
	}

	m.saved = true

	return nil
}

// newInPlaceMigration wires the hooks for moving from sourceCfg's node to a
// node in the journal's target mode. sourceCfg may be nil only when resuming
// past the point where the source is still needed.
func (e *migrationEnv) newInPlaceMigration(
	log *slog.Logger,
	j migrate.Journal,
	sourceCfg, targetCfg *provision.AgentConfig,
	downloads *goalstates.DownloadOverrides,
	archives *goalstates.ContainerImageArchiveStaging,
) (*inPlaceMigration, error) {
	source, sourceRoot, err := e.newBackend(log, migrationEndpoint{Mode: j.From, Slot: j.SourceSlot, Config: sourceCfg, Source: true})
	if err != nil {
		return nil, fmt.Errorf("source %s: %w", j.From, err)
	}

	target, targetRoot, err := e.newBackend(log, migrationEndpoint{Mode: j.To, Slot: j.TargetSlot, Config: targetCfg, Downloads: downloads})
	if err != nil {
		return nil, fmt.Errorf("target %s: %w", j.To, err)
	}

	m := &inPlaceMigration{log: log, store: e.inPlaceJournal(), journal: j}

	m.spec = migrate.InPlaceSpec{
		Source:     source,
		SourceRoot: sourceRoot,
		Target:     cleanTargetBackend{target},
		TargetSpec: isolation.NodeSpec{Slot: j.TargetSlot, Version: targetCfg.Cluster.Version},
		TargetRoot: targetRoot,

		// The target backend's own host requirements, checked before anything
		// is changed.
		Preflight: func(ctx context.Context, _ isolation.Backend) error {
			return e.preflight(ctx, log, j.To, migrate.InPlace)
		},
		// The target's artifacts for the same configuration a repave would use.
		Download: func(ctx context.Context) error { return e.stageArchives(ctx, log, archives) },
		// The files both stacks write at the same host paths, such as the kubelet
		// identity. A rollback restarts the source, which needs them as they were.
		SnapshotConfig: func(context.Context) error {
			return confsnap.Take(e.snapshotDir(), e.ConfigRoot, e.configPaths(j.From, j.To))
		},
		RestoreConfig: func(context.Context) error {
			err := confsnap.Restore(e.snapshotDir(), e.ConfigRoot)
			if errors.Is(err, confsnap.ErrNoSnapshot) {
				// A migration an earlier agent started saved nothing.
				log.Warn("no saved shared configuration to restore")

				return nil
			}

			return err
		},
		DiscardConfig: func(context.Context) error { return confsnap.Discard(e.snapshotDir()) },
		// As a repave does, after the source is stopped.
		CleanupNetwork: func(ctx context.Context) error { return e.cleanupNetwork(ctx, log) },
		PersistTarget: func(ctx context.Context, _ isolation.SlotRef) error {
			return e.persistConfig(ctx, log, j.To, j.TargetSlot, targetCfg)
		},
		// The source's applied config is only removed after success, so
		// restoring it is removing the target's record and rewriting the
		// source's from what was read.
		RestoreSource: func(ctx context.Context) error {
			if err := e.removeConfig(ctx, log, j.To, j.TargetSlot); err != nil {
				return err
			}

			if sourceCfg == nil {
				return nil
			}

			return e.persistConfig(ctx, log, j.From, j.SourceSlot, sourceCfg)
		},
		WaitReady: func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, DefaultMigrationReadyTimeout)
			defer cancel()

			return e.waitReady(ctx, log, j.To, j.TargetSlot)
		},
		RemoveSourceConfig: func(ctx context.Context) error { return e.removeConfig(ctx, log, j.From, j.SourceSlot) },
		// The bootstrap of the target mode installs a flush unit and apt
		// guards of its own; the source mode's would otherwise stay.
		PrepareHost: func(ctx context.Context) error { return e.prepareHost(ctx, log, j.To) },
		Record:             m.record,
	}

	return m, nil
}

// conclude turns the outcome of a run or a resume into the journal's terminal
// state.
func (m *inPlaceMigration) conclude(res migrate.Result, runErr error) error {
	switch {
	case runErr == nil, res.Target != nil:
		// The node runs in the target mode. A cleanup error leaves the journal
		// at its last step, which is not terminal: it is kept, and the next
		// start finishes the cleanup.
		if runErr != nil {
			m.log.Warn("migration succeeded but cleaning up the source did not; it is retried at the next daemon start", "error", runErr)
		}

		return nil
	case errors.Is(runErr, migrate.ErrRollbackFailed):
		m.finishJournal(migrate.StepFailed, runErr)

		return fmt.Errorf("%w; the node needs attention", runErr)
	default:
		m.finishJournal(migrate.StepRolledBack, runErr)

		if res.RolledBack {
			return fmt.Errorf("%w (rolled back to %s)", runErr, m.journal.From)
		}

		return fmt.Errorf("%w (the %s node was not changed)", runErr, m.journal.From)
	}
}

// finishJournal records a failure for whoever reports the change. With no
// journal on disk, nothing was changed and there is nothing to record.
func (m *inPlaceMigration) finishJournal(step migrate.Step, cause error) {
	if !m.saved {
		return
	}

	m.journal.Step = step
	m.journal.Message = cause.Error()

	if err := m.store.Save(m.journal); err != nil {
		m.log.Error("could not record the migration outcome", "error", err)
	}
}

// migrateInPlace moves the node between two slot modes by building the target
// in the alternate slot beside the running source. See migrate.RunInPlace for
// the steps and the rollback.
func (e *migrationEnv) migrateInPlace(ctx context.Context, log *slog.Logger, active *ActiveMachine, desired *provision.UnboundedAgentConfig) error {
	sourceSlot := active.Name
	targetSlot := goalstates.AlternateMachine(sourceSlot)

	target, err := desired.IsolationMode()
	if err != nil {
		return err
	}

	// The alternate slot is dead only when nothing records it as in use.
	// An unreadable record is not a node: persisting over it fails later and
	// rolls back.
	if _, err := e.readConfig(log, target, targetSlot); err == nil {
		return fmt.Errorf("slot %s is still recorded as in use; refusing to build the new node over it", targetSlot)
	}

	downloads, archives, err := e.resolveDownloads(ctx, desired)
	if err != nil {
		return fmt.Errorf("resolve download overrides: %w", err)
	}

	// The desired configuration, in the new mode. The Node name, labels and
	// taints come with it, along with any new Kubernetes version the same
	// repave applies, and the kubelet credentials are fresh: the bootstrap
	// token is the one a repave would use and the client certificate is
	// requested by the new kubelet.
	targetCfg := desired.DeepCopy()

	if err := targetCfg.Validate(); err != nil {
		return fmt.Errorf("configuration for isolation mode %s is invalid: %w", target, err)
	}

	j := migrate.Journal{
		Strategy: migrate.InPlace,
		From:     activeModeOf(active), To: target, SourceSlot: sourceSlot, TargetSlot: targetSlot,
	}

	m, err := e.newInPlaceMigration(log, j, active.Config, targetCfg, downloads, archives)
	if err != nil {
		return err
	}

	log.Info("starting in-place isolation change", "from", j.From, "to", target, "source_slot", sourceSlot, "target_slot", targetSlot)

	res, runErr := migrate.RunInPlace(ctx, m.spec)

	return m.conclude(res, runErr)
}

// resumeInPlace decides, from the journal, what to do with a migration that was
// interrupted: see migrate.ResumeInPlace. When it cannot even read what it
// needs to decide, it returns the error and leaves the journal as it is: the
// change is not over, and the caller must keep the journal.
func (e *migrationEnv) resumeInPlace(ctx context.Context, log *slog.Logger, j migrate.Journal) error {
	sourceCfg, err := e.readConfig(log, j.From, j.SourceSlot)

	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist) && (j.Step == migrate.StepReady):
		// Source cleanup already removed its record; only the destroy is left.
		sourceCfg = nil
	default:
		return fmt.Errorf("read source applied config: %w", err)
	}

	var targetCfg *provision.AgentConfig

	if recorded, err := e.readConfig(log, j.To, j.TargetSlot); err == nil {
		targetCfg = recorded.Config
	} else if sourceCfg != nil {
		targetCfg = sourceCfg.Config.DeepCopy()
		targetCfg.Isolation = string(j.To)
	} else {
		return fmt.Errorf("read target applied config: %w", err)
	}

	var src *provision.AgentConfig
	if sourceCfg != nil {
		src = sourceCfg.Config
	}

	m, err := e.newInPlaceMigration(log, j, src, targetCfg, nil, nil)
	if err != nil {
		return err
	}

	m.saved = true

	log.Warn("resuming an interrupted isolation change", "to", j.To, "step", j.Step)

	res, runErr := migrate.ResumeInPlace(ctx, m.spec, j.Step)

	return m.conclude(res, runErr)
}
