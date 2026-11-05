// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/confsnap"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
)

// The in-place engine runs between two modes that run in a slot. Only nspawn
// does today, so these tests move an nspawn node from one slot to the other
// "in place", which exercises every step and rollback of the engine with fake
// backends.

// hookedBackend lets a test run code when the target starts.
type hookedBackend struct {
	isolation.Backend
	h *migHarness
}

func (b hookedBackend) Start(ctx context.Context, r isolation.SlotRef) error {
	if b.h.onTargetStart != nil {
		b.h.onTargetStart()
	}

	return b.Backend.Start(ctx, r)
}

// sharedTestPaths stands in for confsnap.SharedPaths in the tests: a file, a
// credentials directory and a drop-in directory that does not exist yet.
var sharedTestPaths = []string{
	"/var/lib/kubelet/config.yaml",
	"/var/lib/kubelet/pki",
	"/etc/containerd/conf.d",
}

// migHarness replaces every host-touching step of a migration with a
// recorder, through the fields of a migrationEnv.
type migHarness struct {
	t      *testing.T
	events []string
	env    *migrationEnv

	source, target *isolation.FakeBackend
	applied        map[string]*provision.AgentConfig
	failAt         map[string]error
	// hostRoot holds the shared configuration files the tests change.
	hostRoot string
	// onTargetStart runs when the target backend starts, as the target stack
	// would rewrite the shared files.
	onTargetStart func()
	active        *ActiveMachine
	dir           string
}

const testNode = "node-1"

func newMigHarness(t *testing.T, from isolation.Mode) *migHarness {
	t.Helper()

	h := &migHarness{
		t:       t,
		applied: map[string]*provision.AgentConfig{},
		failAt:  map[string]error{},
		dir:     t.TempDir(),
	}

	h.source = &isolation.FakeBackend{FakeMode: from, Dir: filepath.Join(h.dir, "machines", "source")}
	h.target = &isolation.FakeBackend{FakeMode: isolation.ModeNSpawn, Dir: filepath.Join(h.dir, "machines", "target")}

	cfg := baseConfig()
	cfg.Isolation = string(from)
	cfg.NodeName = testNode
	cfg.Kubelet.Labels = map[string]string{"role": "gpu"}
	// The host opts in to the experimental container mode, which the in-place
	// tests change to and from.
	cfg.Experimental = &provision.ExperimentalConfig{ContainerIsolation: true}
	h.active = &ActiveMachine{Name: "kube1", Config: cfg, Mode: from}
	h.applied["kube1"] = cfg
	h.hostRoot = filepath.Join(h.dir, "hostroot")

	record := func(event string) error {
		h.events = append(h.events, event)
		return h.failAt[event]
	}

	h.env = &migrationEnv{
		ConfigDir:      filepath.Join(h.dir, "agent"),
		ReprovisionDir: filepath.Join(h.dir, "reprovision"),
		UnitDir:        filepath.Join(h.dir, "units"),
		ConfigRoot:     h.hostRoot,
		ConfigPaths:    func() []string { return sharedTestPaths },
		NewBackend: func(_ *slog.Logger, e migrationEndpoint) (isolation.Backend, isolation.SlotRef, error) {
			if e.Source {
				return h.source, isolation.SlotName(e.Slot), nil
			}

			h.target.FakeMode = e.Mode

			return hookedBackend{Backend: h.target, h: h}, isolation.SlotName(e.Slot), nil
		},
		ResolveDownloads: func(context.Context, *provision.UnboundedAgentConfig) (*goalstates.DownloadOverrides, *goalstates.ContainerImageArchiveStaging, error) {
			return nil, nil, h.failAt["downloads"]
		},
		StageArchives: func(context.Context, *slog.Logger, *goalstates.ContainerImageArchiveStaging) error {
			return record("download")
		},
		CleanupNetwork: func(context.Context, *slog.Logger) error { return record("cleanup-network") },
		PersistConfig: func(_ context.Context, _ *slog.Logger, _ isolation.Mode, slot string, cfg *provision.AgentConfig) error {
			if err := record("persist " + slot); err != nil {
				return err
			}

			h.applied[slot] = cfg

			return nil
		},
		RemoveConfig: func(_ context.Context, _ *slog.Logger, _ isolation.Mode, slot string) error {
			delete(h.applied, slot)

			return record("remove-config " + slot)
		},
		ReadConfig: func(_ *slog.Logger, _ isolation.Mode, slot string) (*ActiveMachine, error) {
			if err := h.failAt["read "+slot]; err != nil {
				return nil, err
			}

			cfg, ok := h.applied[slot]
			if !ok {
				return nil, os.ErrNotExist
			}

			mode, err := cfg.IsolationMode()
			if err != nil {
				return nil, err
			}

			return &ActiveMachine{Name: slot, Config: cfg, Mode: mode}, nil
		},
		WaitReady: func(_ context.Context, _ *slog.Logger, _ isolation.Mode, slot string) error {
			h.events = append(h.events, "kubelet-on-host "+slot)
			return h.failAt["kubelet-on-host"]
		},
		Preflight: func(_ context.Context, _ *slog.Logger, mode isolation.Mode, _ migrate.Strategy) error {
			h.events = append(h.events, "preflight "+string(mode))
			return h.failAt["preflight"]
		},
		ProbeToken: func(context.Context, *rest.Config) error { return record("probe-token") },
		Systemctl: func(_ context.Context, _ *slog.Logger, args ...string) error {
			h.events = append(h.events, "systemctl "+strings.Join(args, " "))
			return h.failAt["systemctl"]
		},
		RunHandoff: func(_ context.Context, bin string, args ...string) error {
			h.events = append(h.events, "handoff "+filepath.Base(bin)+" "+strings.Join(args[:2], " "))
			return h.failAt["handoff"]
		},
		Executable: func() (string, error) {
			path := filepath.Join(h.dir, "agent-binary")
			return path, os.WriteFile(path, []byte("binary"), 0o755)
		},
		NodeReturnWait: 1,
		NodeReturnPoll: 1,
	}

	require.NoError(t, os.MkdirAll(h.env.ConfigDir, 0o750))

	return h
}

