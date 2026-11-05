// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/internal/provision"
	agentdaemon "github.com/Azure/unbounded/pkg/agent/daemon"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
)

func recoveryClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()

	return fakeStatusClient(append([]client.Object{&v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 4}}}, objs...)...)
}

// failingStatusClient is a client whose every status patch fails. With no
// objects it holds the test Machine.
func failingStatusClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()

	all := objs
	if len(all) == 0 {
		all = []client.Object{&v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine"}}}
	}

	return fake.NewClientBuilder().WithScheme(fakeScheme()).WithObjects(all...).
		WithStatusSubresource(&v1alpha3.Machine{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
				return errors.New("apiserver unavailable")
			},
		}).Build()
}

// recoverChanges is what a daemon start does with the journals: resolve them
// on the host, then report.
func recoverChanges(t *testing.T, h *migHarness, c client.Client) settleOutcome {
	t.Helper()

	return h.env.reportSettled(context.Background(), discardLogger(), c, "test-machine", h.env.resolveJournals(context.Background(), discardLogger()))
}

func getMachine(t *testing.T, c client.Client) v1alpha3.Machine {
	t.Helper()

	var m v1alpha3.Machine
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "test-machine"}, &m))

	return m
}

func getChange(t *testing.T, c client.Client) *v1alpha3.IsolationChangeStatus {
	t.Helper()

	return getMachine(t, c).Status.IsolationChange
}

// stageReprovision writes a reprovision journal at step, with the staged files
// a handoff leaves.
func stageReprovision(t *testing.T, h *migHarness, step migrate.Step, to isolation.Mode) {
	t.Helper()

	require.NoError(t, h.env.reprovisionJournal().Save(migrate.Journal{
		Strategy: migrate.Reprovision, From: activeModeOf(h.active), To: to, Step: step,
	}))

	for _, name := range []string{ReprovisionBinaryName, ReprovisionConfigName} {
		require.NoError(t, os.WriteFile(filepath.Join(h.env.reprovisionDir(), name), []byte("staged"), 0o600))
	}
}

func applyMode(h *migHarness, mode isolation.Mode) {
	cfg := h.active.Config.DeepCopy()
	cfg.Isolation = string(mode)
	h.applied = map[string]*provision.AgentConfig{"kube1": cfg}
}

func TestRecover_NothingToDo(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	c := recoveryClient(t)

	assert.Equal(t, settleOutcome{}, recoverChanges(t, h, c))
	assert.Nil(t, getChange(t, c))
}

func TestRecover_ReportsAnInterruptedInPlaceChangeAndClearsItsJournal(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	writeJournal(t, h, migrate.StepStartingTarget)

	c := recoveryClient(t)
	res := recoverChanges(t, h, c)

	assert.True(t, res.resolved)
	assert.Equal(t, isolation.ModeNSpawn, res.failedTarget)
	assert.Contains(t, h.source.Calls(), "start kube1", "rolled back to the source")

	change := getChange(t, c)
	require.NotNil(t, change)
	assert.Equal(t, v1alpha3.IsolationChangeFailed, change.Phase)
	assert.Contains(t, change.Message, "interrupted")
	assert.Equal(t, v1alpha3.IsolationNSpawn, getMachine(t, c).Status.Isolation, "a rollback reports the source mode")

	_, err := h.journal()
	assert.ErrorIs(t, err, migrate.ErrNoJournal)
}

func TestRecover_AReprovisionIsReportedByTheNewDaemonWithTheAppliedMode(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	stageReprovision(t, h, migrate.StepBootstrapping, isolation.ModeNone)
	applyMode(h, isolation.ModeNone)

	c := recoveryClient(t)
	recoverChanges(t, h, c)

	m := getMachine(t, c)
	require.NotNil(t, m.Status.IsolationChange)
	assert.Equal(t, v1alpha3.IsolationChangeSucceeded, m.Status.IsolationChange.Phase)
	assert.Equal(t, v1alpha3.IsolationChangeReprovision, m.Status.IsolationChange.Strategy)
	assert.Equal(t, v1alpha3.IsolationNone, m.Status.Isolation, "Succeeded and the applied mode land together")
	assert.NoDirExists(t, h.env.reprovisionDir())
}

