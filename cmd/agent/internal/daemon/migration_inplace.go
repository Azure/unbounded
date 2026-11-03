// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/Azure/unbounded/cmd/agent/internal/containerops"
	"github.com/Azure/unbounded/cmd/agent/internal/nspawnops"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/confsnap"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
	"github.com/Azure/unbounded/pkg/agent/isolation/nspawn"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
)

// migrationEndpoint is one end of an in-place migration.
type migrationEndpoint struct {
	Mode      isolation.Mode
	Slot      string
	Config    *provision.AgentConfig
	Downloads *goalstates.DownloadOverrides
	// Source is true for the end that already exists and is only restarted on
	// rollback, false for the end that is built and activated.
	Source bool
}

// containerMigrationBackend adapts a containerops.Node to isolation.Backend for
// a migration. The container backend's own Start only starts the unit; bringing
// the node up as the only running slot (reset the shared kubelet state, write
// configuration, start, wait for kubelet) is Node.Activate, and restarting a
// slot that was stopped is Node.Resume.
type containerMigrationBackend struct {
	node   *containerops.Node
	source bool
}

var _ isolation.Backend = (*containerMigrationBackend)(nil)

func (*containerMigrationBackend) Mode() isolation.Mode { return isolation.ModeContainer }

func (b *containerMigrationBackend) Requirements() isolation.Requirements {
	return b.node.Backend().Requirements()
}

func (b *containerMigrationBackend) Prepare(ctx context.Context, _ isolation.NodeSpec) (isolation.NodeRoot, error) {
	return b.node.Prepare(ctx)
}

func (b *containerMigrationBackend) Start(ctx context.Context, _ isolation.NodeRoot) error {
	if b.source {
		return b.node.Resume(ctx)
	}

	return b.node.Activate(ctx)
}

func (b *containerMigrationBackend) Stop(ctx context.Context, _ isolation.NodeRoot) error {
	return b.node.Stop(ctx)
}

func (b *containerMigrationBackend) Restart(ctx context.Context, _ isolation.NodeRoot) error {
	return b.node.Restart(ctx)
}

func (b *containerMigrationBackend) Destroy(ctx context.Context, _ isolation.NodeRoot) error {
	return b.node.Destroy(ctx)
}

func (b *containerMigrationBackend) Status(ctx context.Context, r isolation.NodeRoot) (isolation.Status, error) {
	return b.node.Backend().Status(ctx, r)
}

func (b *containerMigrationBackend) Exec(ctx context.Context, r isolation.NodeRoot, cmd isolation.ExecSpec) (isolation.ExecResult, error) {
	return b.node.Backend().Exec(ctx, r, cmd)
}

func (b *containerMigrationBackend) Logs(ctx context.Context, r isolation.NodeRoot, opts isolation.LogOptions) (io.ReadCloser, error) {
	return b.node.Backend().Logs(ctx, r, opts)
}

// cleanTargetBackend removes whatever an earlier attempt left in the target
// slot before preparing it, so a slot is never reused with a stale rootfs. The
// slot is the alternate one, which holds no running node.
type cleanTargetBackend struct{ isolation.Backend }

func (b cleanTargetBackend) Prepare(ctx context.Context, n isolation.NodeSpec) (isolation.NodeRoot, error) {
	if err := b.Destroy(ctx, isolation.DirRoot{SlotName: n.Slot}); err != nil {
		return nil, fmt.Errorf("remove leftovers of slot %s: %w", n.Slot, err)
	}

	return b.Backend.Prepare(ctx, n)
}

