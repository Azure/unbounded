// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"k8s.io/client-go/rest"

	"github.com/Azure/unbounded/cmd/agent/internal/nspawnops"
	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/confsnap"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
	"github.com/Azure/unbounded/pkg/agent/isolation/nspawn"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

// InPlaceJournalName is the file name of the in-place migration journal, which
// lives next to the applied configs. While a migration has two applied configs
// recorded, it says which one the node runs on.
const InPlaceJournalName = "isolation-migration.json"

// inPlaceJournalPathIn is the in-place journal in the agent configuration
// directory dir.
func inPlaceJournalPathIn(dir string) string { return filepath.Join(dir, InPlaceJournalName) }

// DefaultMigrationReadyTimeout bounds the wait for the target node stack to come
// up on the host.
const DefaultMigrationReadyTimeout = 5 * time.Minute

// ErrMigrationHandedOff is returned by a mode change that continues without
// this daemon: a reprovision resets the host, which removes the daemon, and a
// handoff command finishes the bootstrap. The change stays InProgress in the
// Machine status and the daemon that starts afterward reports it.
var ErrMigrationHandedOff = errors.New("isolation migration handed off to the reprovision unit")

const (
	// ReprovisionDir holds what a reprovision needs to survive its own reset:
	// the journal, a copy of the agent binary and the target configuration. The
	// reset does not remove it; the daemon that reports the outcome does.
	ReprovisionDir = "/var/lib/unbounded-agent-reprovision"
	// ReprovisionJournalName, ReprovisionConfigName and ReprovisionBinaryName
	// are the files in ReprovisionDir.
	ReprovisionJournalName = "journal.json"
	ReprovisionConfigName  = "config.json"
	ReprovisionBinaryName  = "unbounded-agent"
	// ReprovisionUnit is the systemd unit that runs the handoff command.
	ReprovisionUnit = "unbounded-agent-reprovision.service"
	// ReprovisionGiveUpAfter is how long after the handoff started its runs
	// keep being retried. It is below machina's ChangeStale threshold (two
	// hours), so a handoff that gives up reports Failed before machina would
	// call the change stale; ChangeStale then means the agent could not report
	// at all.
	ReprovisionGiveUpAfter = 90 * time.Minute
	// ReprovisionRestartDelay is the unit's RestartSec, the shortest pause
	// between two runs. The command adds a growing delay of its own on top.
	ReprovisionRestartDelay = 30 * time.Second
	// ReprovisionMaxBackoff caps the delay between two runs.
	ReprovisionMaxBackoff = 10 * time.Minute
)

// inPlaceConfigSnapshotName is the directory of the saved shared configuration,
// next to the in-place journal, so a restarted daemon can still restore it.
const inPlaceConfigSnapshotName = "isolation-migration-config"

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

