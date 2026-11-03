// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
)

// changeHarness runs ReconcileRepave against a Machine that declares a mode.
type changeHarness struct {
	t          *testing.T
	op         *fakeNodeOperator
	c          client.Client
	reconciler *repaveReconciler
}

// newChangeHarness builds a node running in applied whose Machine declares spec
// (empty for no declaration). When version is set, a machine configuration with
// that Kubernetes version is attached so the same repave carries a version bump.
func newChangeHarness(t *testing.T, applied isolation.Mode, spec v1alpha3.IsolationMode, version string) *changeHarness {
	t.Helper()

	// A failed change waits for the Node to come back before it restarts the
	// stack; tests do not wait.
	nodeReturnWait, nodeReturnPoll = 0, 0

	t.Cleanup(func() { nodeReturnWait, nodeReturnPoll = 45*time.Second, 5*time.Second })

	h := newMigHarness(t, applied)

	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 5},
		Spec:       v1alpha3.MachineSpec{Isolation: spec},
	}
	objs := []client.Object{machine}

	if version != "" {
		machine.Spec.ConfigurationRef = &v1alpha3.MachineConfigurationRef{Name: "config-a", Version: ptr.To(int32(2))}
		objs = append(objs, machineConfigurationVersion("config-a", 2, v1alpha3.MachineConfigurationTemplate{
			Kubernetes: &v1alpha3.MachineConfigurationKubernetes{Version: version},
			Agent:      &v1alpha3.MachineConfigurationAgent{Image: h.active.Config.OCIImage},
		}))
	}

	op := &fakeNodeOperator{active: h.active}
	c := fakeStatusClient(objs...)

	return &changeHarness{
		t: t, op: op, c: c,
		reconciler: &repaveReconciler{
			installation: installstate.NewStore(filepath.Join(t.TempDir(), "state"), filepath.Join(t.TempDir(), "install.lock")),
			Client:       c,
			log:          discardLogger(),
			machineName:  "test-machine",
			nodeOperator: op,
		},
	}
}

func (h *changeHarness) reconcile() {
	h.t.Helper()

	result, err := h.reconciler.ReconcileRepave(context.Background(), "node-delete")
	require.NoError(h.t, err)
	assert.Zero(h.t, result.RequeueAfter, "a mode change is never requeued")
}

func (h *changeHarness) machine() v1alpha3.Machine {
	h.t.Helper()

	var m v1alpha3.Machine
	require.NoError(h.t, h.c.Get(context.Background(), client.ObjectKey{Name: "test-machine"}, &m))

	return m
}

func TestReconcileRepave_ChangesToTheDeclaredModeInPlace(t *testing.T) {
	h := newChangeHarness(t, isolation.ModeNSpawn, v1alpha3.IsolationContainer, "v1.34.1")

	h.reconcile()

	require.Equal(t, 1, h.op.migrateCalls)
	assert.Equal(t, migrate.InPlace, h.op.migrateStrategy, "nspawn to container is in place")
	require.NotNil(t, h.op.migrateDesired)
	assert.Equal(t, string(isolation.ModeContainer), h.op.migrateDesired.Isolation)
	assert.Equal(t, "1.34.1", h.op.migrateDesired.Cluster.Version, "the same repave applies the new version")
	assert.Nil(t, h.op.repaveConfig, "a mode change replaces the same-mode repave")

	m := h.machine()
	require.NotNil(t, m.Status.IsolationChange)
	assert.Equal(t, v1alpha3.IsolationChangeSucceeded, m.Status.IsolationChange.Phase)
	assert.Equal(t, v1alpha3.IsolationContainer, m.Status.IsolationChange.TargetMode)
	assert.Equal(t, v1alpha3.IsolationChangeInPlace, m.Status.IsolationChange.Strategy)
	assert.False(t, m.Status.IsolationChange.LastTransitionTime.IsZero())
	assert.Equal(t, v1alpha3.IsolationContainer, m.Status.Isolation, "the new mode is observed straight away")
	assert.Equal(t, v1alpha3.IsolationContainer, m.Spec.Isolation, "the agent leaves the declaration as it was")
	require.NotNil(t, m.Status.Configuration, "the desired configuration version is marked applied")
	assert.Equal(t, int32(2), m.Status.Configuration.Version)
}

