// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
)

func recoveryClient(t *testing.T) client.Client {
	t.Helper()

	return fakeStatusClient(&v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 4}})
}

func recoveryStore(t *testing.T) *installstate.Store {
	t.Helper()

	return installstate.NewStore(filepath.Join(t.TempDir(), "state"), filepath.Join(t.TempDir(), "install.lock"))
}

// recoveryOperator is the node operator the recovery reads the applied config
// from afterwards.
func recoveryOperator(h *migHarness) *fakeNodeOperator {
	return &fakeNodeOperator{active: h.active}
}

func recoverChanges(t *testing.T, h *migHarness, c client.Client) {
	t.Helper()

	recoverIsolationMigration(context.Background(), discardLogger(), c, recoveryStore(t), "test-machine", recoveryOperator(h))
}

func getChange(t *testing.T, c client.Client) *v1alpha3.IsolationChangeStatus {
	t.Helper()

	var m v1alpha3.Machine
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "test-machine"}, &m))

	return m.Status.IsolationChange
}

func TestRecoverIsolationMigration_NothingToDo(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	c := recoveryClient(t)

	recoverChanges(t, h, c)

	assert.Empty(t, h.events)
	assert.Nil(t, getChange(t, c))
}

func TestRecoverIsolationMigration_ReportsAnInPlaceOutcome(t *testing.T) {
	tests := []struct {
		name      string
		step      migrate.Step
		persisted bool
		wantPhase v1alpha3.IsolationChangePhase
		wantMsg   string
	}{
		{name: "target recorded and Ready", step: migrate.StepAwaitingReady, persisted: true, wantPhase: v1alpha3.IsolationChangeSucceeded, wantMsg: "now runs in isolation mode container"},
		{name: "crashed while starting the target", step: migrate.StepStartingTarget, wantPhase: v1alpha3.IsolationChangeFailed, wantMsg: "interrupted"},
		{name: "already rolled back", step: migrate.StepRolledBack, wantPhase: v1alpha3.IsolationChangeFailed, wantMsg: "readiness timed out"},
		{name: "already complete", step: migrate.StepComplete, wantPhase: v1alpha3.IsolationChangeSucceeded, wantMsg: "now runs in isolation mode container"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newMigHarness(t, isolation.ModeNSpawn)
			writeJournal(t, tt.step, func(j *migrate.Journal) { j.Message = "readiness timed out" })

			if tt.persisted {
				target := h.active.Config.DeepCopy()
				target.Isolation = string(isolation.ModeContainer)
				h.applied["kube2"] = target
			}

			c := recoveryClient(t)

			recoverChanges(t, h, c)

			change := getChange(t, c)
			require.NotNil(t, change)
			assert.Equal(t, tt.wantPhase, change.Phase)
			assert.Contains(t, change.Message, tt.wantMsg)
			assert.Equal(t, v1alpha3.IsolationContainer, change.TargetMode)
			assert.Equal(t, v1alpha3.IsolationChangeInPlace, change.Strategy)

			_, err := h.journal()
			assert.ErrorIs(t, err, migrate.ErrNoJournal, "the journal goes once the outcome is reported")
		})
	}
}