// migrationEnv is everything a mode change touches outside its own journals:
// where its files live and every step that changes the host or asks the
// cluster. The zero value of each field is the real host; tests set them.
type migrationEnv struct {
	// ConfigDir is the agent configuration directory: the applied configs,
	// the in-place journal and its configuration snapshot. Empty is
	// goalstates.AgentConfigDir.
	ConfigDir string
	// ReprovisionDir is where a reprovision is staged. Empty is ReprovisionDir.
	ReprovisionDir string
	// UnitDir is where the reprovision unit is written. Empty is
	// goalstates.SystemdSystemDir.
	UnitDir string
	// ConfigRoot prefixes the shared configuration paths a snapshot saves.
	// Empty is the host root.
	ConfigRoot string
	// ConfigPaths lists the shared configuration paths. Nil is
	// confsnap.SharedPaths.
	ConfigPaths func() []string

	// NewBackend builds the backend and slot of one end of an in-place
	// migration. Nil builds the real backend of the mode.
	NewBackend func(log *slog.Logger, e migrationEndpoint) (isolation.Backend, isolation.SlotRef, error)
	// ResolveDownloads resolves the download overrides and offline archives of
	// a configuration. Nil is provision.ResolveDownloadOverridesWithOfflineArtifacts.
	ResolveDownloads func(ctx context.Context, cfg *provision.UnboundedAgentConfig) (*goalstates.DownloadOverrides, *goalstates.ContainerImageArchiveStaging, error)
	// StageArchives stages the offline container image archives.
	StageArchives func(ctx context.Context, log *slog.Logger, staging *goalstates.ContainerImageArchiveStaging) error
	// CleanupNetwork removes the host networking a stopped node left behind.
	CleanupNetwork func(ctx context.Context, log *slog.Logger) error
	// PersistConfig records cfg as the applied config of slot in mode.
	PersistConfig func(ctx context.Context, log *slog.Logger, mode isolation.Mode, slot string, cfg *provision.AgentConfig) error
	// RemoveConfig removes the applied config of slot in mode.
	RemoveConfig func(ctx context.Context, log *slog.Logger, mode isolation.Mode, slot string) error
	// ReadConfig reads the applied config of slot in mode. A missing one is an
	// error that matches os.ErrNotExist.
	ReadConfig func(log *slog.Logger, mode isolation.Mode, slot string) (*ActiveMachine, error)
	// WaitReady waits for the node stack of slot in mode to be up on the host.
	WaitReady func(ctx context.Context, log *slog.Logger, mode isolation.Mode, slot string) error
	// Preflight checks the target mode's host requirements for a change by
	// strategy, without changing anything.
	Preflight func(ctx context.Context, log *slog.Logger, mode isolation.Mode, strategy migrate.Strategy) error
	// ReadAttest reads the recorded attestation settings.
	ReadAttest func() (*provision.AgentAttestConfig, error)
	// ProbeToken asks the API server whether it accepts a bootstrap token.
	ProbeToken func(ctx context.Context, cfg *rest.Config) error
	// Systemctl runs systemctl.
	Systemctl func(ctx context.Context, log *slog.Logger, args ...string) error
	// RunHandoff runs the staged binary, for its --check.
	RunHandoff func(ctx context.Context, bin string, args ...string) error
	// Executable is the running agent binary, copied into the handoff.
	Executable func() (string, error)
	// Now is the clock.
	Now func() time.Time

	// NodeReturnWait is how long a failed change gives the Node object to come
	// back before the node stack is restarted, and NodeReturnPoll how often it
	// looks. Zero is 45 and 5 seconds.
	NodeReturnWait, NodeReturnPoll time.Duration
}

func (e *migrationEnv) configDir() string {
	if e.ConfigDir != "" {
		return e.ConfigDir
	}

	return goalstates.AgentConfigDir
}

func (e *migrationEnv) reprovisionDir() string {
	if e.ReprovisionDir != "" {
		return e.ReprovisionDir
	}

	return ReprovisionDir
}

func (e *migrationEnv) unitDir() string {
	if e.UnitDir != "" {
		return e.UnitDir
	}

	return goalstates.SystemdSystemDir
}

func (e *migrationEnv) inPlaceJournal() migrate.JournalStore {
	return migrate.JournalStore{Path: inPlaceJournalPathIn(e.configDir()), Now: e.Now}
}

func (e *migrationEnv) reprovisionJournal() migrate.JournalStore {
	return migrate.JournalStore{Path: filepath.Join(e.reprovisionDir(), ReprovisionJournalName), Now: e.Now}
}

// journals are both journal stores, in-place first.
func (e *migrationEnv) journals() []migrate.JournalStore {
	return []migrate.JournalStore{e.inPlaceJournal(), e.reprovisionJournal()}
}

func (e *migrationEnv) snapshotDir() string {
	return filepath.Join(e.configDir(), inPlaceConfigSnapshotName)
}

func (e *migrationEnv) configPaths() []string {
	if e.ConfigPaths != nil {
		return e.ConfigPaths()
	}

	return confsnap.SharedPaths()
}

func (e *migrationEnv) attestPath() string {
	return filepath.Join(e.configDir(), attestRecordName)
}

func (e *migrationEnv) appliedPath(mode isolation.Mode, slot string) string {
	return filepath.Join(e.configDir(), goalstates.AppliedConfigName(mode, slot)+"-applied-config.json")
}

func (e *migrationEnv) newBackend(log *slog.Logger, ep migrationEndpoint) (isolation.Backend, isolation.SlotRef, error) {
	if e.NewBackend != nil {
		return e.NewBackend(log, ep)
	}

	switch ep.Mode {
	case isolation.ModeNSpawn:
		return nspawnops.Backend(nspawnops.Params{Log: log, Config: ep.Config, Downloads: ep.Downloads}), nspawn.Root(ep.Slot), nil
	default:
		return nil, nil, fmt.Errorf("in-place migration is not supported for isolation mode %q", ep.Mode)
	}
}