// desired is the configuration the node is changed to: the applied one, in
// target.
func (h *migHarness) desired(target isolation.Mode) *provision.UnboundedAgentConfig {
	desired := &provision.UnboundedAgentConfig{AgentConfig: *h.active.Config.DeepCopy()}
	desired.Isolation = string(target)

	return desired
}

func (h *migHarness) run(target isolation.Mode, strategy migrate.Strategy) error {
	return h.env.MigrateIsolation(context.Background(), discardLogger(), h.active, h.desired(target), strategy)
}

func (h *migHarness) journal() (migrate.Journal, error) {
	return h.env.inPlaceJournal().Load()
}

// installUnit puts the reprovision unit on disk, as a handoff that started
// leaves it.
func (h *migHarness) installUnit() {
	h.t.Helper()

	require.NoError(h.t, os.MkdirAll(h.env.unitDir(), 0o755))
	require.NoError(h.t, os.WriteFile(h.env.reprovisionUnitPath(), []byte(renderReprovisionUnit(h.env.reprovisionDir())), 0o644))
}

// ---------------------------------------------------------------------------
// In-place
// ---------------------------------------------------------------------------

func TestMigrateInPlace_BothDirections(t *testing.T) {
	for _, tt := range []struct{ from, to isolation.Mode }{
		{isolation.ModeNSpawn, isolation.ModeContainer},
		{isolation.ModeContainer, isolation.ModeNSpawn},
	} {
		t.Run(string(tt.from)+" to "+string(tt.to), func(t *testing.T) {
			h := newMigHarness(t, tt.from)

			require.NoError(t, h.run(tt.to, migrate.InPlace))

			assert.Equal(t, []string{
				"preflight " + string(tt.to),
				"download",
				"cleanup-network",
				"persist kube2",
				"kubelet-on-host kube2",
				"remove-config kube1",
			}, h.events)

			// Target: leftovers removed, built in the alternate slot, started.
			assert.Equal(t, []string{"destroy kube2", "prepare kube2", "start kube2"}, h.target.Calls())
			// Source: stopped, kept until Ready, then destroyed.
			assert.Equal(t, []string{"stop kube1", "destroy kube1"}, h.source.Calls())

			// The applied config moved to the new slot with the new mode, and
			// kept the node identity.
			require.NotContains(t, h.applied, "kube1")
			require.Contains(t, h.applied, "kube2")
			assert.Equal(t, string(tt.to), h.applied["kube2"].Isolation)
			assert.Equal(t, testNode, h.applied["kube2"].NodeName)
			assert.Equal(t, map[string]string{"role": "gpu"}, h.applied["kube2"].Kubelet.Labels)

			j, err := h.journal()
			require.NoError(t, err)
			assert.Equal(t, migrate.StepComplete, j.Step)
		})
	}
}

func TestMigrateInPlace_FromSlotTwoGoesToSlotOne(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.active.Name = "kube2"
	h.applied = map[string]*provision.AgentConfig{"kube2": h.active.Config}

	require.NoError(t, h.run(isolation.ModeContainer, migrate.InPlace))

	assert.Equal(t, []string{"destroy kube1", "prepare kube1", "start kube1"}, h.target.Calls())
	assert.Contains(t, h.applied, "kube1")
	assert.NotContains(t, h.applied, "kube2")
}

