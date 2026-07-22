// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package machineops

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestSessionStatusRecorderUpdatesOnlyExactSessionAndTarget(t *testing.T) {
	t.Parallel()

	sessionA := statusTestSession("session-a", "session-a-uid", "operation-uid", "machine-a")
	sessionB := statusTestSession("session-b", "session-b-uid", "operation-uid", "machine-b")
	op := testOperation("operation-a", v1alpha3.OperationHostReplace)
	op.UID = "operation-uid"
	op.Status.Phase = v1alpha3.OperationPhaseInProgress
	op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{
		{MachineRef: "machine-a", Phase: v1alpha3.OperationPhaseInProgress, Input: &v1alpha3.MachineOperationTargetInput{NetbootSessionRef: &v1alpha3.NetbootSessionReference{Name: sessionA.Name, UID: sessionA.UID}}},
		{MachineRef: "machine-b", Phase: v1alpha3.OperationPhaseInProgress, Input: &v1alpha3.MachineOperationTargetInput{NetbootSessionRef: &v1alpha3.NetbootSessionReference{Name: sessionB.Name, UID: sessionB.UID}}},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(sessionA, sessionB, op).WithStatusSubresource(sessionA, sessionB, op).Build()
	recorder := &SessionStatusRecorder{Client: c, Now: fixedNow}

	require.NoError(t, recorder.RecordCondition(context.Background(), sessionA.Name, sessionA.UID, metav1.Condition{
		Type:    v1alpha3.NetbootSessionConditionBootImageWritten,
		Status:  metav1.ConditionTrue,
		Reason:  "Succeeded",
		Message: "image written",
	}))

	var updatedA v1alpha3.NetbootSession
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: sessionA.Name}, &updatedA))
	require.True(t, apimeta.IsStatusConditionTrue(updatedA.Status.Conditions, v1alpha3.NetbootSessionConditionBootImageWritten))

	var updatedB v1alpha3.NetbootSession
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: sessionB.Name}, &updatedB))
	require.Nil(t, apimeta.FindStatusCondition(updatedB.Status.Conditions, v1alpha3.NetbootSessionConditionBootImageWritten))

	var updatedOp v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updatedOp))
	require.True(t, apimeta.IsStatusConditionTrue(updatedOp.Status.Targets[0].Conditions, v1alpha3.MachineOperationConditionBootImageWritten))
	require.Nil(t, apimeta.FindStatusCondition(updatedOp.Status.Targets[1].Conditions, v1alpha3.MachineOperationConditionBootImageWritten))
	require.Nil(t, apimeta.FindStatusCondition(updatedOp.Status.Conditions, v1alpha3.MachineOperationConditionBootImageWritten))
}

func TestSessionStatusRecorderRejectsStaleSessionUID(t *testing.T) {
	t.Parallel()

	session := statusTestSession("session-a", "current-uid", "operation-uid", "machine-a")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(session).WithStatusSubresource(session).Build()
	recorder := &SessionStatusRecorder{Client: c}

	err := recorder.RecordCondition(context.Background(), session.Name, types.UID("stale-uid"), metav1.Condition{
		Type:   v1alpha3.NetbootSessionConditionCloudInitDone,
		Status: metav1.ConditionTrue,
		Reason: "Succeeded",
	})
	require.ErrorContains(t, err, "identity changed")
}

func statusTestSession(name string, uid, operationUID types.UID, machineName string) *v1alpha3.NetbootSession {
	return &v1alpha3.NetbootSession{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid},
		Spec: v1alpha3.NetbootSessionSpec{
			Machine:   v1alpha3.NetbootSessionObjectSnapshot{Name: machineName, UID: types.UID(machineName + "-uid"), Generation: 1},
			Operation: v1alpha3.NetbootSessionObjectSnapshot{Name: "operation-a", UID: operationUID, Generation: 1},
		},
		Status: v1alpha3.NetbootSessionStatus{Phase: v1alpha3.NetbootSessionPhaseActive},
	}
}