// Seams for the host-mutating parts, replaced in tests.
var (
	// newMigrationBackend builds the backend and node root of one endpoint.
	newMigrationBackend = func(log *slog.Logger, e migrationEndpoint) (isolation.Backend, isolation.NodeRoot, error) {
		switch e.Mode {
		case isolation.ModeNSpawn:
			return nspawnops.Backend(nspawnops.Params{Log: log, Config: e.Config, Downloads: e.Downloads}), nspawn.Root(e.Slot), nil
		case isolation.ModeContainer:
			node := containerops.New(containerops.Params{Log: log, Config: e.Config, Downloads: e.Downloads, Slot: e.Slot})

			root, err := node.Backend().NodeRoot(e.Slot)
			if err != nil {
				return nil, nil, err
			}

			return &containerMigrationBackend{node: node, source: e.Source}, root, nil
		default:
			return nil, nil, fmt.Errorf("in-place migration is not supported for isolation mode %q", e.Mode)
		}
	}

	cleanupMigrationNetwork = func(ctx context.Context, log *slog.Logger) error {
		return reset.CleanupNetwork(log).Do(ctx)
	}

	// waitKubeletOnHost is the readiness check a repave uses: the kubelet is
	// active and has its credentials. Containers wait for it inside Activate.
	waitKubeletOnHost = func(ctx context.Context, log *slog.Logger, mode isolation.Mode, slot string) error {
		if mode != isolation.ModeNSpawn {
			return nil
		}

		return nodestart.WaitForKubelet(log, slot).Do(ctx)
	}

	readMigrationConfig = readActiveSlot
)

// inPlaceMigration carries one in-place migration, fresh or resumed.
type inPlaceMigration struct {
	log     *slog.Logger
	store   migrate.JournalStore
	journal migrate.Journal
	saved   bool

	targetCfg *provision.AgentConfig
	spec      migrate.InPlaceSpec
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
func newInPlaceMigration(
	ctx context.Context,
	log *slog.Logger,
	store migrate.JournalStore,
	j migrate.Journal,
	sourceCfg, targetCfg *provision.AgentConfig,
	downloads *goalstates.DownloadOverrides,
	archives *goalstates.ContainerImageArchiveStaging,
) (*inPlaceMigration, error) {
	source, sourceRoot, err := newMigrationBackend(log, migrationEndpoint{Mode: j.From, Slot: j.SourceSlot, Config: sourceCfg, Source: true})
	if err != nil {
		return nil, fmt.Errorf("source %s: %w", j.From, err)
	}

	target, targetRoot, err := newMigrationBackend(log, migrationEndpoint{Mode: j.To, Slot: j.TargetSlot, Config: targetCfg, Downloads: downloads})
	if err != nil {
		return nil, fmt.Errorf("target %s: %w", j.To, err)
	}

	m := &inPlaceMigration{log: log, store: store, journal: j, targetCfg: targetCfg}

	m.spec = migrate.InPlaceSpec{
		Source:     source,
		SourceRoot: sourceRoot,
		Target:     cleanTargetBackend{target},
		TargetSpec: isolation.NodeSpec{Slot: j.TargetSlot, Version: targetCfg.Cluster.Version},
		TargetRoot: targetRoot,

		// The target backend's own host requirements, checked before anything
		// is changed.
		Preflight: func(ctx context.Context, _ isolation.Backend) error {
			return preflightMigrationTarget(ctx, log, j.To)
		},
		// The target's artifacts for the same configuration a repave would use.
		Download: func(ctx context.Context) error { return stageNodeArchives(ctx, log, archives) },
		// The files both stacks write at the same host paths, such as the kubelet
		// identity. A rollback restarts the source, which needs them as they were.
		SnapshotConfig: func(context.Context) error {
			return confsnap.Take(inPlaceConfigSnapshotDir(), migrationConfigRoot(), migrationConfigPaths())
		},
		RestoreConfig: func(context.Context) error {
			err := confsnap.Restore(inPlaceConfigSnapshotDir(), migrationConfigRoot())
			if errors.Is(err, confsnap.ErrNoSnapshot) {
				// A migration an earlier agent started saved nothing.
				log.Warn("no saved shared configuration to restore")

				return nil
			}

			return err
		},
		DiscardConfig: func(context.Context) error { return confsnap.Discard(inPlaceConfigSnapshotDir()) },
		// As a repave does, after the source is stopped.
		CleanupNetwork: func(ctx context.Context) error { return cleanupMigrationNetwork(ctx, log) },
		PersistTarget: func(ctx context.Context, _ isolation.NodeRoot) error {
			return persistNodeConfig(ctx, log, j.TargetSlot, targetCfg)
		},
		// The source's applied config is only removed after success, so
		// restoring it is removing the target's record and rewriting the
		// source's from what was read.
		RestoreSource: func(ctx context.Context) error {
			if err := removeNodeConfig(ctx, log, j.TargetSlot); err != nil {
				return err
			}

			if sourceCfg == nil {
				return nil
			}

			return persistNodeConfig(ctx, log, j.SourceSlot, sourceCfg)
		},
		WaitReady:          m.waitReady,
		RemoveSourceConfig: func(ctx context.Context) error { return removeNodeConfig(ctx, log, j.SourceSlot) },
		Record:             m.record,
	}

	return m, nil
}

// waitReady is the repave readiness check for the mode (kubelet active on the
// host with its credentials), within a bounded time.
func (m *inPlaceMigration) waitReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, DefaultMigrationReadyTimeout)
	defer cancel()

	return waitKubeletOnHost(ctx, m.log, m.journal.To, m.journal.TargetSlot)
}