func TestMigrateInPlace_PreflightFailureChangesNothing(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.failAt["preflight"] = errors.New("no runtime")

	err := h.run(isolation.ModeContainer, migrate.InPlace)
	require.ErrorContains(t, err, "no runtime")

	assert.Equal(t, []string{"preflight container"}, h.events, "no download, no config change")
	assert.Empty(t, h.source.Calls())
	assert.Empty(t, h.target.Calls())

	_, jerr := h.journal()
	assert.ErrorIs(t, jerr, migrate.ErrNoJournal, "nothing was changed, so nothing is journaled")
}

func TestMigrateInPlace_DownloadFailureChangesNothing(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.failAt["download"] = errors.New("offline")

	require.ErrorContains(t, h.run(isolation.ModeContainer, migrate.InPlace), "offline")
	assert.Empty(t, h.source.Calls())
	assert.Empty(t, h.target.Calls())
}

func TestMigrateInPlace_ResolveDownloadsFailureChangesNothing(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.failAt["downloads"] = errors.New("registry unreachable")

	require.ErrorContains(t, h.run(isolation.ModeContainer, migrate.InPlace), "registry unreachable")
	assert.Empty(t, h.events)
}

func TestMigrateInPlace_RefusesWhenAlternateSlotIsRecordedInUse(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.applied["kube2"] = h.active.Config

	require.ErrorContains(t, h.run(isolation.ModeContainer, migrate.InPlace), "still recorded as in use")
	assert.Empty(t, h.events)
}

func TestMigrateInPlace_AnUnreadableAlternateRecordIsOverwrittenAndRollsBack(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.failAt["read kube2"] = errors.New("is a directory")
	h.failAt["persist kube2"] = errors.New("is a directory")

	require.ErrorContains(t, h.run(isolation.ModeContainer, migrate.InPlace), "rolled back to nspawn")
	assert.Equal(t, []string{"stop kube1", "start kube1"}, h.source.Calls())
}

func TestMigrateInPlace_PrepareFailureLeavesTheSourceRunning(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.target.PrepareErr = errors.New("unpack failed")

	require.ErrorContains(t, h.run(isolation.ModeContainer, migrate.InPlace), "unpack failed")
	assert.Empty(t, h.source.Calls())
	assert.NotContains(t, h.events, "cleanup-network", "the source is not touched before the target is built")
}

func TestMigrateInPlace_TargetStartFailureRollsBackToTheSource(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.target.StartErr = errors.New("container did not start")

	err := h.run(isolation.ModeContainer, migrate.InPlace)
	require.ErrorContains(t, err, "container did not start")
	require.ErrorContains(t, err, "rolled back to nspawn")

	assert.Equal(t, []string{"stop kube1", "start kube1"}, h.source.Calls(), "the source is restarted and never destroyed")
	assert.Equal(t, []string{"destroy kube2", "prepare kube2", "start kube2", "stop kube2", "destroy kube2", "status kube2"}, h.target.Calls())
	assert.NotContains(t, h.applied, "kube2")
	assert.Contains(t, h.applied, "kube1")

	j, jerr := h.journal()
	require.NoError(t, jerr)
	assert.Equal(t, migrate.StepRolledBack, j.Step)
}

func TestMigrateInPlace_PersistFailureRestoresTheSourceConfig(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.failAt["persist kube2"] = errors.New("disk full")

	require.ErrorContains(t, h.run(isolation.ModeContainer, migrate.InPlace), "disk full")

	assert.Contains(t, h.events, "remove-config kube2")
	assert.Contains(t, h.events, "persist kube1", "the source's applied config is written back")
	assert.Equal(t, []string{"stop kube1", "start kube1"}, h.source.Calls())
}

func TestMigrateInPlace_ReadinessFailureRollsBack(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.failAt["kubelet-on-host"] = errors.New("kubelet inactive")

	err := h.run(isolation.ModeContainer, migrate.InPlace)
	require.ErrorContains(t, err, "wait for the target node stack")
	require.ErrorContains(t, err, "rolled back to nspawn")

	assert.Equal(t, []string{"stop kube1", "start kube1"}, h.source.Calls())
	assert.NotContains(t, h.applied, "kube2")
	assert.NotContains(t, h.events, "remove-config kube1", "the source record survives until the target is up")
}

func TestMigrateInPlace_LocalKubeletFailureRollsBack(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.failAt["kubelet-on-host"] = errors.New("kubelet inactive")

	require.ErrorContains(t, h.run(isolation.ModeContainer, migrate.InPlace), "kubelet inactive")
	assert.Equal(t, []string{"stop kube1", "start kube1"}, h.source.Calls())
}

