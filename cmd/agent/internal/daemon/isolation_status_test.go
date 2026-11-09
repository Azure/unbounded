// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	api "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func isolationStatusClient(t *testing.T, funcs *interceptor.Funcs) (client.Client, *api.Machine) {
	t.Helper()

	m := &api.Machine{ObjectMeta: metav1.ObjectMeta{Name: "worker"}}
	b := fake.NewClientBuilder().WithScheme(fakeScheme()).WithObjects(m).WithStatusSubresource(m)

	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}

	return b.Build(), m
}

// optimisticLockConflicts fails, as the API server would after a concurrent
// write, every status write that carries a resourceVersion or is not a merge
// patch.
func optimisticLockConflicts(t *testing.T) *interceptor.Funcs {
	t.Helper()

	conflict := apierrors.NewConflict(schema.GroupResource{Group: "unbounded-cloud.io", Resource: "machines"}, "worker", nil)

	return &interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			data, err := patch.Data(obj)
			require.NoError(t, err)

			if patch.Type() != types.MergePatchType || strings.Contains(string(data), "resourceVersion") {
				return conflict
			}

			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
		SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
			return conflict
		},
	}
}

// noReads fails every read, so a write that succeeds did not depend on a
// (possibly stale) read of the Machine.
func noReads(t *testing.T) *interceptor.Funcs {
	t.Helper()

	funcs := optimisticLockConflicts(t)
	funcs.Get = func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		return apierrors.NewServiceUnavailable("reads are disabled in this test")
	}

	return funcs
}

func TestPatchIsolationStatus(t *testing.T) {
	c, m := isolationStatusClient(t, optimisticLockConflicts(t))

	change := &api.IsolationChangeStatus{
		TargetMode:         api.IsolationNSpawn,
		Phase:              api.IsolationChangeSucceeded,
		Strategy:           api.IsolationChangeInPlace,
		Message:            "done",
		LastTransitionTime: metav1.Now(),
	}

	// The interceptor conflicts on any locked write, so success shows the
	// patch carries no resourceVersion.
	require.NoError(t, patchIsolationStatus(t.Context(), c, m.Name, isolationStatus{Applied: api.IsolationNSpawn, Change: change}))

	// It does not read the Machine first either.
	unreadable, _ := isolationStatusClient(t, noReads(t))
	require.NoError(t, patchIsolationStatus(t.Context(), unreadable, m.Name, isolationStatus{Applied: api.IsolationNSpawn, Change: change}))

	var got api.Machine
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(m), &got))
	assert.Equal(t, api.IsolationNSpawn, got.Status.Isolation)
	require.NotNil(t, got.Status.IsolationChange)
	assert.Equal(t, api.IsolationChangeSucceeded, got.Status.IsolationChange.Phase)
	assert.Equal(t, "done", got.Status.IsolationChange.Message)

	// Applied alone leaves the change as it is.
	require.NoError(t, patchIsolationStatus(t.Context(), c, m.Name, isolationStatus{Applied: api.IsolationNSpawn}))
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(m), &got))
	require.NotNil(t, got.Status.IsolationChange)

	// Nothing to write is a no-op, even for a Machine that does not exist.
	require.NoError(t, patchIsolationStatus(t.Context(), c, "absent", isolationStatus{}))

	// A missing Machine is an error.
	require.Error(t, patchIsolationStatus(t.Context(), c, "absent", isolationStatus{Applied: api.IsolationNSpawn}))
}

// The patch must not clear status fields other writers own, such as the phase
// machina sets or the provisioning format.
func TestPatchIsolationStatus_KeepsOtherStatusFields(t *testing.T) {
	m := &api.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "worker"},
		Status:     api.MachineStatus{Phase: api.MachinePhaseReady, ObservedProvisioningFormat: api.ProvisioningFormatIgnition},
	}
	c := fake.NewClientBuilder().WithScheme(fakeScheme()).WithObjects(m).WithStatusSubresource(m).Build()

	require.NoError(t, patchIsolationStatus(t.Context(), c, m.Name, isolationStatus{Applied: api.IsolationNSpawn}))

	var got api.Machine
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(m), &got))
	assert.Equal(t, api.MachinePhaseReady, got.Status.Phase)
	assert.Equal(t, api.ProvisioningFormatIgnition, got.Status.ObservedProvisioningFormat)
	assert.Equal(t, api.IsolationNSpawn, got.Status.Isolation)
}

