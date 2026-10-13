// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaldashboard

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func testStagePlan() []signalv1.CertificationStage {
	return []signalv1.CertificationStage{
		{ID: "preflight", DisplayName: "Preflight", Category: "health", Required: true},
		{ID: "workload", DisplayName: "Workload", Category: "compute", Required: true},
		{ID: "optional", DisplayName: "Optional check", Category: "fabric", Conditional: true},
		{ID: "health-after", DisplayName: "Final health", Category: "health", Required: true},
	}
}

func TestReconcileStagePlanShowsFullPlanBeforeEvents(t *testing.T) {
	t.Parallel()

	stages, issues, current := reconcileStagePlan(testStagePlan(), nil, false, fixedTime)
	require.Len(t, stages, 4)
	assert.Empty(t, issues)
	assert.Empty(t, current)
	assert.Equal(t, []string{"Pending", "Pending", "Pending", "Pending"}, []string{
		stages[0].Phase, stages[1].Phase, stages[2].Phase, stages[3].Phase,
	})
}

func TestReconcileStagePlanMapsPassedRunningAndPending(t *testing.T) {
	t.Parallel()

	events := parseStageEvents([]byte(
		"ignored\n" +
			`PROJECT_SIGNAL_STAGE {"name":"preflight","status":"running","at_epoch":1735787000}` + "\n" +
			`PROJECT_SIGNAL_STAGE {"name":"preflight","status":"passed","at_epoch":1735787010}` + "\n" +
			`PROJECT_SIGNAL_STAGE {"name":"workload","status":"running","at_epoch":1735787011}` + "\n" +
			`PROJECT_SIGNAL_STAGE not-json` + "\n",
	))
	stages, issues, current := reconcileStagePlan(testStagePlan(), events, false, time.Unix(1735787020, 0))
	assert.Empty(t, issues)
	assert.Equal(t, "workload", current)
	assert.Equal(t, "Succeeded", stages[0].Phase)
	assert.Equal(t, "Running", stages[1].Phase)
	assert.Equal(t, "Pending", stages[2].Phase)
}

func TestReconcileStagePlanSurfacesMismatchAndHaltsAfterFailure(t *testing.T) {
	t.Parallel()

	events := []stageEvent{
		{Name: "preflight", Status: "running", AtEpoch: 1},
		{Name: "preflight", Status: "failed", AtEpoch: 2},
		{Name: "unknown", Status: "running", AtEpoch: 3},
	}
	stages, issues, _ := reconcileStagePlan(testStagePlan(), events, true, fixedTime)
	assert.Equal(t, "Failed", stages[0].Phase)
	assert.Equal(t, "Skipped", stages[1].Phase)
	assert.Contains(t, issues, `Runtime emitted unknown stage "unknown".`)
}

func TestReconcileStagePlanReportsDuplicateOutOfOrderAndMissingEvents(t *testing.T) {
	t.Parallel()

	events := []stageEvent{
		{Name: "workload", Status: "running", AtEpoch: 1},
		{Name: "workload", Status: "running", AtEpoch: 2},
		{Name: "preflight", Status: "running", AtEpoch: 3},
		{Name: "preflight", Status: "passed", AtEpoch: 4},
	}
	_, issues, _ := reconcileStagePlan(testStagePlan(), events, true, fixedTime)
	assert.Contains(t, issues, `Stage "workload" emitted duplicate Running events.`)
	assert.Contains(t, issues, `Stage "preflight" started out of declared order.`)
	assert.Contains(t, issues, `Terminal run is missing required stage "workload".`)
	assert.Contains(t, issues, `Terminal run is missing required stage "health-after".`)
}

func TestReconcileStagePlanAllowsConditionalSkip(t *testing.T) {
	t.Parallel()

	events := []stageEvent{
		{Name: "preflight", Status: "passed", AtEpoch: 1},
		{Name: "workload", Status: "passed", AtEpoch: 2},
		{Name: "optional", Status: "skipped", AtEpoch: 3, Reason: "NotSupported"},
		{Name: "health-after", Status: "passed", AtEpoch: 4},
	}
	stages, issues, _ := reconcileStagePlan(testStagePlan(), events, true, fixedTime)
	assert.Empty(t, issues)
	assert.Equal(t, "Skipped", stages[2].Phase)
	assert.Equal(t, "NotSupported", stages[2].Reason)
}

func TestRunPurposeAndClassification(t *testing.T) {
	t.Parallel()

	old := signalv1.CertificationRun{
		ObjectMeta: metav1.ObjectMeta{Name: "old", CreationTimestamp: metav1.NewTime(fixedTime.Add(-time.Hour))},
		Spec: signalv1.CertificationRunSpec{
			Profile:      signalv1.PinnedProfile{Name: "gpu-full"},
			Participants: []signalv1.Participant{{NodeName: "node-1"}},
			Script:       signalv1.ScriptSnapshot{Key: "full.sh"},
		},
		Status: signalv1.CertificationRunStatus{Phase: signalv1.RunPhaseFailed, Verdict: "Fail"},
	}
	newer := old
	newer.Name = "new"
	newer.CreationTimestamp = metav1.NewTime(fixedTime)
	newer.Status = signalv1.CertificationRunStatus{Phase: signalv1.RunPhaseSucceeded, Verdict: "Pass"}

	classification, superseded := runClassification(&old, []signalv1.CertificationRun{old, newer})
	assert.Equal(t, "Superseded", classification)
	assert.True(t, superseded)

	diagnostic := old
	diagnostic.Spec.Profile.Name = "storage-diagnostic"
	diagnostic.Status.Reason = "StorageDiagnosticOnly"
	assert.True(t, diagnosticRun(&diagnostic))
	assert.Equal(t, "Diagnostic", func() string { purpose, _ := runPurpose(&diagnostic); return purpose }())

	explicit := old
	explicit.Spec.Purpose = signalv1.PurposeTargetedVerification

	assert.Equal(t, "Targeted verification", func() string { purpose, _ := runPurpose(&explicit); return purpose }())
}