func (e *migrationEnv) resolveDownloads(ctx context.Context, cfg *provision.UnboundedAgentConfig) (*goalstates.DownloadOverrides, *goalstates.ContainerImageArchiveStaging, error) {
	if e.ResolveDownloads != nil {
		return e.ResolveDownloads(ctx, cfg)
	}

	return provision.ResolveDownloadOverridesWithOfflineArtifacts(ctx, cfg)
}

func (e *migrationEnv) stageArchives(ctx context.Context, log *slog.Logger, staging *goalstates.ContainerImageArchiveStaging) error {
	if e.StageArchives != nil {
		return e.StageArchives(ctx, log, staging)
	}

	return rootfs.DownloadContainerImageArchives(log, staging).Do(ctx)
}

func (e *migrationEnv) cleanupNetwork(ctx context.Context, log *slog.Logger) error {
	if e.CleanupNetwork != nil {
		return e.CleanupNetwork(ctx, log)
	}

	return reset.CleanupNetwork(log).Do(ctx)
}

func (e *migrationEnv) persistConfig(ctx context.Context, log *slog.Logger, mode isolation.Mode, slot string, cfg *provision.AgentConfig) error {
	if e.PersistConfig != nil {
		return e.PersistConfig(ctx, log, mode, slot, cfg)
	}

	return PersistAppliedConfigFor(log, mode, slot, cfg).Do(ctx)
}

func (e *migrationEnv) removeConfig(ctx context.Context, log *slog.Logger, mode isolation.Mode, slot string) error {
	if e.RemoveConfig != nil {
		return e.RemoveConfig(ctx, log, mode, slot)
	}

	return RemoveAppliedConfig(log, goalstates.AppliedConfigName(mode, slot)).Do(ctx)
}

func (e *migrationEnv) readConfig(log *slog.Logger, mode isolation.Mode, slot string) (*ActiveMachine, error) {
	if e.ReadConfig != nil {
		return e.ReadConfig(log, mode, slot)
	}

	loc := goalstates.AppliedConfigLocation{Name: goalstates.AppliedConfigName(mode, slot), Slot: slot, Mode: mode}

	return readAppliedLocation(log, loc, e.appliedPath(mode, slot))
}

func (e *migrationEnv) waitReady(ctx context.Context, log *slog.Logger, mode isolation.Mode, slot string) error {
	if e.WaitReady != nil {
		return e.WaitReady(ctx, log, mode, slot)
	}

	// The readiness check a repave uses: the kubelet is active and has its
	// credentials.
	if mode != isolation.ModeNSpawn {
		return nil
	}

	return nodestart.WaitForKubelet(log, slot).Do(ctx)
}

func (e *migrationEnv) preflight(ctx context.Context, log *slog.Logger, mode isolation.Mode, strategy migrate.Strategy) error {
	if e.Preflight != nil {
		return e.Preflight(ctx, log, mode, strategy)
	}

	return preflightMigrationTarget(ctx, log, mode, strategy)
}

func (e *migrationEnv) readAttest() (*provision.AgentAttestConfig, error) {
	if e.ReadAttest != nil {
		return e.ReadAttest()
	}

	return readAttestRecord(e.attestPath())
}

func (e *migrationEnv) probeToken(ctx context.Context, cfg *rest.Config) error {
	if e.ProbeToken != nil {
		return e.ProbeToken(ctx, cfg)
	}

	return defaultProbeBootstrapToken(ctx, cfg)
}

func (e *migrationEnv) systemctl(ctx context.Context, log *slog.Logger, args ...string) error {
	if e.Systemctl != nil {
		return e.Systemctl(ctx, log, args...)
	}

	return executil.RunCmd(ctx, log, executil.Systemctl(), args...)
}

func (e *migrationEnv) runHandoff(ctx context.Context, bin string, args ...string) error {
	if e.RunHandoff != nil {
		return e.RunHandoff(ctx, bin, args...)
	}

	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput() //nolint:gosec // the binary is the agent's own staged copy
	if err != nil {
		return fmt.Errorf("%s %v: %w: %s", bin, args, err, out)
	}

	return nil
}

func (e *migrationEnv) executable() (string, error) {
	if e.Executable != nil {
		return e.Executable()
	}

	return os.Executable()
}

func (e *migrationEnv) nodeReturn() (wait, poll time.Duration) {
	wait, poll = e.NodeReturnWait, e.NodeReturnPoll
	if wait == 0 {
		wait = 45 * time.Second
	}

	if poll == 0 {
		poll = 5 * time.Second
	}

	return wait, poll
}