func TestPatchIsolationStatus_TruncatesTheMessage(t *testing.T) {
	var sent []byte

	c, m := isolationStatusClient(t, &interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			data, err := patch.Data(obj)
			require.NoError(t, err)

			sent = data

			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})

	long := strings.Repeat("é", api.IsolationChangeMessageMaxLength)
	change := &api.IsolationChangeStatus{TargetMode: "none", Phase: api.IsolationChangeFailed, Message: long}

	require.NoError(t, patchIsolationStatus(t.Context(), c, m.Name, isolationStatus{Change: change}))
	assert.Len(t, change.Message, len(long), "the caller's value is not modified")

	var patch struct {
		Status struct {
			IsolationChange api.IsolationChangeStatus `json:"isolationChange"`
		} `json:"status"`
	}
	require.NoError(t, json.Unmarshal(sent, &patch))

	msg := patch.Status.IsolationChange.Message
	assert.LessOrEqual(t, len(msg), api.IsolationChangeMessageMaxLength)
	assert.True(t, utf8.ValidString(msg))
	assert.NotContains(t, string(sent), "\"isolation\":", "an empty Applied is not written")
}

// A refused change leaves a reason. A later attempt writes none, and the
// stored reason must not survive it, or machina reports a failed reprovision
// as a refusal that is still waiting for the opt-in.
func TestPatchIsolationStatus_ReplacesTheWholeChange(t *testing.T) {
	c, m := isolationStatusClient(t, nil)

	get := func() *api.IsolationChangeStatus {
		t.Helper()

		var got api.Machine
		require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(m), &got))
		require.NotNil(t, got.Status.IsolationChange)

		return got.Status.IsolationChange
	}

	for _, reason := range []string{api.IsolationChangeReasonReprovisionNotAllowed, api.IsolationChangeReasonUnsupportedMode} {
		t.Run(reason, func(t *testing.T) {
			refused := &api.IsolationChangeStatus{
				TargetMode: "none", Phase: api.IsolationChangeFailed, Strategy: api.IsolationChangeReprovision,
				Reason: reason, Message: "refused", LastTransitionTime: metav1.Now(),
			}
			require.NoError(t, patchIsolationStatus(t.Context(), c, m.Name, isolationStatus{Change: refused}))
			assert.Equal(t, reason, get().Reason)

			inProgress := &api.IsolationChangeStatus{TargetMode: "none", Phase: api.IsolationChangeInProgress, Strategy: api.IsolationChangeReprovision}
			require.NoError(t, patchIsolationStatus(t.Context(), c, m.Name, isolationStatus{Change: inProgress}))

			got := get()
			assert.Equal(t, api.IsolationChangeInProgress, got.Phase)
			assert.Empty(t, got.Reason, "the refusal reason is cleared")
			assert.Empty(t, got.Message, "the refusal message is cleared")
			assert.True(t, got.LastTransitionTime.IsZero(), "the old transition time is cleared")

			failed := &api.IsolationChangeStatus{TargetMode: "none", Phase: api.IsolationChangeFailed, Message: "token rejected"}
			require.NoError(t, patchIsolationStatus(t.Context(), c, m.Name, isolationStatus{Change: failed}))

			got = get()
			assert.Equal(t, api.IsolationChangeFailed, got.Phase)
			assert.Empty(t, got.Reason)
			assert.Empty(t, got.Strategy, "the strategy of the earlier attempt is cleared")
			assert.Equal(t, "token rejected", got.Message)

			// Applied alone still leaves the change as it is.
			require.NoError(t, patchIsolationStatus(t.Context(), c, m.Name, isolationStatus{Applied: api.IsolationNSpawn}))
			assert.Equal(t, "token rejected", get().Message)
		})
	}
}