type fakePodLogs struct {
	content    []byte
	err        error
	tailLines  int64
	limitBytes int64
}

func (f *fakePodLogs) ReadPodLog(_ context.Context, _, _, _ string, tailLines, limitBytes int64) ([]byte, error) {
	f.tailLines = tailLines
	f.limitBytes = limitBytes

	return f.content, f.err
}

func TestBoundedLogRequestAndUnavailableLogs(t *testing.T) {
	t.Parallel()

	logs := &fakePodLogs{err: errors.New("unavailable")}
	reader := fixtureReader()
	reader.runs[0].Spec.StagePlan = testStagePlan()
	reader.runs[0].Status.Phase = signalv1.RunPhaseRunning
	reader.runs[0].Status.FinishedAt = nil
	reader.pods = append(reader.pods, corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "run-pod", Labels: map[string]string{"workflows.argoproj.io/workflow": "workflow-example"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:    "main",
			Command: []string{"/bin/bash", "/scripts/full.sh"},
		}}},
	})
	collector := fixtureCollector(reader)
	collector.PodLogs = logs
	detail, err := collector.Run(context.Background(), collector.Collect(context.Background()), "run-example")
	require.NoError(t, err)
	assert.Equal(t, maxStageLogLines, logs.tailLines)
	assert.Equal(t, maxStageLogBytes, logs.limitBytes)
	assert.Contains(t, detail.PlanIssues, "Structured stage events are temporarily unavailable.")
	assert.Equal(t, []string{"/bin/bash", "/scripts/full.sh"}, detail.Script.Command)
}

func TestWorkflowStagePodsIncludesEveryMainTaskPodInNodeOrder(t *testing.T) {
	t.Parallel()

	workflow := &unstructured.Unstructured{}
	workflow.SetName("workflow-example")
	workflow.SetUID("workflow-uid")

	pods := []corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "health-after",
				Labels:      map[string]string{"workflows.argoproj.io/workflow": "workflow-example"},
				Annotations: map[string]string{"workflows.argoproj.io/node-name": "workflow-example.health-after"},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "preflight",
				Labels:      map[string]string{"workflows.argoproj.io/workflow": "workflow-example"},
				Annotations: map[string]string{"workflows.argoproj.io/node-name": "workflow-example.node-health"},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "sidecar-only",
				Labels: map[string]string{"workflows.argoproj.io/workflow": "workflow-example"},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "wait"}}},
		},
	}

	got := workflowStagePods(workflow, pods)
	require.Len(t, got, 2)
	assert.Equal(t, "health-after", got[0].Name)
	assert.Equal(t, "preflight", got[1].Name)
}

func TestScriptStatusShowsPayloadCommandAndDigestOnly(t *testing.T) {
	t.Parallel()

	run := &signalv1.CertificationRun{Spec: signalv1.CertificationRunSpec{
		RunnerImage: "private.example/runner@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Script: signalv1.ScriptSnapshot{
			Key:           "full.sh",
			ConfigMapName: "private-run-name-scripts",
			Digest:        "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
	}}
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name:    "main",
		Command: []string{"/var/run/argo/argoexec"},
		Args:    []string{"emissary", "--", "/bin/bash", "/scripts/full.sh"},
	}}}}

	status := scriptStatus(run, pod)
	assert.Equal(t, []string{"/bin/bash", "/scripts/full.sh"}, status.Command)
	assert.Equal(t, "sha256:aaaaaaaa...", status.RunnerImageDigest)
	assert.NotContains(t, status.RunnerImageDigest, "private.example")
	assert.Equal(t, "run-snapshot-[redacted]-scripts", status.SnapshotConfigMap)
}

func TestWorkflowPodPrefersNodeHealthOverLaterWorkflowTasks(t *testing.T) {
	t.Parallel()

	workflow := &unstructured.Unstructured{}
	workflow.SetName("workflow-example")

	pods := []corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "workflow-example-mpi-backend",
				Labels:      map[string]string{"workflows.argoproj.io/workflow": workflow.GetName()},
				Annotations: map[string]string{"workflows.argoproj.io/node-name": "workflow-example.mpi-backend"},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "workflow-example-node-health",
				Labels:      map[string]string{"workflows.argoproj.io/workflow": workflow.GetName()},
				Annotations: map[string]string{"workflows.argoproj.io/node-name": "workflow-example.node-health"},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}},
		},
	}

	pod := workflowPod(workflow, pods)
	require.NotNil(t, pod)
	assert.Equal(t, "workflow-example-node-health", pod.Name)
}