func TestMigrateInPlace_RollbackFailureIsJournaled(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.target.StartErr = errors.New("container did not start")
	h.source.StartErr = errors.New("machine will not start")

	err := h.run(isolation.ModeContainer, migrate.InPlace)
	require.ErrorIs(t, err, migrate.ErrRollbackFailed)
	require.ErrorContains(t, err, "needs attention")

	j, jerr := h.journal()
	require.NoError(t, jerr)
	assert.Equal(t, migrate.StepFailed, j.Step)
}

func TestMigrateInPlace_CleanupFailureStillSucceedsAndKeepsTheJournalToRetry(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.source.DestroyErr = errors.New("busy")

	require.NoError(t, h.run(isolation.ModeContainer, migrate.InPlace))

	j, err := h.journal()
	require.NoError(t, err)
	assert.Equal(t, migrate.StepReady, j.Step, "the next daemon start finishes the cleanup")
}

// ---------------------------------------------------------------------------
// Crash safety: resume from the journal
// ---------------------------------------------------------------------------

func writeJournal(t *testing.T, h *migHarness, step migrate.Step, mutate ...func(*migrate.Journal)) migrate.Journal {
	t.Helper()

	j := migrate.Journal{
		Strategy: migrate.InPlace,
		From:     isolation.ModeNSpawn, To: isolation.ModeContainer,
		SourceSlot: "kube1", TargetSlot: "kube2", Step: step,
	}
	for _, m := range mutate {
		m(&j)
	}

	require.NoError(t, h.env.inPlaceJournal().Save(j))

	return j
}

func TestMigrateInPlace_ResumeAtEveryStep(t *testing.T) {
	tests := []struct {
		step       migrate.Step
		persisted  bool // the target's applied config is on disk
		wantErr    string
		source     []string
		target     []string
		wantEvents []string
		final      migrate.Step
		wantConfig []string // applied slots afterwards
	}{
		{
			step: migrate.StepPreparing, wantErr: "interrupted",
			target: []string{"destroy kube2"}, final: migrate.StepRolledBack, wantConfig: []string{"kube1"},
		},
		{
			step: migrate.StepStoppingSource, wantErr: "interrupted",
			source: []string{"start kube1"}, target: []string{"stop kube2", "destroy kube2", "status kube2"},
			final: migrate.StepRolledBack, wantConfig: []string{"kube1"},
		},
		{
			step: migrate.StepStartingTarget, wantErr: "interrupted",
			source: []string{"start kube1"}, target: []string{"stop kube2", "destroy kube2", "status kube2"},
			final: migrate.StepRolledBack, wantConfig: []string{"kube1"},
		},
		{
			step: migrate.StepPersisting, persisted: true, wantErr: "interrupted",
			source: []string{"start kube1"}, target: []string{"stop kube2", "destroy kube2", "status kube2"},
			wantEvents: []string{"remove-config kube2", "persist kube1"}, final: migrate.StepRolledBack, wantConfig: []string{"kube1"},
		},
		{
			step: migrate.StepAwaitingReady, persisted: true,
			source: []string{"destroy kube1"}, wantEvents: []string{"kubelet-on-host kube2", "remove-config kube1"},
			final: migrate.StepComplete, wantConfig: []string{"kube2"},
		},
		{
			step: migrate.StepReady, persisted: true,
			source: []string{"destroy kube1"}, wantEvents: []string{"remove-config kube1"},
			final: migrate.StepComplete, wantConfig: []string{"kube2"},
		},
	}

	for _, tt := range tests {
		t.Run(string(tt.step), func(t *testing.T) {
			h := newMigHarness(t, isolation.ModeNSpawn)
			journal := writeJournal(t, h, tt.step)

			if tt.persisted {
				target := h.active.Config.DeepCopy()
				target.Isolation = string(isolation.ModeContainer)
				h.applied["kube2"] = target
			}

			// The journal is found again after the restart.
			err := h.env.resolveJournal(context.Background(), discardLogger(), journal)
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.wantErr)
			}

			assert.Equal(t, tt.source, nilIfEmpty(h.source.Calls()))
			assert.Equal(t, tt.target, nilIfEmpty(h.target.Calls()))
			assert.Equal(t, tt.wantEvents, h.events)

			var slots []string
			for slot := range h.applied {
				slots = append(slots, slot)
			}

			assert.ElementsMatch(t, tt.wantConfig, slots)

			j, jerr := h.journal()
			require.NoError(t, jerr)
			assert.Equal(t, tt.final, j.Step)
		})
	}
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}

	return s
}

func TestMigrateInPlace_ResumeAwaitingReadyRollsBackWhenTheTargetIsNotUp(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	journal := writeJournal(t, h, migrate.StepAwaitingReady)

	target := h.active.Config.DeepCopy()
	target.Isolation = string(isolation.ModeContainer)
	h.applied["kube2"] = target
	h.failAt["kubelet-on-host"] = errors.New("timed out")

	err := h.env.resolveJournal(context.Background(), discardLogger(), journal)
	require.ErrorContains(t, err, "timed out")
	assert.Equal(t, []string{"start kube1"}, h.source.Calls())
	assert.NotContains(t, h.applied, "kube2")
	assert.Contains(t, h.applied, "kube1")
}