// The patch sends every empty optional field as null, so it does not depend
// on how a particular client or server merges a missing key.
func TestIsolationChangePatch_NullsEmptyOptionalFields(t *testing.T) {
	value, err := isolationChangePatch(api.IsolationChangeStatus{TargetMode: "none", Phase: api.IsolationChangeFailed})
	require.NoError(t, err)

	data, err := json.Marshal(value)
	require.NoError(t, err)
	assert.JSONEq(t, `{"targetMode":"none","phase":"Failed","strategy":null,"reason":null,"message":null,"lastTransitionTime":null}`, string(data))

	// Set fields are sent as they are.
	now := metav1.NewTime(metav1.Now().Rfc3339Copy().Time)
	value, err = isolationChangePatch(api.IsolationChangeStatus{
		TargetMode: "none", Phase: api.IsolationChangeFailed, Strategy: api.IsolationChangeReprovision,
		Reason: api.IsolationChangeReasonReprovisionNotAllowed, Message: "refused", LastTransitionTime: now,
	})
	require.NoError(t, err)

	data, err = json.Marshal(value)
	require.NoError(t, err)

	var back api.IsolationChangeStatus
	require.NoError(t, json.Unmarshal(data, &back))
	assert.Equal(t, api.IsolationChangeReprovision, back.Strategy)
	assert.Equal(t, api.IsolationChangeReasonReprovisionNotAllowed, back.Reason)
	assert.Equal(t, "refused", back.Message)
	assert.True(t, now.Equal(&back.LastTransitionTime))
}

// Every optional field of IsolationChangeStatus must be listed, or a field
// added later would keep its old value across writes.
func TestIsolationChangeOptionalFields_CoverTheType(t *testing.T) {
	var optional []string

	typ := reflect.TypeOf(api.IsolationChangeStatus{})
	for i := range typ.NumField() {
		name, opts, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if strings.Contains(opts, "omitempty") {
			optional = append(optional, name)
		}
	}

	assert.ElementsMatch(t, optional, isolationChangeOptionalFields)
}

func TestReportAppliedIsolation(t *testing.T) {
	for _, value := range []string{"", "nspawn"} {
		c, m := isolationStatusClient(t, nil)
		cfg := &provision.AgentConfig{MachineName: m.Name, Isolation: value}

		require.NoError(t, reportAppliedIsolation(t.Context(), c, cfg), value)
		require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(m), m))
		require.Equal(t, api.IsolationNSpawn, m.Status.Isolation, value)

		// Reporting again is harmless.
		require.NoError(t, reportAppliedIsolation(t.Context(), c, cfg), value)
	}

	c, m := isolationStatusClient(t, nil)
	require.ErrorContains(t, reportAppliedIsolation(t.Context(), c, &provision.AgentConfig{MachineName: m.Name, Isolation: "microvm"}), "unsupported isolation mode")
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(m), m))
	require.Empty(t, m.Status.Isolation)
}

func TestReportMachineObservations(t *testing.T) {
	c, m := isolationStatusClient(t, nil)

	require.NoError(t, reportMachineObservations(t.Context(), discardLogger(), c, &provision.AgentConfig{MachineName: m.Name, ProvisioningFormat: "ignition"}))
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(m), m))
	require.Equal(t, api.ProvisioningFormatIgnition, m.Status.ObservedProvisioningFormat)
	require.Equal(t, api.IsolationNSpawn, m.Status.Isolation)

	// The spec belongs to the operator: reporting never writes it.
	require.Empty(t, m.Spec.Isolation)

	// The isolation report never fails the registration.
	require.NoError(t, reportMachineObservations(t.Context(), discardLogger(), c, &provision.AgentConfig{MachineName: m.Name, Isolation: "microvm"}))

	failing, m2 := isolationStatusClient(t, &interceptor.Funcs{
		SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
			return apierrors.NewInternalError(context.DeadlineExceeded)
		},
	})
	require.NoError(t, reportMachineObservations(t.Context(), discardLogger(), failing, &provision.AgentConfig{MachineName: m2.Name}))

	// The provisioning format report still does.
	require.Error(t, reportMachineObservations(t.Context(), discardLogger(), c, &provision.AgentConfig{MachineName: m.Name, ProvisioningFormat: "bogus"}))
}