func TestRecover_AReprovisionThatGaveUpIsReported(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	require.NoError(t, h.env.reprovisionJournal().Save(migrate.Journal{
		Strategy: migrate.Reprovision, From: isolation.ModeNSpawn, To: isolation.ModeNone,
		Step: migrate.StepFailed, Message: "reprovision to isolation mode none gave up",
	}))

	c := recoveryClient(t)
	recoverChanges(t, h, c)

	change := getChange(t, c)
	require.NotNil(t, change)
	assert.Equal(t, v1alpha3.IsolationChangeFailed, change.Phase)
	assert.Contains(t, change.Message, "gave up")
	assert.NoDirExists(t, h.env.reprovisionDir())
}

func TestRecover_ARunningReprovisionStaysInProgress(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	stageReprovision(t, h, migrate.StepReprovisioning, isolation.ModeNone)
	h.installUnit()

	c := recoveryClient(t)
	res := recoverChanges(t, h, c)

	assert.True(t, res.handedOff)
	assert.Equal(t, v1alpha3.IsolationChangeInProgress, getChange(t, c).Phase)
	assert.FileExists(t, filepath.Join(h.env.reprovisionDir(), ReprovisionJournalName), "the handoff is still in charge")
}

// A daemon that stopped after writing the reprovision journal and before
// installing the unit reset nothing. The change is over, as a failure, rather
// than reported as handed off forever.
func TestRecover_AReprovisionWhoseUnitWasNeverInstalledNeverStarted(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	stageReprovision(t, h, migrate.StepReprovisioning, isolation.ModeNone)

	c := recoveryClient(t)
	res := recoverChanges(t, h, c)

	assert.False(t, res.handedOff)
	assert.True(t, res.resolved)

	change := getChange(t, c)
	require.NotNil(t, change)
	assert.Equal(t, v1alpha3.IsolationChangeFailed, change.Phase)
	assert.Contains(t, change.Message, "never started")
	assert.Contains(t, change.Message, "nothing was reset")
	assert.NoDirExists(t, h.env.reprovisionDir(), "the staged token and binary are gone")
}

// The unit must also be enabled: a unit file systemd does not run carries
// nothing on.
func TestRecover_AReprovisionUnitThatIsNotEnabledDoesNotCarryTheHandoff(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	stageReprovision(t, h, migrate.StepBootstrapping, isolation.ModeNone)
	h.installUnit()
	h.failAt["systemctl"] = errors.New("disabled")

	c := recoveryClient(t)
	res := recoverChanges(t, h, c)

	assert.False(t, res.handedOff)
	change := getChange(t, c)
	require.NotNil(t, change)
	assert.Equal(t, v1alpha3.IsolationChangeFailed, change.Phase)
	assert.Contains(t, change.Message, "needs attention")
}

// A journal that is still not over after it was resolved is kept, whatever
// the report said, and blocks new changes.
func TestRecover_AJournalThatIsNotOverIsKept(t *testing.T) {
	t.Run("the source cleanup failed", func(t *testing.T) {
		h := newMigHarness(t, isolation.ModeNSpawn)
		writeJournal(t, h, migrate.StepReady)
		appliedTarget := h.active.Config.DeepCopy()
		h.applied["kube2"] = appliedTarget
		h.source.DestroyErr = errors.New("busy")

		c := recoveryClient(t)
		res := recoverChanges(t, h, c)

		assert.True(t, res.blocked)
		assert.False(t, res.resolved)

		j, err := h.journal()
		require.NoError(t, err, "kept so the next start finishes the cleanup")
		assert.Equal(t, migrate.StepReady, j.Step)
	})

	t.Run("the source config could not be read", func(t *testing.T) {
		h := newMigHarness(t, isolation.ModeNSpawn)
		writeJournal(t, h, migrate.StepStoppingSource)
		h.failAt["read kube1"] = errors.New("input/output error")

		c := recoveryClient(t)
		res := recoverChanges(t, h, c)

		assert.True(t, res.blocked)
		assert.Empty(t, h.source.Calls(), "nothing was decided")

		change := getChange(t, c)
		require.NotNil(t, change)
		assert.Equal(t, v1alpha3.IsolationChangeFailed, change.Phase)
		assert.Contains(t, change.Message, "input/output error")

		j, err := h.journal()
		require.NoError(t, err)
		assert.Equal(t, migrate.StepStoppingSource, j.Step)
	})
}