// ---------------------------------------------------------------------------
// Reprovision
// ---------------------------------------------------------------------------

func TestMigrateReprovision_StagesTheHandoffAndStartsTheUnit(t *testing.T) {
	// Every pair that involves none, and the in-place pairs asked to reprovision.
	for _, tt := range []struct{ from, to isolation.Mode }{
		{isolation.ModeNSpawn, isolation.ModeNone},
		{isolation.ModeNone, isolation.ModeNSpawn},
	} {
		t.Run(string(tt.from)+" to "+string(tt.to), func(t *testing.T) {
			h := newMigHarness(t, tt.from)

			err := h.run(tt.to, migrate.Reprovision)
			require.ErrorIs(t, err, ErrMigrationHandedOff)

			// Preflight and the staged binary's check come before the unit
			// starts, and nothing drains or cordons the node.
			assert.Equal(t, []string{
				"preflight " + string(tt.to),
				"probe-token",
				"handoff unbounded-agent isolation-reprovision --check",
				"systemctl daemon-reload",
				"systemctl enable " + ReprovisionUnit,
				"systemctl start --no-block " + ReprovisionUnit,
			}, h.events)

			dir := h.env.reprovisionDir()
			assert.FileExists(t, filepath.Join(dir, ReprovisionBinaryName))

			cfgPath := filepath.Join(dir, ReprovisionConfigName)
			assert.FileExists(t, cfgPath)

			info, serr := os.Stat(cfgPath)
			require.NoError(t, serr)
			assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the configuration holds a token")

			data, rerr := os.ReadFile(cfgPath)
			require.NoError(t, rerr)
			assert.Contains(t, string(data), fmt.Sprintf("%q", string(tt.to)))

			j, jerr := h.env.reprovisionJournal().Load()
			require.NoError(t, jerr)
			assert.Equal(t, migrate.StepReprovisioning, j.Step)
			assert.Equal(t, migrate.Reprovision, j.Strategy)
			assert.Equal(t, tt.to, j.To)

			unit, uerr := os.ReadFile(h.env.reprovisionUnitPath())
			require.NoError(t, uerr)
			assert.Contains(t, string(unit), "isolation-reprovision --handoff-dir "+dir)
			assert.Contains(t, string(unit), "Restart=on-failure")
			assert.Contains(t, string(unit), "ConditionPathExists="+filepath.Join(dir, ReprovisionJournalName))

			// Nothing is reset by the daemon: the source node is untouched.
			assert.Empty(t, h.source.Calls())
			assert.Empty(t, h.target.Calls())
		})
	}
}

func TestMigrateReprovision_FailureBeforeTheUnitUndoesEverything(t *testing.T) {
	tests := []struct {
		name    string
		fail    func(h *migHarness)
		wantErr string
	}{
		{name: "preflight", fail: func(h *migHarness) { h.failAt["preflight"] = errors.New("read-only /opt") }, wantErr: "read-only /opt"},
		{name: "staged check", fail: func(h *migHarness) { h.failAt["handoff"] = errors.New("no plan") }, wantErr: "cannot be bootstrapped"},
		{name: "unit start", fail: func(h *migHarness) { h.failAt["systemctl"] = errors.New("boom") }, wantErr: "boom"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newMigHarness(t, isolation.ModeNSpawn)
			tt.fail(h)

			require.ErrorContains(t, h.run(isolation.ModeNone, migrate.Reprovision), tt.wantErr)

			assert.NoDirExists(t, h.env.reprovisionDir(), "no handoff is left behind")
			assert.Empty(t, h.source.Calls(), "the node stack was never stopped")
		})
	}
}

func TestMigrateReprovision_InvalidTargetConfigIsRejectedBeforeAnything(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.active.Config.Cluster.ClusterDNS = ""

	require.ErrorContains(t, h.run(isolation.ModeNone, migrate.Reprovision), "is invalid")
	assert.Empty(t, h.events)
	assert.NoDirExists(t, h.env.reprovisionDir())
}

