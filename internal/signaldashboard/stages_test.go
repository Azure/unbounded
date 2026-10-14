// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaldashboard

import (
	"context"
	"encoding/json"
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

func TestCertificationTestStagesRetainIndependentVerdictsAndSafeParameters(t *testing.T) {
	t.Parallel()

	gpu := signalv1.TestInvocation{ID: "gpu-burn", NodeName: "private-node", NodeUID: "private-node-uid", ContainerName: "main"}
	cpu := signalv1.TestInvocation{
		ID: "cpu-stress", NodeName: "private-node", NodeUID: "private-node-uid", ContainerName: "cpu-stress",
		Parameters: map[string]string{"cpu-workers": "8", "endpoint": "https://internal.invalid"},
	}
	run := &signalv1.CertificationRun{
		Spec: signalv1.CertificationRunSpec{TestPlan: []signalv1.TestInvocation{gpu, cpu}},
		Status: signalv1.CertificationRunStatus{
			Phase:    signalv1.RunPhaseFailed,
			Workflow: signalv1.WorkflowStatus{Attempt: 1},
			TestResults: []signalv1.TestResult{
				{TestInvocation: gpu, Attempt: 1, ExecutionState: "Succeeded", Verdict: "Pass"},
				{
					TestInvocation: cpu, Attempt: 1, ExecutionState: "Succeeded", Verdict: "Fail",
					Reason: "HostStressFailed", Measurements: "RAW-GPU-LOG-NOT-PUBLIC",
					Evidence: signalv1.EvidenceStatus{URI: "kubernetes://signal/configmaps/attempt-1/evidence.json.gz", Digest: "sha256:0123456789abcdef"},
				},
			},
		},
	}
	stages, issues, current := certificationTestStages(run, nil, map[string]string{"private-node": "gpu-node-1"}, fixedTime)
	require.Len(t, stages, 2)
	assert.Empty(t, issues)
	assert.Empty(t, current)
	assert.Equal(t, "Pass", stages[0].Verdict)
	assert.Equal(t, "Succeeded", stages[1].ExecutionState)
	assert.Equal(t, "Fail", stages[1].Verdict, "successful execution must not hide a diagnostic failure")
	assert.Equal(t, "gpu-node-1", stages[1].NodeAlias)
	assert.Equal(t, map[string]string{"cpu-workers": "8"}, stages[1].Parameters)
	assert.NotEmpty(t, stages[1].EvidenceURI)

	content, err := json.Marshal(stages)
	require.NoError(t, err)
	assert.NotContains(t, string(content), "private-node")
	assert.NotContains(t, string(content), "internal.invalid")
	assert.NotContains(t, string(content), "RAW-GPU-LOG-NOT-PUBLIC")
}

func TestCertificationTestStagesKeyResultsByNodeUIDAndAttempt(t *testing.T) {
	t.Parallel()

	left := signalv1.TestInvocation{ID: "gpu-health", NodeName: "node-left", NodeUID: "uid-left"}
	right := signalv1.TestInvocation{ID: "gpu-health", NodeName: "node-right", NodeUID: "uid-right"}
	run := &signalv1.CertificationRun{
		Spec: signalv1.CertificationRunSpec{TestPlan: []signalv1.TestInvocation{left, right}},
		Status: signalv1.CertificationRunStatus{
			Phase:    signalv1.RunPhaseFailed,
			Workflow: signalv1.WorkflowStatus{Attempt: 2},
			TestResults: []signalv1.TestResult{
				{TestInvocation: right, Attempt: 2, ExecutionState: "Succeeded", Verdict: "Fail", Reason: "RightAttempt2"},
				{TestInvocation: left, Attempt: 1, ExecutionState: "Succeeded", Verdict: "Fail", Reason: "LeftAttempt1"},
				{TestInvocation: right, Attempt: 1, ExecutionState: "Succeeded", Verdict: "Pass", Reason: "RightAttempt1"},
				{TestInvocation: left, Attempt: 2, ExecutionState: "Succeeded", Verdict: "Pass", Reason: "LeftAttempt2"},
			},
		},
	}
	stages, issues, current := certificationTestStages(run, nil, map[string]string{
		"node-left": "gpu-node-1", "node-right": "gpu-node-2",
	}, fixedTime)
	require.Len(t, stages, 4)
	assert.Empty(t, issues)
	assert.Empty(t, current)
	assert.Equal(t, []string{"LeftAttempt1", "RightAttempt1", "LeftAttempt2", "RightAttempt2"}, []string{
		stages[0].Reason, stages[1].Reason, stages[2].Reason, stages[3].Reason,
	})
	assert.NotEqual(t, stages[0].ID, stages[1].ID)
	assert.NotEqual(t, stages[0].ID, stages[2].ID)
}

func TestCertificationTestStagesDoNotInferMissingCoverageFromRootPass(t *testing.T) {
	t.Parallel()

	run := &signalv1.CertificationRun{
		Spec: signalv1.CertificationRunSpec{TestPlan: []signalv1.TestInvocation{{
			ID: "cpu-stress", NodeName: "node-1", NodeUID: "uid-1", ContainerName: "cpu-stress",
		}}},
		Status: signalv1.CertificationRunStatus{Phase: signalv1.RunPhaseSucceeded, Verdict: "Pass"},
	}
	stages, issues, current := certificationTestStages(run, nil, nil, fixedTime)
	require.Len(t, stages, 1)
	assert.Equal(t, "Omitted", stages[0].Phase)
	assert.Equal(t, "Inconclusive", stages[0].Verdict)
	assert.Equal(t, "MissingTestResult", stages[0].Reason)
	require.Len(t, issues, 1)
	assert.Empty(t, current)
}

func TestDashboardUsesRetainedTestResultsAfterWorkflowAndPodRetention(t *testing.T) {
	t.Parallel()

	reader := fixtureReader()
	reader.workflows, reader.mpiJobs, reader.pods = nil, nil, nil
	invocation := signalv1.TestInvocation{ID: "gpu-health", NodeName: "approved-a100", NodeUID: "node-uid"}
	reader.runs[0].Spec.TestPlan = []signalv1.TestInvocation{invocation}
	reader.runs[0].Status.Workflow.Attempt = 1
	reader.runs[0].Status.TestResults = []signalv1.TestResult{{
		TestInvocation: invocation, Attempt: 1, ExecutionState: "Succeeded", Verdict: "Pass", Reason: "FullNodeWorkloadPassed",
	}}
	logs := &fakePodLogs{err: errors.New("logs must not be needed")}
	collector := fixtureCollector(reader)
	collector.PodLogs = logs
	detail, err := collector.Run(t.Context(), collector.Collect(t.Context()), "run-example")
	require.NoError(t, err)
	require.Len(t, detail.ExecutionPlan, 1)
	assert.Equal(t, "Pass", detail.ExecutionPlan[0].Verdict)
	assert.Equal(t, "a100-node-1", detail.ExecutionPlan[0].NodeAlias)
	assert.Empty(t, detail.PlanIssues)
	assert.Empty(t, detail.WorkflowNodes)
	assert.Zero(t, logs.tailLines)
	assert.Equal(t, "Per-test certification group", detail.ArgoTask.Name)
	assert.Equal(t, "Pass", detail.ArgoTask.Verdict)
}

func TestDashboardGroupVerdictDoesNotCertifySuccessfulWorkflow(t *testing.T) {
	t.Parallel()

	reader := fixtureReader()
	gpu := signalv1.TestInvocation{ID: "gpu-burn", NodeName: "approved-a100", NodeUID: "node-uid", ContainerName: "main"}
	cpu := gpu
	cpu.ID, cpu.ContainerName = "cpu-stress", "cpu-stress"
	reader.runs[0].Spec.TestPlan = []signalv1.TestInvocation{gpu, cpu}
	reader.runs[0].Status.Phase = signalv1.RunPhaseFailed
	reader.runs[0].Status.Verdict = "Fail"
	reader.runs[0].Status.Reason = "HostStressFailed"
	reader.runs[0].Status.Workflow.Attempt = 1
	reader.runs[0].Status.TestResults = []signalv1.TestResult{
		{TestInvocation: gpu, Attempt: 1, ExecutionState: "Succeeded", Verdict: "Pass"},
		{TestInvocation: cpu, Attempt: 1, ExecutionState: "Succeeded", Verdict: "Fail", Reason: "HostStressFailed"},
	}
	collector := fixtureCollector(reader)
	detail, err := collector.Run(t.Context(), collector.Collect(t.Context()), "run-example")
	require.NoError(t, err)
	require.Len(t, detail.ExecutionPlan, 2)
	assert.Equal(t, "Succeeded", detail.ArgoTask.ExecutionState)
	assert.Equal(t, "Fail", detail.ArgoTask.Verdict)
	assert.Equal(t, "HostStressFailed", detail.ArgoTask.Reason)
	assert.Equal(t, "Pass", detail.ExecutionPlan[0].Verdict)
	assert.Equal(t, "Fail", detail.ExecutionPlan[1].Verdict)
}

func TestCertificationTestStagesShowConcurrentContainerNodesWithoutPass(t *testing.T) {
	t.Parallel()

	run := &signalv1.CertificationRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-example", Namespace: "signal", UID: "run-uid"},
		Spec: signalv1.CertificationRunSpec{
			Script:       signalv1.ScriptSnapshot{Digest: "sha256:test-script"},
			Workflow:     signalv1.WorkflowReference{Name: "project-signal-gpu-burn-canary"},
			Participants: []signalv1.Participant{{NodeName: "node-1", NodeUID: "node-uid"}},
			ParticipantPlan: []signalv1.ParticipantSnapshot{{
				NodeName: "node-1", NodeUID: "node-uid", ObservedGPUCapacity: "8", ObservedGPUAllocatable: "8",
			}},
			Parameters: map[string]string{"observed-gpu-capacity": "8", "observed-gpu-allocatable": "8"},
		},
		Status: signalv1.CertificationRunStatus{
			Phase: signalv1.RunPhaseRunning,
			Workflow: signalv1.WorkflowStatus{
				Name: "workflow-example", UID: "workflow-uid", Attempt: 1,
			},
		},
	}

	parameters := []interface{}{}
	for name, value := range map[string]string{
		"test-id": "mixed-burn", "node-name": "node-1", "node-uid": "node-uid",
		"run-uid": "run-uid", "attempt": "1", "script-digest": "sha256:test-script",
	} {
		parameters = append(parameters, map[string]interface{}{"name": name, "value": value})
	}

	parent := map[string]interface{}{
		"id": "burn-pod", "name": "workflow-example.mixed-burn", "type": "Pod", "phase": "Running",
		"displayName": "mixed-burn", "hostNodeName": "node-1",
		"inputs": map[string]interface{}{"parameters": parameters},
	}
	nodes := map[string]interface{}{"burn-pod": parent}
	children := []interface{}{}

	for _, test := range []struct{ id, container string }{
		{"gpu-burn", "main"},
		{"cpu-stress", "cpu-stress"},
		{"memory-integrity", "memory-integrity"},
		{"storage-integrity", "storage-integrity"},
	} {
		run.Spec.TestPlan = append(run.Spec.TestPlan, signalv1.TestInvocation{
			ID: test.id, NodeName: "node-1", NodeUID: "node-uid", ContainerName: test.container,
			Parameters: map[string]string{"observed-gpu-capacity": "8", "observed-gpu-allocatable": "8"},
		})
		id := "container-" + test.container
		children = append(children, id)
		nodes[id] = map[string]interface{}{
			"id": id, "name": "workflow-example.mixed-burn." + test.container,
			"type": "Container", "boundaryID": "burn-pod", "phase": "Running",
			"startedAt": fixedTime.Add(-time.Minute).Format(time.RFC3339),
		}
	}

	parent["children"] = children
	workflow := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Workflow",
		"metadata": map[string]interface{}{
			"name": "workflow-example", "namespace": run.Namespace, "uid": "workflow-uid",
		},
		"status": map[string]interface{}{"phase": "Running", "nodes": nodes},
	}}
	workflow.SetOwnerReferences([]metav1.OwnerReference{
		*metav1.NewControllerRef(run, signalv1.GroupVersion.WithKind("CertificationRun")),
	})

	stages, issues, current := certificationTestStages(run, workflow, map[string]string{"node-1": "gpu-node-1"}, fixedTime)
	require.Len(t, stages, 4)
	assert.Empty(t, issues)
	assert.Equal(t, "test-1-a1, test-2-a1, test-3-a1, test-4-a1", current)

	for _, stage := range stages {
		assert.Equal(t, "Running", stage.ExecutionState)
		assert.Empty(t, stage.Verdict, "running a test is not a certification pass")
		assert.Equal(t, "1m0s", stage.Duration)
	}
}