func TestSettleIsolationChanges_AnUnreadableJournalBlocksNewChanges(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	path := inPlaceJournalPathIn(h.env.ConfigDir)
	require.NoError(t, os.WriteFile(path, []byte("{"), 0o600))

	c := recoveryClient(t)
	res := h.env.settleIsolationChanges(context.Background(), discardLogger(), c, "test-machine")

	assert.True(t, res.blocked)

	change := getChange(t, c)
	require.NotNil(t, change, "the operator is told why nothing new starts")
	assert.Equal(t, v1alpha3.IsolationChangeFailed, change.Phase)
	assert.Equal(t, unknownTargetMode, change.TargetMode)
	assert.Contains(t, change.Message, path)
	assert.FileExists(t, path, "an unreadable journal is never removed")
}

func TestSettleIsolationChanges_AFailedReportKeepsTheTerminalJournal(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	writeJournal(t, h, migrate.StepRolledBack, func(j *migrate.Journal) { j.Message = "boom" })

	res := h.env.settleIsolationChanges(context.Background(), discardLogger(), failingStatusClient(t), "test-machine")
	assert.True(t, res.resolved)

	_, err := h.journal()
	require.NoError(t, err, "reported again at the next start")
}

func TestReportStartupIsolationChanges_SkipsWithNothingSettled(t *testing.T) {
	assert.Equal(t, settleOutcome{}, reportStartupIsolationChanges(context.Background(), discardLogger(), nil, &runOptions{}, "m", nil))
}

// A daemon whose credentials or client cannot be built still resolves an
// interrupted in-place change on the host before it gives up: the node is never
// left half changed because the API server cannot be reached.
func TestRun_ResolvesAnInterruptedChangeBeforeItNeedsTheCluster(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	writeJournal(t, h, migrate.StepStartingTarget)

	op := &fakeNodeOperator{active: h.active}
	err := run(context.Background(), discardLogger(), runOptions{
		NodeOperator: op,
		installation: installstate.NewStore(t.TempDir(), filepath.Join(t.TempDir(), "lock")),
		Migration:    h.env,
		NewClient: func(*rest.Config, client.Options) (client.WithWatch, error) {
			return nil, errors.New("no route to the API server")
		},
	})
	require.Error(t, err)

	assert.Contains(t, h.source.Calls(), "start kube1", "rolled back without the cluster")

	j, jerr := h.journal()
	require.NoError(t, jerr, "kept until it is reported")
	assert.Equal(t, migrate.StepRolledBack, j.Step)
}

// fakeMigrator stands in for the engine in reconcile tests.
type fakeMigrator struct {
	strategies []migrate.Strategy
	err        error
}

func (m *fakeMigrator) MigrateIsolation(_ context.Context, _ *slog.Logger, _ *ActiveMachine, _ *provision.UnboundedAgentConfig, s migrate.Strategy) error {
	m.strategies = append(m.strategies, s)

	return m.err
}