func TestReconcileRepave_AMoveInvolvingNoneIsAReprovision(t *testing.T) {
	for _, tt := range []struct {
		name    string
		applied isolation.Mode
		spec    v1alpha3.IsolationMode
	}{
		{"to none", isolation.ModeNSpawn, v1alpha3.IsolationNone},
		{"from none", isolation.ModeNone, v1alpha3.IsolationNSpawn},
		{"container to none", isolation.ModeContainer, v1alpha3.IsolationNone},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newChangeHarness(t, tt.applied, tt.spec, "")
			h.op.migrateErr = ErrMigrationHandedOff

			h.reconcile()

			require.Equal(t, 1, h.op.migrateCalls)
			assert.Equal(t, migrate.Reprovision, h.op.migrateStrategy)

			// The host resets without this daemon, so the change is still in
			// progress and nothing is marked applied or observed yet.
			m := h.machine()
			require.NotNil(t, m.Status.IsolationChange)
			assert.Equal(t, v1alpha3.IsolationChangeInProgress, m.Status.IsolationChange.Phase)
			assert.Equal(t, v1alpha3.IsolationChangeReprovision, m.Status.IsolationChange.Strategy)
			assert.Empty(t, m.Status.Isolation)
		})
	}
}

func TestReconcileRepave_AFailedChangeIsRecordedAndNotRetried(t *testing.T) {
	h := newChangeHarness(t, isolation.ModeNSpawn, v1alpha3.IsolationContainer, "")
	h.op.migrateErr = errors.New("start container: kubelet not active (rolled back to nspawn)")

	h.reconcile() // no error and no requeue: the node is not put through another outage

	m := h.machine()
	require.NotNil(t, m.Status.IsolationChange)
	assert.Equal(t, v1alpha3.IsolationChangeFailed, m.Status.IsolationChange.Phase)
	assert.Contains(t, m.Status.IsolationChange.Message, "rolled back to nspawn")
	assert.Nil(t, m.Status.Configuration, "a failed change does not mark the configuration applied")
	assert.Equal(t, 1, h.op.migrateCalls)

	// The next trigger (the next Node deletion) tries again.
	h.reconcile()
	assert.Equal(t, 2, h.op.migrateCalls)
}

// The operator deleted the Node to trigger this repave. When the change is
// refused before anything is touched, the old stack never restarted, and a
// running kubelet does not create a deleted Node object again, so the agent
// restarts the stack to put the node back in the cluster.
func TestReconcileRepave_AFailedChangeRestoresTheNodeRegistration(t *testing.T) {
	t.Run("the Node is missing", func(t *testing.T) {
		h := newChangeHarness(t, isolation.ModeNSpawn, v1alpha3.IsolationContainer, "")
		h.reconciler.nodeName = "test-node"
		h.op.migrateErr = errors.New("host cannot run isolation mode container: no cgroup v2 (the nspawn node was not changed)")

		h.reconcile()

		assert.True(t, h.op.restartCalled, "the stack restarts so the kubelet registers the Node again")
		require.NotNil(t, h.machine().Status.IsolationChange)
		assert.Equal(t, v1alpha3.IsolationChangeFailed, h.machine().Status.IsolationChange.Phase)
	})

	t.Run("the Node came back by itself", func(t *testing.T) {
		h := newChangeHarness(t, isolation.ModeNSpawn, v1alpha3.IsolationContainer, "")
		h.reconciler.nodeName = "test-node"
		h.op.migrateErr = errors.New("start container: boom (rolled back to nspawn)")
		require.NoError(t, h.c.Create(context.Background(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "test-node"}}))

		h.reconcile()

		assert.False(t, h.op.restartCalled, "a rolled-back stack already restarted; do not restart it again")
	})

	t.Run("a restart that fails is only logged", func(t *testing.T) {
		h := newChangeHarness(t, isolation.ModeNSpawn, v1alpha3.IsolationContainer, "")
		h.reconciler.nodeName = "test-node"
		h.op.migrateErr = errors.New("boom")
		h.op.restartErr = errors.New("systemd unavailable")

		h.reconcile()

		assert.True(t, h.op.restartCalled)
	})

	t.Run("a successful change does not restart anything", func(t *testing.T) {
		h := newChangeHarness(t, isolation.ModeNSpawn, v1alpha3.IsolationContainer, "")
		h.reconciler.nodeName = "test-node"

		h.reconcile()

		assert.False(t, h.op.restartCalled)
	})
}

