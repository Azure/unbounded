// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestReportConditionTransitions(t *testing.T) {
	seen := map[string]conditionState{}
	cond := metav1.Condition{
		Type:    v1alpha3.MachineOperationConditionCloudInitDone,
		Status:  metav1.ConditionFalse,
		Reason:  "Running",
		Message: "Machine machine-1 started first-boot cloud-init",
	}

	out := captureStdout(t, func() {
		reportConditionTransitions([]metav1.Condition{cond}, seen)
		reportConditionTransitions([]metav1.Condition{cond}, seen)

		cond.Message = "Machine machine-1 is still running first-boot cloud-init"
		reportConditionTransitions([]metav1.Condition{cond}, seen)

		cond.Status = metav1.ConditionTrue
		cond.Reason = "Succeeded"
		cond.Message = "Machine machine-1 completed first-boot cloud-init successfully"
		reportConditionTransitions([]metav1.Condition{cond}, seen)
	})

	require.Equal(t, 1, strings.Count(out, "Running first-boot cloud-init"))
	require.Contains(t, out, "Cloud-init complete")
	require.NotContains(t, out, "Condition CloudInitDone")
}

func TestReportTargetTransitions(t *testing.T) {
	seen := map[string]string{}
	target := v1alpha3.MachineOperationTargetStatus{
		MachineRef: "machine-1",
		Phase:      v1alpha3.OperationPhaseInProgress,
		Stage:      v1alpha3.OperationStageWaitingRepave,
		Message:    "waiting for PXE repave to complete",
	}

	out := captureStdout(t, func() {
		reportTargetTransitions([]v1alpha3.MachineOperationTargetStatus{target}, seen)
		reportTargetTransitions([]v1alpha3.MachineOperationTargetStatus{target}, seen)

		target.Message = "still waiting for PXE repave"
		reportTargetTransitions([]v1alpha3.MachineOperationTargetStatus{target}, seen)

		target.Stage = v1alpha3.OperationStageWaitingNode
		target.Message = "waiting for Node machine-1 to exist"
		reportTargetTransitions([]v1alpha3.MachineOperationTargetStatus{target}, seen)
	})

	require.Equal(t, 1, strings.Count(out, "Booting PXE installer"))
	require.Contains(t, out, "Waiting for node to join cluster")
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)

	os.Stdout = w

	fn()

	require.NoError(t, w.Close())

	os.Stdout = oldStdout

	out, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())

	return string(out)
}