// changeFixture is a repave reconciler over a harness, for a Machine that
// declares spec and may allow a reprovision.
func changeFixture(t *testing.T, h *migHarness, spec v1alpha3.IsolationMode, version string, allow bool, c func(objs ...client.Object) client.Client) (*repaveReconciler, *fakeNodeOperator, *fakeMigrator, client.Client) {
	t.Helper()

	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "test-machine"},
		Spec: v1alpha3.MachineSpec{
			Isolation:        spec,
			ConfigurationRef: &v1alpha3.MachineConfigurationRef{Name: "config-a", Version: ptr.To(int32(2))},
		},
	}
	if allow {
		machine.Annotations = map[string]string{v1alpha3.AnnotationAllowReprovision: "true"}
	}

	mcv := machineConfigurationVersion("config-a", 2, v1alpha3.MachineConfigurationTemplate{
		Kubernetes: &v1alpha3.MachineConfigurationKubernetes{Version: version},
		Agent:      &v1alpha3.MachineConfigurationAgent{Image: h.active.Config.OCIImage},
	})

	cl := c(machine, mcv)
	op := &fakeNodeOperator{active: h.active}
	mig := &fakeMigrator{}

	return &repaveReconciler{
		installation:      installstate.NewStore(filepath.Join(t.TempDir(), "state"), filepath.Join(t.TempDir(), "install.lock")),
		Client:            cl,
		log:               discardLogger(),
		machineName:       "test-machine",
		nodeName:          "test-node",
		nodeOperator:      op,
		env:               h.env,
		isolationMigrator: mig,
	}, op, mig, cl
}

func healthyClient(t *testing.T) func(objs ...client.Object) client.Client {
	t.Helper()

	return func(objs ...client.Object) client.Client { return fakeStatusClient(objs...) }
}

func TestReconcileRepave_AnAllowedReprovisionIsMade(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	r, op, mig, c := changeFixture(t, h, v1alpha3.IsolationNone, h.active.Config.Cluster.Version, true, healthyClient(t))

	res, err := r.ReconcileRepave(t.Context(), "node-delete")
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)
	assert.Equal(t, []migrate.Strategy{migrate.Reprovision}, mig.strategies)
	assert.Nil(t, op.repaveConfig, "a change is not also a same-mode repave")

	m := getMachine(t, c)
	require.NotNil(t, m.Status.IsolationChange)
	assert.Equal(t, v1alpha3.IsolationChangeSucceeded, m.Status.IsolationChange.Phase)
	assert.Equal(t, v1alpha3.IsolationNone, m.Status.Isolation, "Succeeded sets the applied mode in the same patch")
}

func TestReconcileRepave_AReprovisionThatIsNotAllowedIsRefused(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	r, op, mig, c := changeFixture(t, h, v1alpha3.IsolationNone, "1.34.9", false, healthyClient(t))

	_, err := r.ReconcileRepave(t.Context(), "node-delete")
	require.NoError(t, err)

	assert.Empty(t, mig.strategies, "nothing is reset without the opt-in")
	require.NotNil(t, op.repaveConfig, "the version rollout goes ahead in the current mode")
	assert.Equal(t, h.active.Config.Isolation, op.repaveConfig.Isolation)

	change := getChange(t, c)
	require.NotNil(t, change)
	assert.Equal(t, v1alpha3.IsolationChangeFailed, change.Phase)
	assert.Equal(t, v1alpha3.IsolationChangeReasonReprovisionNotAllowed, change.Reason)
	assert.Equal(t, v1alpha3.IsolationChangeReprovision, change.Strategy)
	assert.Contains(t, change.Message, v1alpha3.AnnotationAllowReprovision)
}

// A handed off reprovision leaves the daemon about to be removed. Nothing new
// starts: no repave, and no MachineOperation.
func TestReconcileRepave_AHandoffStopsNewWork(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	r, op, mig, _ := changeFixture(t, h, v1alpha3.IsolationNone, h.active.Config.Cluster.Version, true, healthyClient(t))
	mig.err = ErrMigrationHandedOff
	r.handoff = &handoffState{}

	_, err := r.ReconcileRepave(t.Context(), "node-delete")
	require.NoError(t, err)
	assert.True(t, r.handoff.active())

	_, err = r.ReconcileRepave(t.Context(), "node-delete")
	require.NoError(t, err)
	assert.Len(t, mig.strategies, 1, "the second reconcile does nothing")
	assert.Nil(t, op.repaveConfig)

	target := &machineOperationTarget{log: discardLogger(), handoff: r.handoff}
	called := false
	handler := target.unlessStopping(func(context.Context, agentdaemon.MachineOperationStore[int64], agentdaemon.MachineOperation) (ctrl.Result, error) {
		called = true
		return ctrl.Result{}, nil
	})

	_, err = handler(t.Context(), nil, agentdaemon.MachineOperation{Name: "op"})
	require.NoError(t, err)
	assert.False(t, called, "MachineOperations wait for the daemon the handoff installs")
}

