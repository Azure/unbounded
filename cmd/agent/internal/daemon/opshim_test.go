// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func configMapWithOps(name, opsJSON string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: operationNamespace,
			Labels:    map[string]string{operationLabelKey: "test-machine"},
		},
		Data: map[string]string{
			operationDataKey: opsJSON,
		},
	}
}

func Test_parseOperations_Valid(t *testing.T) {
	cm := configMapWithOps("ops-1", `[{"type":"reboot","state":"pending"}]`)

	ops, err := parseOperations(cm)
	require.NoError(t, err)
	require.Len(t, ops, 1)
	assert.Equal(t, OpTypeReboot, ops[0].Type)
	assert.Equal(t, OpStatePending, ops[0].State)
}

func Test_parseOperations_Multiple(t *testing.T) {
	cm := configMapWithOps("ops-2", `[
		{"type":"reboot","state":"completed"},
		{"type":"reboot","state":"pending","message":""}
	]`)

	ops, err := parseOperations(cm)
	require.NoError(t, err)
	require.Len(t, ops, 2)
	assert.Equal(t, OpStateCompleted, ops[0].State)
	assert.Equal(t, OpStatePending, ops[1].State)
}

func Test_parseOperations_MissingKey(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "ops-3", Namespace: operationNamespace},
		Data:       map[string]string{"other": "data"},
	}

	_, err := parseOperations(cm)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing")
}

func Test_parseOperations_InvalidJSON(t *testing.T) {
	cm := configMapWithOps("ops-4", `not-json`)

	_, err := parseOperations(cm)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse operations JSON")
}

func Test_serializeOperations_Roundtrip(t *testing.T) {
	original := []Operation{
		{Type: OpTypeReboot, State: OpStatePending},
		{Type: OpTypeReboot, State: OpStateCompleted, Message: "done"},
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "ops-5", Namespace: operationNamespace},
	}

	err := serializeOperations(cm, original)
	require.NoError(t, err)

	parsed, err := parseOperations(cm)
	require.NoError(t, err)
	assert.Equal(t, original, parsed)
}

func Test_hasPendingOperations(t *testing.T) {
	tests := []struct {
		name string
		ops  []Operation
		want bool
	}{
		{
			name: "no operations",
			ops:  nil,
			want: false,
		},
		{
			name: "all completed",
			ops:  []Operation{{State: OpStateCompleted}, {State: OpStateFailed}},
			want: false,
		},
		{
			name: "one pending",
			ops:  []Operation{{State: OpStateCompleted}, {State: OpStatePending}},
			want: true,
		},
		{
			name: "one in_progress (crash recovery)",
			ops:  []Operation{{State: OpStateInProgress}},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, hasPendingOperations(tt.ops))
		})
	}
}