func TestCertificationTestStagesBoundInterruptedDurationAndPreserveVerdict(t *testing.T) {
	t.Parallel()

	invocation := signalv1.TestInvocation{
		ID: "cpu-stress", NodeName: "node-1", NodeUID: "node-uid", ContainerName: "cpu-stress",
	}
	finished := metav1.NewTime(fixedTime.Add(-30 * time.Second))
	run := &signalv1.CertificationRun{
		Spec: signalv1.CertificationRunSpec{TestPlan: []signalv1.TestInvocation{invocation}},
		Status: signalv1.CertificationRunStatus{
			Phase: signalv1.RunPhaseFailed, FinishedAt: &finished,
			Workflow: signalv1.WorkflowStatus{Attempt: 1},
			TestResults: []signalv1.TestResult{{
				TestInvocation: invocation, Attempt: 1, ContainerNodeID: "cpu-container",
				ExecutionState: "Running", Verdict: "Inconclusive", Reason: "RunCanceled",
			}},
		},
	}
	workflow := &unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{"nodes": map[string]interface{}{
			"cpu-container": map[string]interface{}{
				"id": "cpu-container", "name": "workflow-example.mixed-burn.cpu-stress",
				"type": "Container", "phase": "Running",
				"startedAt": fixedTime.Add(-2 * time.Minute).Format(time.RFC3339),
			},
		}},
	}}

	stages, issues, current := certificationTestStages(run, workflow, nil, fixedTime.Add(time.Hour))
	require.Len(t, stages, 1)
	assert.Empty(t, issues)
	assert.Empty(t, current)
	assert.Equal(t, "Inconclusive", stages[0].Verdict)
	assert.Equal(t, "RunCanceled", stages[0].Reason)
	assert.Equal(t, "1m30s", stages[0].Duration, "interrupted tests must not appear to run indefinitely")
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