// conclude turns the outcome of a run or a resume into the journal's terminal
// state.
func (m *inPlaceMigration) conclude(_ context.Context, res migrate.Result, runErr error) error {
	switch {
	case runErr == nil, res.Target != nil:
		// The node runs in the target mode. A cleanup error leaves the journal
		// at its last step so the next start finishes the cleanup.
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

// migrateInPlace moves the node between nspawn and container by building the
// target in the alternate slot beside the running source. See the design,
// section 10.3, and migrate.RunInPlace for the steps and the rollback.
func migrateInPlace(ctx context.Context, log *slog.Logger, active *ActiveMachine, desired *provision.UnboundedAgentConfig) error {
	sourceSlot := active.Name
	targetSlot := goalstates.AlternateMachine(sourceSlot)

	target, err := desired.IsolationMode()
	if err != nil {
		return err
	}

	// The alternate slot is dead only when nothing records it as in use.
	// An unreadable record is not a node: persisting over it fails later and
	// rolls back.
	if _, err := readMigrationConfig(log, targetSlot); err == nil {
		return fmt.Errorf("slot %s is still recorded as in use; refusing to build the new node over it", targetSlot)
	}

	downloads, archives, err := resolveNodeDownloads(ctx, desired)
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
		From:     active.Mode, To: target, SourceSlot: sourceSlot, TargetSlot: targetSlot,
	}

	m, err := newInPlaceMigration(ctx, log, inPlaceJournalStore(), j, active.Config, targetCfg, downloads, archives)
	if err != nil {
		return err
	}

	log.Info("starting in-place isolation change", "from", active.Mode, "to", target, "source_slot", sourceSlot, "target_slot", targetSlot)

	res, runErr := migrate.RunInPlace(ctx, m.spec)

	return m.conclude(ctx, res, runErr)
}

// resumeInPlace decides, from the journal, what to do with a migration that was
// interrupted: see migrate.ResumeInPlace.
func resumeInPlace(ctx context.Context, log *slog.Logger, j migrate.Journal) error {
	sourceCfg, err := readMigrationConfig(log, j.SourceSlot)

	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist) && (j.Step == migrate.StepReady):
		// Source cleanup already removed its record; only the destroy is left.
		sourceCfg = nil
	default:
		return fmt.Errorf("read source applied config: %w", err)
	}

	var targetCfg *provision.AgentConfig

	if recorded, err := readMigrationConfig(log, j.TargetSlot); err == nil {
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

	store := inPlaceJournalStore()

	m, err := newInPlaceMigration(ctx, log, store, j, src, targetCfg, nil, nil)
	if err != nil {
		return err
	}

	m.saved = true

	log.Warn("resuming an interrupted isolation change", "to", j.To, "step", j.Step)

	res, runErr := migrate.ResumeInPlace(ctx, m.spec, j.Step)

	return m.conclude(ctx, res, runErr)
}