func TestReconcileRepave_AFailedChangeRestoresTheNodeRegistrationEvenWhenTheReportFails(t *testing.T) {
	for name, newClient := range map[string]func(objs ...client.Object) client.Client{
		"report lands": healthyClient(t),
		"report fails": func(objs ...client.Object) client.Client { return failingStatusClient(t, objs...) },
	} {
		t.Run(name, func(t *testing.T) {
			h := newMigHarness(t, isolation.ModeNSpawn)
			r, op, mig, _ := changeFixture(t, h, v1alpha3.IsolationNone, h.active.Config.Cluster.Version, true, newClient)
			mig.err = errors.New("preflight failed (the nspawn node was not changed)")

			res, err := r.ReconcileRepave(t.Context(), "node-delete")
			require.NoError(t, err)
			assert.Equal(t, ctrl.Result{}, res, "a failed change is not retried")
			assert.True(t, op.restartCalled, "the deleted Node must register again")
		})
	}
}

func TestReconcileRepave_AFailedRollbackDoesNotRestartTheSource(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	r, op, mig, c := changeFixture(t, h, v1alpha3.IsolationNone, h.active.Config.Cluster.Version, true, healthyClient(t))
	mig.err = fmt.Errorf("start failed: %w; the node needs attention", migrate.ErrRollbackFailed)

	_, err := r.ReconcileRepave(t.Context(), "node-delete")
	require.NoError(t, err)
	assert.False(t, op.restartCalled, "the target may still run")

	m := getMachine(t, c)
	assert.Equal(t, v1alpha3.IsolationChangeFailed, m.Status.IsolationChange.Phase)
	assert.Empty(t, m.Status.Isolation, "the applied mode is not known")
}

// A journal left from an earlier change is settled and reported before
// anything else, and the reconcile is requeued rather than consumed: the
// Node deletion still asks for a repave. The repave that follows goes ahead in
// the current mode without trying the failed change again at once; the next
// Node deletion tries it.
func TestReconcileRepave_APendingJournalIsResolvedBeforeAnythingElse(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	r, op, mig, c := changeFixture(t, h, v1alpha3.IsolationNone, "1.34.9", true, healthyClient(t))
	require.NoError(t, h.env.reprovisionJournal().Save(migrate.Journal{
		Strategy: migrate.Reprovision, From: isolation.ModeNSpawn, To: isolation.ModeNone,
		Step: migrate.StepFailed, Message: "reprovision to isolation mode none gave up",
	}))

	res, err := r.ReconcileRepave(t.Context(), "node-delete")
	require.NoError(t, err)
	assert.Positive(t, res.RequeueAfter, "the repave is requeued, not consumed")
	assert.Nil(t, op.repaveConfig)
	assert.Empty(t, mig.strategies)
	assert.Equal(t, v1alpha3.IsolationChangeFailed, getChange(t, c).Phase)
	assert.NoDirExists(t, h.env.reprovisionDir(), "reported, so cleared")

	_, err = r.ReconcileRepave(t.Context(), "node-delete")
	require.NoError(t, err)
	require.NotNil(t, op.repaveConfig, "the requeued reconcile repaves")
	assert.Equal(t, "1.34.9", op.repaveConfig.Cluster.Version)
	assert.Empty(t, mig.strategies, "the change that just failed is not tried again at once")

	op.repaveConfig = nil

	_, err = r.ReconcileRepave(t.Context(), "node-delete")
	require.NoError(t, err)
	assert.Equal(t, []migrate.Strategy{migrate.Reprovision}, mig.strategies, "the next repave tries the change again")
}