func TestRecoverIsolationMigration_ReprovisionReportedByTheNewDaemon(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)

	require.NoError(t, reprovisionJournalFor().Save(migrate.Journal{
		Strategy: migrate.Reprovision, From: isolation.ModeNSpawn, To: isolation.ModeNone,
		Step: migrate.StepBootstrapping,
	}))

	// The bootstrap has not finished: the change is still in progress.
	c := recoveryClient(t)
	recoverChanges(t, h, c)

	change := getChange(t, c)
	require.NotNil(t, change)
	assert.Equal(t, v1alpha3.IsolationChangeInProgress, change.Phase)
	assert.Equal(t, v1alpha3.IsolationChangeReprovision, change.Strategy)

	_, err := reprovisionJournalFor().Load()
	require.NoError(t, err, "the journal stays until the outcome is known")

	// Once the applied config is in the target mode it is reported, the observed
	// mode follows, and the journal and handoff go.
	none := h.active.Config.DeepCopy()
	none.Isolation = string(isolation.ModeNone)
	h.applied = map[string]*provision.AgentConfig{"kube1": none}

	op := recoveryOperator(h)
	op.active = &ActiveMachine{Name: "kube1", Config: none, Mode: isolation.ModeNone}

	recoverIsolationMigration(context.Background(), discardLogger(), c, recoveryStore(t), "test-machine", op)

	change = getChange(t, c)
	require.NotNil(t, change)
	assert.Equal(t, v1alpha3.IsolationChangeSucceeded, change.Phase)
	assert.Contains(t, change.Message, "isolation mode none")

	var m v1alpha3.Machine
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "test-machine"}, &m))
	assert.Equal(t, v1alpha3.IsolationNone, m.Status.Isolation, "the observed mode follows the recovered change")

	_, err = reprovisionJournalFor().Load()
	assert.ErrorIs(t, err, migrate.ErrNoJournal)
	assert.NoDirExists(t, reprovisionDir())
}

func TestRecoverIsolationMigration_MarksTheDesiredConfigurationAppliedAfterASuccess(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	writeJournal(t, migrate.StepComplete)

	version := int32(2)
	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 4},
		Spec:       v1alpha3.MachineSpec{ConfigurationRef: &v1alpha3.MachineConfigurationRef{Name: "config-a", Version: &version}},
	}
	mcv := machineConfigurationVersion("config-a", version, v1alpha3.MachineConfigurationTemplate{
		Kubernetes: &v1alpha3.MachineConfigurationKubernetes{Version: h.active.Config.Cluster.Version},
		Agent:      &v1alpha3.MachineConfigurationAgent{Image: h.active.Config.OCIImage},
	})
	c := fakeStatusClient(machine, mcv)

	recoverChanges(t, h, c)

	var got v1alpha3.Machine
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "test-machine"}, &got))
	require.NotNil(t, got.Status.Configuration)
	assert.Equal(t, int32(2), got.Status.Configuration.Version)
}

func TestRecoverIsolationMigration_DoesNotMarkAFailedChangeApplied(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	writeJournal(t, migrate.StepRolledBack, func(j *migrate.Journal) { j.Message = "boom" })

	version := int32(2)
	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 4},
		Spec:       v1alpha3.MachineSpec{ConfigurationRef: &v1alpha3.MachineConfigurationRef{Name: "config-a", Version: &version}},
	}
	mcv := machineConfigurationVersion("config-a", version, v1alpha3.MachineConfigurationTemplate{
		Kubernetes: &v1alpha3.MachineConfigurationKubernetes{Version: "v9.9.9"},
	})
	c := fakeStatusClient(machine, mcv)

	recoverChanges(t, h, c)

	var got v1alpha3.Machine
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "test-machine"}, &got))
	assert.Nil(t, got.Status.Configuration)
	assert.Equal(t, v1alpha3.IsolationNSpawn, got.Status.Isolation, "the rolled back node is observed as it runs")
}

func TestRecoverIsolationMigration_StaysQuietWhileInstallationHoldsTheHost(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	writeJournal(t, migrate.StepStartingTarget)

	store := recoveryStore(t)
	rec, err := installstate.NewRecord("test-machine", "x")
	require.NoError(t, err)
	require.NoError(t, store.Save(rec)) // Installing

	c := recoveryClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 200_000_000)
	defer cancel()

	recoverIsolationMigration(ctx, discardLogger(), c, store, "test-machine", recoveryOperator(h))

	assert.Empty(t, h.source.Calls(), "no host work without installation ownership")
	assert.Nil(t, getChange(t, c))
}