func TestResolveReprovision(t *testing.T) {
	stage := func(h *migHarness, step migrate.Step) {
		require.NoError(t, h.env.reprovisionJournal().Save(migrate.Journal{
			Strategy: migrate.Reprovision, From: isolation.ModeNSpawn, To: isolation.ModeNone,
			Step: step,
		}))
		require.NoError(t, os.MkdirAll(h.env.reprovisionDir(), 0o700))
	}

	t.Run("still in progress until the target mode is applied", func(t *testing.T) {
		h := newMigHarness(t, isolation.ModeNSpawn)
		stage(h, migrate.StepBootstrapping)
		h.installUnit()

		j, _ := h.env.reprovisionJournal().Load() //nolint:errcheck // staged above
		require.ErrorIs(t, h.env.resolveJournal(context.Background(), discardLogger(), j), ErrMigrationHandedOff)
		assert.DirExists(t, h.env.reprovisionDir())
	})

	t.Run("reported once the target mode is applied", func(t *testing.T) {
		h := newMigHarness(t, isolation.ModeNSpawn)
		stage(h, migrate.StepReprovisioning)

		none := h.active.Config.DeepCopy()
		none.Isolation = string(isolation.ModeNone)
		h.applied = map[string]*provision.AgentConfig{"kube1": none}

		j, _ := h.env.reprovisionJournal().Load() //nolint:errcheck // staged above
		require.NoError(t, h.env.resolveJournal(context.Background(), discardLogger(), j))

		got, err := h.env.reprovisionJournal().Load()
		require.NoError(t, err)
		assert.Equal(t, migrate.StepBootstrapped, got.Step)
	})
}

func TestRenderReprovisionUnit(t *testing.T) {
	unit := renderReprovisionUnit("/var/lib/x")
	assert.Contains(t, unit, "ExecStart=/var/lib/x/unbounded-agent isolation-reprovision --handoff-dir /var/lib/x")
	assert.NotContains(t, unit, "\u2014", "no em-dashes")
	assert.Contains(t, unit, "StartLimitBurst=30")
	assert.Contains(t, unit, "RestartSec=30")
}

func TestMigrateReprovision_AttestedHostAttestsAgainInsteadOfReusingTheToken(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	require.NoError(t, os.WriteFile(h.env.attestPath(), []byte(`{"URL":"http://10.0.0.1:8880"}`), 0o600))

	// An expired token does not matter: the bootstrap gets a new one.
	h.failAt["probe-token"] = ErrBootstrapTokenRejected

	require.ErrorIs(t, h.run(isolation.ModeNone, migrate.Reprovision), ErrMigrationHandedOff)
	assert.NotContains(t, h.events, "probe-token", "an attested host does not depend on the recorded token")

	data, err := os.ReadFile(filepath.Join(h.env.reprovisionDir(), ReprovisionConfigName))
	require.NoError(t, err)

	var staged provision.UnboundedAgentConfig
	require.NoError(t, json.Unmarshal(data, &staged))
	require.NotNil(t, staged.Attest)
	assert.Equal(t, "http://10.0.0.1:8880", staged.Attest.URL)
}

func TestMigrateReprovision_HostWithoutAttestationStagesNone(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)

	require.ErrorIs(t, h.run(isolation.ModeNone, migrate.Reprovision), ErrMigrationHandedOff)

	data, err := os.ReadFile(filepath.Join(h.env.reprovisionDir(), ReprovisionConfigName))
	require.NoError(t, err)
	assert.NotContains(t, string(data), "Attest")
}

func TestMigrateReprovision_RejectedTokenFailsBeforeAnythingChanges(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.failAt["probe-token"] = ErrBootstrapTokenRejected

	err := h.run(isolation.ModeNone, migrate.Reprovision)
	require.ErrorIs(t, err, ErrBootstrapTokenRejected)
	require.NotErrorIs(t, err, ErrMigrationHandedOff)
	assert.Contains(t, err.Error(), "Renew the token")

	assert.Equal(t, []string{"preflight none", "probe-token"}, h.events, "nothing was staged, installed or started")
	assert.NoDirExists(t, h.env.reprovisionDir())
	assert.Empty(t, h.source.Calls())

	_, jerr := h.env.reprovisionJournal().Load()
	assert.ErrorIs(t, jerr, migrate.ErrNoJournal)
}

func TestMigrateReprovision_UnanswerableTokenCheckDoesNotBlockTheMigration(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.failAt["probe-token"] = errors.New("connection refused")

	require.ErrorIs(t, h.run(isolation.ModeNone, migrate.Reprovision), ErrMigrationHandedOff)
}

func TestMigrateReprovision_NoTokenToCheckForAnExecCredentialHost(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.active.Config.Kubelet.Auth = provision.KubeletAuthInfo{ExecCredential: &clientcmdapi.ExecConfig{Command: "cred"}}

	require.ErrorIs(t, h.run(isolation.ModeNone, migrate.Reprovision), ErrMigrationHandedOff)
	assert.NotContains(t, h.events, "probe-token")
}