func Test_buildMachineCR_CarriesTheConfiguredIsolation(t *testing.T) {
	cfg := baseConfig()
	cfg.Isolation = "nspawn"
	assert.Equal(t, api.IsolationNSpawn, buildMachineCR(cfg).Spec.Isolation)

	assert.Empty(t, buildMachineCR(baseConfig()).Spec.Isolation, "no isolation in the config leaves the spec undeclared")
}

func Test_hasDrift_Isolation(t *testing.T) {
	applied, desired := baseConfig(), baseConfig()

	// Unset and explicit nspawn are the same mode.
	desired.Isolation = "nspawn"
	assert.False(t, hasDrift(applied, desired))

	// A different declared mode is drift, known to this agent or not.
	for _, mode := range []string{"none", "container", "microvm"} {
		desired.Isolation = mode
		assert.True(t, hasDrift(applied, desired), mode)
	}
}

func Test_activeModeOf(t *testing.T) {
	assert.Equal(t, isolation.ModeNSpawn, activeModeOf(&ActiveMachine{}))
	assert.Equal(t, isolation.ModeNSpawn, activeModeOf(&ActiveMachine{Mode: isolation.ModeNSpawn}))
}

func Test_decodeActiveMachine_Mode(t *testing.T) {
	encode := func(isolation string) []byte {
		cfg := baseConfig()
		cfg.Isolation = isolation

		data, err := json.Marshal(cfg)
		require.NoError(t, err)

		return data
	}

	for value, want := range map[string]isolation.Mode{
		"": isolation.ModeNSpawn, "nspawn": isolation.ModeNSpawn, "none": isolation.ModeNone, "container": isolation.ModeContainer,
	} {
		active, err := decodeActiveMachine(discardLogger(), "kube1", "/applied.json", encode(value))
		require.NoError(t, err, value)
		assert.Equal(t, want, active.Mode, value)
		assert.Equal(t, "kube1", active.Name)
	}

	// A mode this agent cannot operate fails closed rather than being treated
	// as nspawn.
	for _, value := range []string{"microvm"} {
		_, err := decodeActiveMachine(discardLogger(), "kube1", "/applied.json", encode(value))
		require.ErrorContains(t, err, "unsupported isolation mode", value)
	}
}

func repaveFixture(t *testing.T, spec api.IsolationMode, version string) (*repaveReconciler, *fakeNodeOperator, client.Client) {
	t.Helper()

	machine := &api.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "test-machine"},
		Spec: api.MachineSpec{
			Isolation:        spec,
			ConfigurationRef: &api.MachineConfigurationRef{Name: "config-a", Version: ptr.To(int32(2))},
		},
	}
	base := baseConfig()
	mcv := machineConfigurationVersion("config-a", 2, api.MachineConfigurationTemplate{
		Kubernetes: &api.MachineConfigurationKubernetes{Version: version},
		Agent:      &api.MachineConfigurationAgent{Image: base.OCIImage},
	})

	op := &fakeNodeOperator{active: &ActiveMachine{Name: "kube1", Config: base, Mode: isolation.ModeNSpawn}}
	c := fakeStatusClient(machine, mcv)

	return &repaveReconciler{
		installation: installstate.NewStore(filepath.Join(t.TempDir(), "state"), filepath.Join(t.TempDir(), "install.lock")),
		Client:       c,
		log:          discardLogger(),
		machineName:  "test-machine",
		nodeName:     "test-node",
		nodeOperator: op,
		env:          testMigrationEnv(t),
	}, op, c
}