func TestNewMigrationBackendKnowsInPlaceModesOnly(t *testing.T) {
	cfg := baseConfig()

	b, root, err := newMigrationBackend(discardLogger(), migrationEndpoint{Mode: isolation.ModeNSpawn, Slot: "kube2", Config: cfg})
	require.NoError(t, err)
	assert.Equal(t, isolation.ModeNSpawn, b.Mode())
	assert.Equal(t, "kube2", root.Slot())

	b, root, err = newMigrationBackend(discardLogger(), migrationEndpoint{Mode: isolation.ModeContainer, Slot: "kube2", Config: cfg})
	require.NoError(t, err)
	assert.Equal(t, isolation.ModeContainer, b.Mode())
	assert.Equal(t, "kube2", root.Slot())

	_, _, err = newMigrationBackend(discardLogger(), migrationEndpoint{Mode: isolation.ModeNone, Slot: "kube1", Config: cfg})
	require.ErrorContains(t, err, "not supported")

	_, _, err = newMigrationBackend(discardLogger(), migrationEndpoint{Mode: isolation.ModeContainer, Slot: "bogus", Config: cfg})
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// A reprovision whose bootstrap never succeeded
// ---------------------------------------------------------------------------

func stageGivenUpHandoff(t *testing.T, h *migHarness) {
	t.Helper()

	dir := reprovisionDir()
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ReprovisionBinaryName), []byte("binary"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ReprovisionConfigName), []byte(`{"token":"secret"}`), 0o600))
	require.NoError(t, os.MkdirAll(reprovisionUnitDir(), 0o755))
	require.NoError(t, os.WriteFile(reprovisionUnitPath(), []byte("[Unit]\n"), 0o644))
	require.NoError(t, reprovisionJournalFor().Save(migrate.Journal{
		Strategy: migrate.Reprovision, From: isolation.ModeNSpawn, To: isolation.ModeNone,
		Step: migrate.StepFailed, Attempts: ReprovisionMaxAttempts,
		Message: "reprovision to isolation mode none gave up after 5 attempts; the host was reset and is not bootstrapped: kubelet did not join",
	}))
}

func TestAbandonReprovisionKeepsOnlyTheJournal(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	stageGivenUpHandoff(t, h)

	require.NoError(t, AbandonReprovision(context.Background(), discardLogger(), reprovisionDir()))

	assert.NoFileExists(t, reprovisionUnitPath())
	assert.NoFileExists(t, filepath.Join(reprovisionDir(), ReprovisionBinaryName))
	assert.NoFileExists(t, filepath.Join(reprovisionDir(), ReprovisionConfigName), "the staged configuration holds the bootstrap token")
	assert.FileExists(t, reprovisionJournalOf(), "the journal is what the next daemon reports")
	assert.Equal(t, []string{"systemctl disable " + ReprovisionUnit, "systemctl daemon-reload"}, h.events)

	// Running it again, as an interrupted handoff does, is harmless.
	require.NoError(t, AbandonReprovision(context.Background(), discardLogger(), reprovisionDir()))
}

func TestRecoverIsolationMigration_ReportsAReprovisionThatGaveUp(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	stageGivenUpHandoff(t, h)
	require.NoError(t, AbandonReprovision(context.Background(), discardLogger(), reprovisionDir()))

	c := recoveryClient(t)

	// The first daemon to start on the reset host, after a new bootstrap.
	recoverChanges(t, h, c)

	change := getChange(t, c)
	require.NotNil(t, change)
	assert.Equal(t, v1alpha3.IsolationChangeFailed, change.Phase)
	assert.Equal(t, v1alpha3.IsolationNone, change.TargetMode)
	assert.Contains(t, change.Message, "gave up after 5 attempts")
	assert.Contains(t, change.Message, "kubelet did not join")

	_, err := reprovisionJournalFor().Load()
	assert.ErrorIs(t, err, migrate.ErrNoJournal, "the journal goes once the failure is reported")
	assert.NoDirExists(t, reprovisionDir())
}

func TestRemoveReprovisionHandoffLeavesNothingBehind(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)

	// Nothing staged: a reset on an ordinary host does nothing.
	require.NoError(t, RemoveReprovisionHandoff(discardLogger()).Do(context.Background()))
	assert.Empty(t, h.events)

	stageGivenUpHandoff(t, h)

	require.NoError(t, RemoveReprovisionHandoff(discardLogger()).Do(context.Background()))

	assert.NoFileExists(t, reprovisionUnitPath())
	assert.NoDirExists(t, reprovisionDir())
	assert.Equal(t, []string{
		"systemctl stop --no-block " + ReprovisionUnit,
		"systemctl disable " + ReprovisionUnit,
		"systemctl daemon-reload",
	}, h.events)
}