func TestCheckBootstrapCredentialAgainstAServer(t *testing.T) {
	// The real probe, against servers that answer as an API server does.
	for _, tt := range []struct {
		name    string
		status  int
		wantErr error
	}{
		{"accepted", http.StatusOK, nil},
		{"rejected", http.StatusUnauthorized, ErrBootstrapTokenRejected},
		{"authenticated but forbidden is still accepted", http.StatusForbidden, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var gotAuth string

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Get("Authorization")

				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			err := defaultProbeBootstrapToken(context.Background(), &rest.Config{Host: srv.URL, BearerToken: "abc123.xyz789"})
			if tt.wantErr == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tt.wantErr)
			}

			assert.Equal(t, "Bearer abc123.xyz789", gotAuth)
		})
	}
}

// ---------------------------------------------------------------------------
// Shared host configuration across a migration
// ---------------------------------------------------------------------------

func (h *migHarness) hostWrite(rel, content string) {
	h.t.Helper()

	path := filepath.Join(h.hostRoot, rel)
	require.NoError(h.t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(h.t, os.WriteFile(path, []byte(content), 0o600))
}

func (h *migHarness) hostRead(rel string) string {
	h.t.Helper()

	data, err := os.ReadFile(filepath.Join(h.hostRoot, rel))
	require.NoError(h.t, err)

	return string(data)
}

// seedHost writes the files the source stack runs on.
func (h *migHarness) seedHost() {
	h.hostWrite("var/lib/kubelet/config.yaml", "source: config\n")
	h.hostWrite("var/lib/kubelet/pki/kubelet-client-current.pem", "SOURCE-KEY")
}

// targetRewritesTheHost is what starting the target stack does to the shared
// files: a new kubelet configuration, new client credentials and a new drop-in.
func (h *migHarness) targetRewritesTheHost() {
	h.hostWrite("var/lib/kubelet/config.yaml", "target: config\n")
	require.NoError(h.t, os.RemoveAll(filepath.Join(h.hostRoot, "var/lib/kubelet/pki")))
	h.hostWrite("var/lib/kubelet/pki/kubelet-client-current.pem", "TARGET-KEY")
	h.hostWrite("etc/containerd/conf.d/10-target.toml", "target")
}

func (h *migHarness) assertHostIsTheSources() {
	h.t.Helper()

	assert.Equal(h.t, "source: config\n", h.hostRead("var/lib/kubelet/config.yaml"))
	assert.Equal(h.t, "SOURCE-KEY", h.hostRead("var/lib/kubelet/pki/kubelet-client-current.pem"))
	assert.NoDirExists(h.t, filepath.Join(h.hostRoot, "etc/containerd/conf.d"))
}

func TestMigrateInPlace_RollbackRestoresTheSharedConfiguration(t *testing.T) {
	for _, failing := range []string{"cleanup-network", "persist kube2", "kubelet-on-host"} {
		t.Run(failing, func(t *testing.T) {
			h := newMigHarness(t, isolation.ModeNSpawn)
			h.seedHost()
			h.onTargetStart = h.targetRewritesTheHost
			h.failAt[failing] = errors.New("boom")

			err := h.run(isolation.ModeNSpawn, migrate.InPlace)
			require.ErrorContains(t, err, "rolled back to nspawn")

			h.assertHostIsTheSources()
			assert.NoDirExists(t, h.env.snapshotDir(), "the snapshot goes once it is restored")
		})
	}
}

func TestMigrateInPlace_TargetStartFailureRestoresTheSharedConfiguration(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.seedHost()
	h.onTargetStart = h.targetRewritesTheHost
	h.target.StartErr = errors.New("cannot start")

	require.ErrorContains(t, h.run(isolation.ModeNSpawn, migrate.InPlace), "cannot start")

	h.assertHostIsTheSources()
}

func TestMigrateInPlace_SuccessKeepsTheTargetsFilesAndDropsTheSnapshot(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.seedHost()
	h.onTargetStart = h.targetRewritesTheHost

	require.NoError(t, h.run(isolation.ModeNSpawn, migrate.InPlace))

	assert.Equal(t, "target: config\n", h.hostRead("var/lib/kubelet/config.yaml"))
	assert.Equal(t, "TARGET-KEY", h.hostRead("var/lib/kubelet/pki/kubelet-client-current.pem"))
	assert.NoDirExists(t, h.env.snapshotDir())
}

func TestMigrateInPlace_PreflightFailureSavesNothing(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.seedHost()
	h.failAt["preflight"] = errors.New("no runtime")

	require.Error(t, h.run(isolation.ModeNSpawn, migrate.InPlace))
	assert.NoDirExists(t, h.env.snapshotDir())
}

func TestMigrateInPlace_SnapshotFailureChangesNothing(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.seedHost()

	// A socket among the shared files cannot be saved, so nothing may start.
	sock := filepath.Join(h.hostRoot, "var/lib/kubelet/pki")
	require.NoError(t, os.MkdirAll(sock, 0o755))

	l, err := net.Listen("unix", filepath.Join(sock, "s"))
	require.NoError(t, err)

	defer l.Close() //nolint:errcheck // test cleanup

	require.ErrorContains(t, h.run(isolation.ModeNSpawn, migrate.InPlace), "save shared configuration")
	assert.Empty(t, h.source.Calls())
	assert.Empty(t, h.target.Calls())
}

func TestMigrateInPlace_RestoreFailureIsReportedAndKeepsTheSnapshot(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.seedHost()
	h.onTargetStart = func() {
		h.targetRewritesTheHost()
		// The drop-in the target created cannot be removed from a read-only
		// directory.
		require.NoError(t, os.Chmod(filepath.Join(h.hostRoot, "etc/containerd"), 0o500))
	}
	h.failAt["kubelet-on-host"] = errors.New("not ready")

	t.Cleanup(func() { _ = os.Chmod(filepath.Join(h.hostRoot, "etc/containerd"), 0o700) }) //nolint:errcheck // cleanup

	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}

	err := h.run(isolation.ModeNSpawn, migrate.InPlace)
	require.ErrorContains(t, err, "restore shared configuration")
	assert.DirExists(t, h.env.snapshotDir(), "kept so the restore can be tried again")
}

