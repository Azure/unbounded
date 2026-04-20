// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded-kube/api/machina/v1alpha3"
)

// mockExecutor records calls and returns a configurable error.
type mockExecutor struct {
	softRestartCalls []string
	softRestartErr   error
}

func (m *mockExecutor) softRestart(_ context.Context, _ *slog.Logger, machineName string) error {
	m.softRestartCalls = append(m.softRestartCalls, machineName)
	return m.softRestartErr
}

// stubFindActive returns a findActive function that always returns the given
// nspawn machine name.
func stubFindActive(nspawnName string) func(*slog.Logger) (*ActiveMachine, error) {
	return func(_ *slog.Logger) (*ActiveMachine, error) {
		return &ActiveMachine{Name: nspawnName}, nil
	}
}

func softRestartScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(v1alpha3.AddToScheme(s))

	return s
}

func softRestartClient(objs ...client.Object) client.WithWatch {
	return fake.NewClientBuilder().
		WithScheme(softRestartScheme()).
		WithObjects(objs...).
		Build().(client.WithWatch)
}

func opConfigMap(name, machineName, opsJSON string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: operationNamespace,
			Labels:    map[string]string{operationLabelKey: machineName},
		},
		Data: map[string]string{
			operationDataKey: opsJSON,
		},
	}
}

func getOpsFromConfigMap(t *testing.T, c client.Client, ns, name string) []Operation {
	t.Helper()

	var cm corev1.ConfigMap
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &cm))

	ops, err := parseOperations(&cm)
	require.NoError(t, err)

	return ops
}

func Test_reconcileSoftRestart_PendingReboot(t *testing.T) {
	cm := opConfigMap("ops-1", "test-machine", `[{"type":"reboot","state":"pending"}]`)
	c := softRestartClient(cm)
	exec := &mockExecutor{}

	r := &reconciler{client: c, machineName: "test-machine", exec: exec, findActive: stubFindActive("kube1")}

	err := r.reconcileSoftRestart(context.Background(), discardLogger(), operationNamespace+"/ops-1")
	require.NoError(t, err)

	// Executor should have been called once.
	assert.Equal(t, []string{"kube1"}, exec.softRestartCalls)

	// Operation should be completed.
	ops := getOpsFromConfigMap(t, c, operationNamespace, "ops-1")
	require.Len(t, ops, 1)
	assert.Equal(t, OpStateCompleted, ops[0].State)
}

func Test_reconcileSoftRestart_InProgressRecovery(t *testing.T) {
	// Simulates crash recovery: operation left in_progress by a previous run.
	cm := opConfigMap("ops-2", "test-machine", `[{"type":"reboot","state":"in_progress"}]`)
	c := softRestartClient(cm)
	exec := &mockExecutor{}

	r := &reconciler{client: c, machineName: "test-machine", exec: exec, findActive: stubFindActive("kube1")}

	err := r.reconcileSoftRestart(context.Background(), discardLogger(), operationNamespace+"/ops-2")
	require.NoError(t, err)

	assert.Len(t, exec.softRestartCalls, 1)

	ops := getOpsFromConfigMap(t, c, operationNamespace, "ops-2")
	require.Len(t, ops, 1)
	assert.Equal(t, OpStateCompleted, ops[0].State)
}

func Test_reconcileSoftRestart_NoPending(t *testing.T) {
	cm := opConfigMap("ops-3", "test-machine", `[{"type":"reboot","state":"completed"}]`)
	c := softRestartClient(cm)
	exec := &mockExecutor{}

	r := &reconciler{client: c, machineName: "test-machine", exec: exec, findActive: stubFindActive("kube1")}

	err := r.reconcileSoftRestart(context.Background(), discardLogger(), operationNamespace+"/ops-3")
	require.NoError(t, err)

	// Executor should not have been called.
	assert.Empty(t, exec.softRestartCalls)
}