func TestReconcileRepave_NoDeclarationLeavesTheModeAlone(t *testing.T) {
	for _, applied := range []isolation.Mode{isolation.ModeNSpawn, isolation.ModeNone, isolation.ModeContainer} {
		t.Run(string(applied), func(t *testing.T) {
			h := newChangeHarness(t, applied, "", "")

			h.reconcile()

			assert.Zero(t, h.op.migrateCalls)
			assert.Nil(t, h.op.repaveConfig, "no drift, so no repave either")
			assert.Nil(t, h.machine().Status.IsolationChange)
		})
	}
}

func TestReconcileRepave_TheSameDeclaredModeIsAnOrdinaryRepave(t *testing.T) {
	h := newChangeHarness(t, isolation.ModeContainer, v1alpha3.IsolationContainer, "v1.34.1")

	h.reconcile()

	assert.Zero(t, h.op.migrateCalls)
	require.NotNil(t, h.op.repaveConfig, "the version bump is a normal repave")
	assert.Equal(t, "1.34.1", h.op.repaveConfig.Cluster.Version)
	assert.Equal(t, string(isolation.ModeContainer), h.op.repaveConfig.Isolation)
	assert.Nil(t, h.machine().Status.IsolationChange)
}

// The CRD enum keeps an unknown mode out of spec.isolation, and the status
// cannot name one, so the agent only logs it and leaves the node as it is.
func TestReconcileRepave_AnUnknownDeclaredModeLeavesTheNodeAlone(t *testing.T) {
	h := newChangeHarness(t, isolation.ModeNSpawn, v1alpha3.IsolationMode("microvm"), "")

	h.reconcile()

	assert.Zero(t, h.op.migrateCalls)
	assert.Nil(t, h.op.repaveConfig)
	assert.Nil(t, h.machine().Status.IsolationChange)
}

func TestReconcileRepave_APendingJournalIsResolvedBeforeAnythingElse(t *testing.T) {
	h := newChangeHarness(t, isolation.ModeNSpawn, v1alpha3.IsolationContainer, "")
	writeJournal(t, migrate.StepRolledBack, func(j *migrate.Journal) { j.Message = "readiness timed out" })

	h.reconcile()

	// The earlier attempt is reported, and no new attempt starts in the same
	// reconcile: the node was only just put back.
	assert.Zero(t, h.op.migrateCalls)

	m := h.machine()
	require.NotNil(t, m.Status.IsolationChange)
	assert.Equal(t, v1alpha3.IsolationChangeFailed, m.Status.IsolationChange.Phase)
	assert.Contains(t, m.Status.IsolationChange.Message, "readiness timed out")

	_, err := h.journalOnDisk()
	assert.ErrorIs(t, err, migrate.ErrNoJournal, "the journal goes once its outcome is reported")
}

func (h *changeHarness) journalOnDisk() (migrate.Journal, error) {
	return inPlaceJournalStore().Load()
}

func TestResolveDesiredRepaveConfig_TakesTheDeclaredMode(t *testing.T) {
	for _, tt := range []struct {
		name    string
		applied string
		spec    v1alpha3.IsolationMode
		want    string
	}{
		{"declared mode wins", "", v1alpha3.IsolationContainer, "container"},
		{"declared nspawn over none", "none", v1alpha3.IsolationNSpawn, "nspawn"},
		{"no declaration keeps none", "none", "", "none"},
		{"no declaration keeps a legacy empty config", "", "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine"}, Spec: v1alpha3.MachineSpec{Isolation: tt.spec}}
			applied := baseConfig()
			applied.Isolation = tt.applied

			got, _, err := resolveDesiredRepaveConfig(context.Background(), fakeStatusClient(machine), "test-machine", applied)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got.Isolation)
		})
	}
}