func TestMigrateInPlace_ResumeAfterACrashRestoresTheSharedConfiguration(t *testing.T) {
	for _, step := range []migrate.Step{migrate.StepPreparing, migrate.StepStoppingSource, migrate.StepStartingTarget, migrate.StepPersisting} {
		t.Run(string(step), func(t *testing.T) {
			h := newMigHarness(t, isolation.ModeNSpawn)
			h.seedHost()

			// The first daemon took the snapshot and the target rewrote the files
			// before it died.
			require.NoError(t, confsnap.Take(h.env.snapshotDir(), h.hostRoot, sharedTestPaths))
			h.targetRewritesTheHost()

			writeJournal(t, h, step)

			c := recoveryClient(t)
			recoverChanges(t, h, c)

			h.assertHostIsTheSources()
			assert.NoDirExists(t, h.env.snapshotDir())
			require.NotNil(t, getChange(t, c))
			assert.Equal(t, v1alpha3.IsolationChangeFailed, getChange(t, c).Phase)
		})
	}
}

func TestMigrateInPlace_ResumeWithoutASnapshotStillRollsBack(t *testing.T) {
	// A migration an earlier agent started saved no snapshot.
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.seedHost()
	writeJournal(t, h, migrate.StepStartingTarget)

	c := recoveryClient(t)
	recoverChanges(t, h, c)

	require.NotNil(t, getChange(t, c))
	assert.Equal(t, v1alpha3.IsolationChangeFailed, getChange(t, c).Phase)
	assert.Contains(t, h.source.Calls(), "start kube1")
}

func TestMigrateInPlace_ResumeOfAFinishedMigrationDropsTheSnapshot(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.seedHost()
	require.NoError(t, confsnap.Take(h.env.snapshotDir(), h.hostRoot, sharedTestPaths))
	h.targetRewritesTheHost()

	target := h.active.Config.DeepCopy()
	target.Isolation = string(isolation.ModeNSpawn)
	h.applied["kube2"] = target

	writeJournal(t, h, migrate.StepAwaitingReady)

	c := recoveryClient(t)
	recoverChanges(t, h, c)

	assert.Equal(t, "target: config\n", h.hostRead("var/lib/kubelet/config.yaml"), "a migration that held keeps the target's files")
	assert.NoDirExists(t, h.env.snapshotDir())
}

func TestMigrateReprovision_LeavingNoneDropsTheHostToolkitLocation(t *testing.T) {
	for _, target := range []isolation.Mode{isolation.ModeNSpawn} {
		t.Run(string(target), func(t *testing.T) {
			h := newMigHarness(t, isolation.ModeNone)
			h.active.Config.NvidiaToolkit = &config.NvidiaToolkitConfig{CTKPath: "/opt/tk/nvidia-ctk"}

			// The setting is only valid for mode none, so keeping it would make
			// the target configuration invalid.
			require.ErrorIs(t, h.run(target, migrate.Reprovision), ErrMigrationHandedOff)

			data, err := os.ReadFile(filepath.Join(h.env.reprovisionDir(), ReprovisionConfigName))
			require.NoError(t, err)
			assert.NotContains(t, string(data), "NvidiaToolkit")
		})
	}
}

func TestMigrateReprovision_EnteringNoneDoesNotInventAToolkitLocation(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)

	require.ErrorIs(t, h.run(isolation.ModeNone, migrate.Reprovision), ErrMigrationHandedOff)

	data, err := os.ReadFile(filepath.Join(h.env.reprovisionDir(), ReprovisionConfigName))
	require.NoError(t, err)
	assert.NotContains(t, string(data), "NvidiaToolkit", "an unset location stays unset")
}