// A terminal journal and a deleted Node: the second reconcile repaves.
func TestReconcileRepave_ATerminalJournalDoesNotConsumeTheRepave(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	r, op, _, _ := changeFixture(t, h, "", "1.34.9", false, healthyClient(t))
	writeJournal(t, h, migrate.StepComplete, func(j *migrate.Journal) { j.From, j.To = isolation.ModeNSpawn, isolation.ModeNSpawn })

	res, err := r.ReconcileRepave(t.Context(), "node-delete")
	require.NoError(t, err)
	assert.Positive(t, res.RequeueAfter)
	assert.Nil(t, op.repaveConfig)

	_, err = r.ReconcileRepave(t.Context(), "node-delete")
	require.NoError(t, err)
	require.NotNil(t, op.repaveConfig)
}

func TestReconcileRepave_AnUnreadableJournalBlocksTheRepave(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	r, op, mig, _ := changeFixture(t, h, v1alpha3.IsolationNone, "1.34.9", true, healthyClient(t))
	require.NoError(t, os.WriteFile(inPlaceJournalPathIn(h.env.ConfigDir), []byte("{"), 0o600))

	res, err := r.ReconcileRepave(t.Context(), "node-delete")
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)
	assert.Nil(t, op.repaveConfig)
	assert.Empty(t, mig.strategies)
}

func TestIsolationStatusFromJournal(t *testing.T) {
	j := migrate.Journal{Strategy: migrate.Reprovision, From: isolation.ModeNSpawn, To: isolation.ModeNone}

	ok := isolationStatusFromJournal(j, nil)
	assert.Equal(t, v1alpha3.IsolationChangeSucceeded, ok.Change.Phase)
	assert.Equal(t, v1alpha3.IsolationNone, ok.Applied)

	running := isolationStatusFromJournal(j, ErrMigrationHandedOff)
	assert.Equal(t, v1alpha3.IsolationChangeInProgress, running.Change.Phase)
	assert.Empty(t, running.Applied)

	j.Step = migrate.StepRolledBack
	back := isolationStatusFromJournal(j, errors.New("boom"))
	assert.Equal(t, v1alpha3.IsolationChangeFailed, back.Change.Phase)
	assert.Equal(t, v1alpha3.IsolationNSpawn, back.Applied)

	j.Step = migrate.StepFailed
	failed := isolationStatusFromJournal(j, errors.New("boom"))
	assert.Empty(t, failed.Applied, "after a failed rollback the mode is not known")
}

func TestAbandonReprovisionKeepsOnlyTheJournal(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	stageReprovision(t, h, migrate.StepFailed, isolation.ModeNone)
	h.installUnit()

	require.NoError(t, h.env.abandonReprovision(context.Background(), discardLogger()))

	assert.FileExists(t, filepath.Join(h.env.reprovisionDir(), ReprovisionJournalName))
	assert.NoFileExists(t, filepath.Join(h.env.reprovisionDir(), ReprovisionConfigName))
	assert.NoFileExists(t, filepath.Join(h.env.reprovisionDir(), ReprovisionBinaryName))
	assert.NoFileExists(t, h.env.reprovisionUnitPath())
}

func TestRemoveReprovisionHandoffLeavesNothingBehind(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	stageReprovision(t, h, migrate.StepBootstrapping, isolation.ModeNone)
	h.installUnit()

	require.NoError(t, h.env.removeReprovisionHandoff(discardLogger()).Do(context.Background()))

	assert.NoDirExists(t, h.env.reprovisionDir())
	assert.NoFileExists(t, h.env.reprovisionUnitPath())
	assert.Contains(t, h.events, "systemctl stop --no-block "+ReprovisionUnit)
}

func TestResetIncludesTheHandoffCleanupExceptForTheHandoffsOwnReset(t *testing.T) {
	assert.Contains(t, resetResources(discardLogger()).Name(), "remove-reprovision-handoff")
	assert.NotContains(t, ResetAgentForReprovision(discardLogger()).Name(), "remove-reprovision-handoff")
	assert.Contains(t, ResetAgent(discardLogger()).Name(), "remove-reprovision-handoff")
}