func TestResetIncludesTheHandoffCleanupExceptForTheHandoffsOwnReset(t *testing.T) {
	log := discardLogger()

	assert.Contains(t, resetResources(log).Name(), "remove-reprovision-handoff")
	assert.NotContains(t, resetNodeResources(log).Name(), "remove-reprovision-handoff")
}

// Two outcomes are pending: reporting the first must not clear the second's
// journal. If the second report then fails, the journal is all that is left of
// its outcome.
func TestRecoverIsolationMigration_ClearsOnlyTheJournalItReported(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)

	writeJournal(t, migrate.StepRolledBack, func(j *migrate.Journal) { j.Message = "readiness timed out" })
	require.NoError(t, reprovisionJournalFor().Save(migrate.Journal{
		Strategy: migrate.Reprovision, From: isolation.ModeNSpawn, To: isolation.ModeNone,
		Step: migrate.StepBootstrapped,
	}))

	c := fake.NewClientBuilder().WithScheme(fakeScheme()).
		WithStatusSubresource(&v1alpha3.Machine{}).
		WithObjects(&v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 4}}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if m, ok := obj.(*v1alpha3.Machine); ok && m.Status.IsolationChange != nil && m.Status.IsolationChange.TargetMode == v1alpha3.IsolationNone {
					return errors.New("API server unavailable")
				}

				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).Build()

	recoverChanges(t, h, c)

	change := getChange(t, c)
	require.NotNil(t, change)
	assert.Equal(t, v1alpha3.IsolationContainer, change.TargetMode, "the first outcome was reported")

	_, err := inPlaceJournalStore().Load()
	assert.ErrorIs(t, err, migrate.ErrNoJournal, "and its journal is gone")

	_, err = reprovisionJournalFor().Load()
	assert.NoError(t, err, "the second report failed, so its journal stays for the next start")
}

// A reprovision that is still running is not "resolved": it stays InProgress
// and the reconciler starts nothing over it.
func TestSettleIsolationChanges_ARunningReprovisionStaysInProgress(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)

	require.NoError(t, os.MkdirAll(reprovisionDir(), 0o700))
	require.NoError(t, reprovisionJournalFor().Save(migrate.Journal{
		Strategy: migrate.Reprovision, From: isolation.ModeNSpawn, To: isolation.ModeNone,
		Step: migrate.StepBootstrapping,
	}))

	c := recoveryClient(t)

	assert.True(t, settleIsolationChanges(context.Background(), discardLogger(), c, "test-machine", recoveryOperator(h)))

	change := getChange(t, c)
	require.NotNil(t, change)
	assert.Equal(t, v1alpha3.IsolationChangeInProgress, change.Phase)
	assert.NotContains(t, change.Message, "resolved")

	_, err := reprovisionJournalFor().Load()
	assert.NoError(t, err, "the journal stays")
}

// A journal that cannot be read means a change may be under way, so nothing
// new starts over it.
func TestSettleIsolationChanges_AnUnreadableJournalBlocksNewChanges(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	require.NoError(t, os.WriteFile(inPlaceJournalPath(), []byte("{broken"), 0o600))

	c := recoveryClient(t)

	assert.True(t, settleIsolationChanges(context.Background(), discardLogger(), c, "test-machine", recoveryOperator(h)))
	assert.Nil(t, getChange(t, c), "there is nothing to report without the journal")
	assert.Empty(t, h.source.Calls())

	_, err := os.Stat(inPlaceJournalPath())
	assert.NoError(t, err, "the journal is left for an operator")
}

func TestSettleIsolationChanges_NothingPendingSettlesNothing(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)

	assert.False(t, settleIsolationChanges(context.Background(), discardLogger(), recoveryClient(t), "test-machine", recoveryOperator(h)))
}