func Test_reconcileSoftRestart_ExecutorError(t *testing.T) {
	cm := opConfigMap("ops-4", "test-machine", `[{"type":"reboot","state":"pending"}]`)
	c := softRestartClient(cm)
	exec := &mockExecutor{softRestartErr: fmt.Errorf("nspawn exploded")}

	r := &reconciler{client: c, machineName: "test-machine", exec: exec, findActive: stubFindActive("kube1")}

	err := r.reconcileSoftRestart(context.Background(), discardLogger(), operationNamespace+"/ops-4")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nspawn exploded")

	// Operation should be marked failed with the error message.
	ops := getOpsFromConfigMap(t, c, operationNamespace, "ops-4")
	require.Len(t, ops, 1)
	assert.Equal(t, OpStateFailed, ops[0].State)
	assert.Contains(t, ops[0].Message, "nspawn exploded")
}

func Test_reconcileSoftRestart_UnknownType(t *testing.T) {
	cm := opConfigMap("ops-5", "test-machine", `[{"type":"explode","state":"pending"}]`)
	c := softRestartClient(cm)
	exec := &mockExecutor{}

	r := &reconciler{client: c, machineName: "test-machine", exec: exec, findActive: stubFindActive("kube1")}

	err := r.reconcileSoftRestart(context.Background(), discardLogger(), operationNamespace+"/ops-5")
	require.Error(t, err)

	// Executor should not have been called (unknown type dispatches to
	// executeOperation which returns an error).
	assert.Empty(t, exec.softRestartCalls)

	ops := getOpsFromConfigMap(t, c, operationNamespace, "ops-5")
	require.Len(t, ops, 1)
	assert.Equal(t, OpStateFailed, ops[0].State)
	assert.Contains(t, ops[0].Message, "unknown operation type")
}

func Test_reconcileSoftRestart_MultipleOps_ProcessesFirstOnly(t *testing.T) {
	cm := opConfigMap("ops-6", "test-machine", `[
		{"type":"reboot","state":"pending"},
		{"type":"reboot","state":"pending"}
	]`)
	c := softRestartClient(cm)
	exec := &mockExecutor{}

	r := &reconciler{client: c, machineName: "test-machine", exec: exec, findActive: stubFindActive("kube1")}

	err := r.reconcileSoftRestart(context.Background(), discardLogger(), operationNamespace+"/ops-6")
	require.NoError(t, err)

	// Only one call - processes one operation per reconcile cycle.
	assert.Len(t, exec.softRestartCalls, 1)

	ops := getOpsFromConfigMap(t, c, operationNamespace, "ops-6")
	require.Len(t, ops, 2)
	assert.Equal(t, OpStateCompleted, ops[0].State)
	assert.Equal(t, OpStatePending, ops[1].State)
}

func Test_reconcileSoftRestart_SkipsCompleted(t *testing.T) {
	cm := opConfigMap("ops-7", "test-machine", `[
		{"type":"reboot","state":"completed"},
		{"type":"reboot","state":"pending"}
	]`)
	c := softRestartClient(cm)
	exec := &mockExecutor{}

	r := &reconciler{client: c, machineName: "test-machine", exec: exec, findActive: stubFindActive("kube1")}

	err := r.reconcileSoftRestart(context.Background(), discardLogger(), operationNamespace+"/ops-7")
	require.NoError(t, err)

	assert.Len(t, exec.softRestartCalls, 1)

	ops := getOpsFromConfigMap(t, c, operationNamespace, "ops-7")
	require.Len(t, ops, 2)
	assert.Equal(t, OpStateCompleted, ops[0].State)
	assert.Equal(t, OpStateCompleted, ops[1].State)
}

func Test_parseConfigMapKey(t *testing.T) {
	tests := []struct {
		key     string
		ns      string
		name    string
		wantErr bool
	}{
		{"unbounded-system/ops-1", "unbounded-system", "ops-1", false},
		{"ns/name", "ns", "name", false},
		{"invalid", "", "", true},
		{"/name", "", "", true},
		{"ns/", "", "", true},
		{"", "", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			ns, name, err := parseConfigMapKey(tt.key)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.ns, ns)
				assert.Equal(t, tt.name, name)
			}
		})
	}
}