func TestHasDrift_ComparesTheEffectiveMode(t *testing.T) {
	base := func(mode string) *provision.AgentConfig {
		cfg := baseConfig()
		cfg.Isolation = mode

		return cfg
	}

	assert.False(t, hasDrift(base(""), base("nspawn")), "empty is nspawn")
	assert.False(t, hasDrift(base("nspawn"), base("")))
	assert.False(t, hasDrift(base("none"), base("none")))
	assert.True(t, hasDrift(base(""), base("none")))
	assert.True(t, hasDrift(base("nspawn"), base("container")))
	assert.True(t, hasDrift(base("none"), base("container")))
}

func TestIsolationChangeFromJournal(t *testing.T) {
	for _, tt := range []struct {
		name     string
		step     migrate.Step
		outcome  error
		strategy migrate.Strategy
		phase    v1alpha3.IsolationChangePhase
		wantMsg  string
	}{
		{"complete in place", migrate.StepComplete, nil, migrate.InPlace, v1alpha3.IsolationChangeSucceeded, "container"},
		{"bootstrapped reprovision", migrate.StepBootstrapped, nil, migrate.Reprovision, v1alpha3.IsolationChangeSucceeded, "container"},
		{"failed", migrate.StepFailed, errors.New("boom"), migrate.InPlace, v1alpha3.IsolationChangeFailed, "boom"},
		{"handed off", migrate.StepReprovisioning, ErrMigrationHandedOff, migrate.Reprovision, v1alpha3.IsolationChangeInProgress, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			j := migrate.Journal{Strategy: tt.strategy, From: isolation.ModeNSpawn, To: isolation.ModeContainer, Step: tt.step}

			got := isolationChangeFromJournal(j, tt.outcome)

			assert.Equal(t, tt.phase, got.Phase)
			assert.Equal(t, v1alpha3.IsolationContainer, got.TargetMode)
			assert.Contains(t, got.Message, tt.wantMsg)
			assert.Equal(t, isolationChangeStrategy(tt.strategy), got.Strategy)
		})
	}

	assert.Equal(t, v1alpha3.IsolationChangeInPlace, isolationChangeStrategy(migrate.InPlace))
	assert.Equal(t, v1alpha3.IsolationChangeReprovision, isolationChangeStrategy(migrate.Reprovision))
}

func TestReportIsolationChange_PatchesOnlyThatField(t *testing.T) {
	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "test-machine"},
		Status:     v1alpha3.MachineStatus{Isolation: v1alpha3.IsolationNone, Message: "keep me"},
	}
	c := fakeStatusClient(machine)

	require.NoError(t, reportIsolationChange(context.Background(), c, "test-machine", v1alpha3.IsolationChangeStatus{
		TargetMode: v1alpha3.IsolationContainer, Phase: v1alpha3.IsolationChangeFailed, Message: "boom",
	}))

	var got v1alpha3.Machine
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "test-machine"}, &got))
	require.NotNil(t, got.Status.IsolationChange)
	assert.Equal(t, "boom", got.Status.IsolationChange.Message)
	assert.False(t, got.Status.IsolationChange.LastTransitionTime.IsZero(), "the transition time is stamped")
	assert.Equal(t, v1alpha3.IsolationNone, got.Status.Isolation)
	assert.Equal(t, "keep me", got.Status.Message)
}

func TestReportIsolationChange_KeepsTheTransitionTimeWhilePhaseIsUnchanged(t *testing.T) {
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine"}}
	c := fakeStatusClient(machine)
	change := v1alpha3.IsolationChangeStatus{TargetMode: v1alpha3.IsolationContainer, Phase: v1alpha3.IsolationChangeInProgress}

	require.NoError(t, reportIsolationChange(context.Background(), c, "test-machine", change))

	var first v1alpha3.Machine
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "test-machine"}, &first))

	change.Message = "still going"
	require.NoError(t, reportIsolationChange(context.Background(), c, "test-machine", change))

	var second v1alpha3.Machine
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "test-machine"}, &second))
	assert.Equal(t, first.Status.IsolationChange.LastTransitionTime, second.Status.IsolationChange.LastTransitionTime)
	assert.Equal(t, "still going", second.Status.IsolationChange.Message)
}

func TestReportIsolationChange_MissingMachineIsAnError(t *testing.T) {
	require.Error(t, reportIsolationChange(context.Background(), fakeStatusClient(), "missing", v1alpha3.IsolationChangeStatus{}))
}