// A declared mode this agent cannot change to is refused and reported, and the
// repave goes ahead in the current mode so a version rollout continues. A mode
// this agent knows but can reach only by a reprovision is refused too while
// the Machine does not allow one.
func TestReconcileRepave_RefusesAModeChangeAndRepavesInTheCurrentMode(t *testing.T) {
	for target, want := range map[string]struct{ reason, msg string }{
		"none":      {api.IsolationChangeReasonReprovisionNotAllowed, api.AnnotationAllowReprovision},
		"container": {api.IsolationChangeReasonExperimentalNotEnabled, "Experimental.ContainerIsolation"},
		"microvm":   {api.IsolationChangeReasonUnsupportedMode, "cannot run isolation \"microvm\""},
	} {
		t.Run(target, func(t *testing.T) {
			r, op, c := repaveFixture(t, api.IsolationMode(target), "1.34.9")

			_, err := r.ReconcileRepave(t.Context(), "node-delete")
			require.NoError(t, err)

			require.NotNil(t, op.repaveConfig, "the version change still repaves")
			assert.Equal(t, "1.34.9", op.repaveConfig.Cluster.Version)
			assert.Equal(t, baseConfig().Isolation, op.repaveConfig.Isolation, "the repave keeps the applied mode")

			var got api.Machine
			require.NoError(t, c.Get(t.Context(), client.ObjectKey{Name: "test-machine"}, &got))
			assert.Equal(t, api.IsolationNSpawn, got.Status.Isolation)
			require.NotNil(t, got.Status.IsolationChange)
			assert.Equal(t, api.IsolationMode(target), got.Status.IsolationChange.TargetMode)
			assert.Equal(t, api.IsolationChangeFailed, got.Status.IsolationChange.Phase)
			assert.Equal(t, want.reason, got.Status.IsolationChange.Reason)
			assert.Contains(t, got.Status.IsolationChange.Message, want.msg)
			assert.False(t, got.Status.IsolationChange.LastTransitionTime.IsZero())
		})
	}
}

// When the mode is the only difference, the refusal leaves nothing to repave.
func TestReconcileRepave_ARefusedModeChangeAloneDoesNotRepave(t *testing.T) {
	r, op, c := repaveFixture(t, "container", baseConfig().Cluster.Version)

	_, err := r.ReconcileRepave(t.Context(), "node-delete")
	require.NoError(t, err)
	assert.Nil(t, op.repaveConfig)
	assert.True(t, op.restartCalled, "the Node deleted for the change must register again")

	var got api.Machine
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Name: "test-machine"}, &got))
	require.NotNil(t, got.Status.IsolationChange)
	assert.Equal(t, api.IsolationChangeFailed, got.Status.IsolationChange.Phase)
}

// A failed report does not block the repave.
func TestReconcileRepave_ARefusalThatCannotBeReportedStillRepaves(t *testing.T) {
	r, op, _ := repaveFixture(t, "none", "1.34.9")
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
			return apierrors.NewInternalError(context.DeadlineExceeded)
		},
	})

	_, err := r.ReconcileRepave(t.Context(), "node-delete")
	require.NoError(t, err)
	require.NotNil(t, op.repaveConfig)
}

// Declaring the mode the node already runs is not a change.
func TestReconcileRepave_DeclaringTheCurrentModeIsNotAChange(t *testing.T) {
	r, op, c := repaveFixture(t, api.IsolationNSpawn, baseConfig().Cluster.Version)

	_, err := r.ReconcileRepave(t.Context(), "node-delete")
	require.NoError(t, err)
	assert.Nil(t, op.repaveConfig)

	var got api.Machine
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Name: "test-machine"}, &got))
	assert.Nil(t, got.Status.IsolationChange)
}
