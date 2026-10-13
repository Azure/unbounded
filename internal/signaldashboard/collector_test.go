// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaldashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
	"github.com/Azure/unbounded/internal/signal"
)

var fixedTime = time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)

type fakeReader struct {
	runs       []signalv1.CertificationRun
	profiles   []signalv1.CertificationProfile
	workflows  []unstructured.Unstructured
	mpiJobs    []unstructured.Unstructured
	pods       []corev1.Pod
	nodes      map[string]corev1.Node
	listErrors map[string]error
	gets       []string
}

func (f *fakeReader) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	switch typed := list.(type) {
	case *signalv1.CertificationRunList:
		if err := f.listErrors["CertificationRun"]; err != nil {
			return err
		}

		typed.Items = append([]signalv1.CertificationRun(nil), f.runs...)
	case *signalv1.CertificationProfileList:
		if err := f.listErrors["CertificationProfile"]; err != nil {
			return err
		}

		typed.Items = append([]signalv1.CertificationProfile(nil), f.profiles...)
	case *unstructured.UnstructuredList:
		switch typed.GroupVersionKind().Kind {
		case "WorkflowList":
			if err := f.listErrors["Workflow"]; err != nil {
				return err
			}

			typed.Items = append([]unstructured.Unstructured(nil), f.workflows...)
		case "MPIJobList":
			if err := f.listErrors["MPIJob"]; err != nil {
				return err
			}

			typed.Items = append([]unstructured.Unstructured(nil), f.mpiJobs...)
		}
	case *corev1.PodList:
		if err := f.listErrors["Pod"]; err != nil {
			return err
		}

		typed.Items = append([]corev1.Pod(nil), f.pods...)
	}

	return nil
}

func (f *fakeReader) Get(_ context.Context, key client.ObjectKey, object client.Object, _ ...client.GetOption) error {
	f.gets = append(f.gets, key.Name)

	node, ok := f.nodes[key.Name]
	if !ok {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "nodes"}, key.Name)
	}

	*object.(*corev1.Node) = *node.DeepCopy()

	return nil
}

func TestCollectorCorrelatesWorkflowAndMPIJobByOwnership(t *testing.T) {
	t.Parallel()

	reader := fixtureReader()
	collector := fixtureCollector(reader)
	snapshot := collector.Collect(context.Background())
	detail, err := collector.Run(context.Background(), snapshot, "run-example")
	require.NoError(t, err)

	assert.Equal(t, "workflow-example", detail.WorkflowName)
	assert.Equal(t, "Succeeded", detail.WorkflowPhase)
	require.NotNil(t, detail.MPIJob)
	assert.Equal(t, "mpi-example", detail.MPIJob.Name)
	assert.Equal(t, "Succeeded", detail.MPIJob.Phase)
	assert.Equal(t, "Pass", detail.Outputs["verdict"])
	assert.Equal(t, "hardware-pass", detail.OutcomeType)
	assert.Equal(t, []string{"a100-node-1"}, detail.NodeAliases)
	assert.Len(t, detail.WorkflowNodes, 2)
	assert.True(t, detail.Evidence.Retained)
	assert.Equal(t, "kubernetes://project-signal/configmaps/run-example-evidence/evidence.json.gz", detail.Evidence.URI)
	assert.Equal(t, "sha256:evidence", detail.Evidence.Digest)
}

func TestOutcomeTypeSeparatesHardwareAndInfrastructureFailures(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "hardware-failure", outcomeType(signalv1.RunPhaseFailed, "Completed", "Fail"))
	assert.Equal(t, "hardware-failure", outcomeType(signalv1.RunPhaseFailed, "Completed", "Suspect"))
	assert.Equal(t, "infrastructure-failure", outcomeType(signalv1.RunPhaseFailed, "WorkflowFailed", ""))
	assert.Equal(t, "inconclusive", outcomeType(signalv1.RunPhaseInconclusive, "Completed", ""))
	assert.Equal(t, "hardware-pass", outcomeType(signalv1.RunPhaseSucceeded, "Completed", "Pass"))
}

func TestWorkflowForRunPrefersRecordedAttempt(t *testing.T) {
	t.Parallel()

	run := &signalv1.CertificationRun{ObjectMeta: metav1.ObjectMeta{Name: "run-1", UID: "run-uid"}}
	run.Status.Workflow.UID = "current-workflow"

	previous := unstructured.Unstructured{}
	previous.SetUID("previous-workflow")
	previous.SetOwnerReferences([]metav1.OwnerReference{{UID: run.UID}})

	current := unstructured.Unstructured{}
	current.SetUID("current-workflow")
	current.SetOwnerReferences([]metav1.OwnerReference{{UID: run.UID}})

	got := workflowForRun(run, []unstructured.Unstructured{previous, current})
	assert.NotNil(t, got)
	assert.Equal(t, types.UID("current-workflow"), got.GetUID())
}

func TestRunTriggerUsesControllerAnnotation(t *testing.T) {
	t.Parallel()

	run := &signalv1.CertificationRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "run-example",
			Annotations: map[string]string{signal.RunTriggerAnnotation: "recurring"},
		},
	}
	assert.Equal(t, "recurring", runTrigger(run, nil))

	run.Annotations = nil
	assert.Equal(t, "manual", runTrigger(run, nil))

	profile := signalv1.CertificationProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "profile"},
		Status: signalv1.CertificationProfileStatus{
			Nodes: []signalv1.RecurringNodeStatus{{LastSuccessfulRun: run.Name}},
		},
	}
	run.Spec.Profile.Name = profile.Name
	assert.Equal(t, "recurring", runTrigger(run, []signalv1.CertificationProfile{profile}))
}

func TestCollectorGetsOnlyNodesReferencedByProfilesOrRuns(t *testing.T) {
	t.Parallel()

	reader := fixtureReader()
	reader.nodes["unrelated-node"] = corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "unrelated-node"}}
	collector := fixtureCollector(reader)
	snapshot := collector.Collect(context.Background())

	assert.Equal(t, []string{"approved-a100"}, reader.gets)
	assert.Contains(t, snapshot.nodes, "approved-a100")
	assert.NotContains(t, snapshot.nodes, "unrelated-node")
}

func TestNodeSummaryUsesStatusForProjectedProfile(t *testing.T) {
	t.Parallel()

	reader := fixtureReader()
	otherDue := metav1.NewTime(fixedTime.Add(24 * time.Hour))
	reader.profiles = append(reader.profiles, signalv1.CertificationProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "other-profile"},
		Status: signalv1.CertificationProfileStatus{
			Nodes: []signalv1.RecurringNodeStatus{{
				NodeName:  "approved-a100",
				NextDueAt: &otherDue,
				Reason:    "IdleWindow",
			}},
		},
	})

	nodes := fixtureCollector(reader).Nodes(fixtureCollector(reader).Collect(context.Background()))
	require.Len(t, nodes.Items, 1)
	assert.Equal(t, "a100-smoke", nodes.Items[0].Profile)
	require.NotNil(t, nodes.Items[0].NextDueAt)
	assert.Equal(t, fixedTime.Add(time.Hour), *nodes.Items[0].NextDueAt)
	assert.Equal(t, "NotDue", nodes.Items[0].RecurringReason)
}

func TestSummarySeparatesCurrentHealthFromRetainedHistory(t *testing.T) {
	t.Parallel()

	reader := fixtureReader()
	active := reader.runs[0]
	active.Name = "active-full"
	active.Spec.Profile.Name = "a100-full"
	active.Spec.Script.Key = "full.sh"
	active.Status = signalv1.CertificationRunStatus{Phase: signalv1.RunPhaseRunning, StartedAt: ptrMetaTime(metav1.NewTime(fixedTime.Add(-time.Hour)))}

	failed := reader.runs[0]
	failed.Name = "old-failure"
	failed.CreationTimestamp = metav1.NewTime(fixedTime.Add(-2 * time.Hour))
	failed.Status = signalv1.CertificationRunStatus{Phase: signalv1.RunPhaseFailed, ExecutionState: "Completed", Verdict: "Fail"}

	diagnostic := failed
	diagnostic.Name = "storage-diagnostic"
	diagnostic.Spec.Profile.Name = "storage-diagnostic"
	diagnostic.Status.Reason = "StorageDiagnosticOnly"
	reader.runs = []signalv1.CertificationRun{active, failed, diagnostic}
	node := reader.nodes["approved-a100"]
	node.Labels[signal.StateLabel] = "testing"
	node.Labels[signal.EligibilityLabel] = "blocked"
	reader.nodes["approved-a100"] = node

	collector := fixtureCollector(reader)
	summary := collector.Summary(collector.Collect(context.Background()))
	assert.Equal(t, 1, summary.Active.Running)
	assert.Equal(t, 1, summary.CurrentNodes.Blocked)
	assert.Equal(t, 1, summary.RetainedHistory.Failed)
}

func TestViewFilterSeparatesCurrentHistoricalDiagnosticsAndSuperseded(t *testing.T) {
	t.Parallel()

	reader := fixtureReader()
	active := reader.runs[0]
	active.Name = "active"
	active.Status.Phase = signalv1.RunPhaseRunning

	old := reader.runs[0]
	old.Name = "old"
	old.CreationTimestamp = metav1.NewTime(fixedTime.Add(-time.Hour))
	old.Status = signalv1.CertificationRunStatus{Phase: signalv1.RunPhaseFailed, Verdict: "Fail"}

	passed := old
	passed.Name = "passed"
	passed.CreationTimestamp = metav1.NewTime(fixedTime)
	passed.Status = signalv1.CertificationRunStatus{Phase: signalv1.RunPhaseSucceeded, Verdict: "Pass"}

	diagnostic := old
	diagnostic.Name = "diagnostic"
	diagnostic.Spec.Purpose = signalv1.PurposeDiagnostic

	snapshot := collected{runs: []signalv1.CertificationRun{active, old, passed, diagnostic}}

	cases := map[string]string{
		"Current":     "active",
		"Historical":  "passed",
		"Diagnostics": "diagnostic",
		"Superseded":  "old",
	}
	for view, want := range cases {
		request := httptest.NewRequest(http.MethodGet, "/api/runs?view="+view, nil)
		filtered := filterRuns(snapshot, request)
		require.Len(t, filtered.runs, 1, view)
		assert.Equal(t, want, filtered.runs[0].Name, view)
	}
}

func TestSanitizeDisplayRedactsUUIDsAndBoundsMessages(t *testing.T) {
	t.Parallel()

	value := sanitizeDisplay("device GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee id 11111111-2222-4333-8444-555555555555 endpoint https://private.example/path digest " + strings.Repeat("a", 64))
	assert.NotContains(t, value, "GPU-aaaaaaaa")
	assert.NotContains(t, value, strings.Repeat("a", 64))
	assert.NotContains(t, value, "private.example")
	assert.Contains(t, value, "GPU-[redacted]")
	assert.Contains(t, value, "[id-redacted]")
	assert.Contains(t, value, "[endpoint-redacted]")

	long := sanitizeDisplay(strings.Repeat("x", maxDisplayValue+50))
	assert.LessOrEqual(t, len(long), maxDisplayValue+len("...[truncated]"))
	assert.Contains(t, long, "[truncated]")
}

func TestRunsPaginationIsBounded(t *testing.T) {
	t.Parallel()

	reader := fixtureReader()

	reader.runs = make([]signalv1.CertificationRun, 150)
	for i := range reader.runs {
		reader.runs[i].Name = "run-" + strings.Repeat("x", i%4)
	}

	collector := fixtureCollector(reader)
	snapshot := collector.Collect(context.Background())
	page := collector.Runs(snapshot, -3, 1000)

	assert.Equal(t, 0, page.Offset)
	assert.Equal(t, maxPageLimit, page.Limit)
	assert.Len(t, page.Items, maxPageLimit)
	assert.Equal(t, 150, page.Total)
}

func TestReadyAndAPIsSurfacePartialErrors(t *testing.T) {
	t.Parallel()

	reader := fixtureReader()
	reader.listErrors["Workflow"] = errors.New("workflow API unavailable")
	server := &Server{Collector: fixtureCollector(reader)}
	handler := server.Handler()

	ready := httptest.NewRecorder()
	handler.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusServiceUnavailable, ready.Code)

	summaryResponse := httptest.NewRecorder()
	handler.ServeHTTP(summaryResponse, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	assert.Equal(t, http.StatusOK, summaryResponse.Code)

	var summary Summary
	require.NoError(t, json.Unmarshal(summaryResponse.Body.Bytes(), &summary))
	assert.True(t, summary.Partial)
	require.NotEmpty(t, summary.Errors)
	assert.Equal(t, "Workflow", summary.Errors[0].Resource)
	assert.Equal(t, "unknown", summary.Argo.Status)
	assert.Equal(t, "nosniff", summaryResponse.Header().Get("X-Content-Type-Options"))
	assert.Contains(t, summaryResponse.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'")
}

func TestReasonExplanationsCoverPolicyStates(t *testing.T) {
	t.Parallel()

	for _, reason := range []string{
		"NotDue",
		"IdleWindow",
		"ConcurrencyLimit",
		"ExclusiveProfileActive",
		"Backoff",
		"CircuitOpen",
		"Expired",
		"WorkflowFailed",
		"PodFailed",
		"APIFailure",
	} {
		assert.NotEmpty(t, explainReason(reason), reason)
	}
}

func TestRunEndpointRejectsUnknownRun(t *testing.T) {
	t.Parallel()

	server := &Server{Collector: fixtureCollector(fixtureReader())}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/runs/missing", nil))
	assert.Equal(t, http.StatusNotFound, response.Code)
}

func TestRunsEndpointFiltersWithoutChangingNodeScope(t *testing.T) {
	t.Parallel()

	server := &Server{Collector: fixtureCollector(fixtureReader())}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/runs?phase=Failed&limit=1000", nil))
	assert.Equal(t, http.StatusOK, response.Code)

	var runs RunList
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &runs))
	assert.Empty(t, runs.Items)
	assert.Equal(t, 0, runs.Total)
	assert.Equal(t, maxPageLimit, runs.Limit)
}

func fixtureCollector(reader *fakeReader) *Collector {
	return &Collector{
		Reader:    reader,
		Namespace: "project-signal",
		Now:       func() time.Time { return fixedTime },
	}
}

func fixtureReader() *fakeReader {
	runUID := types.UID("run-uid")
	workflowUID := types.UID("workflow-uid")
	started := metav1.NewTime(fixedTime.Add(-2 * time.Minute))
	finished := metav1.NewTime(fixedTime.Add(-time.Minute))

	run := signalv1.CertificationRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "run-example",
			Namespace:         "project-signal",
			UID:               runUID,
			CreationTimestamp: metav1.NewTime(fixedTime.Add(-3 * time.Minute)),
		},
		Spec: signalv1.CertificationRunSpec{
			Profile:      signalv1.PinnedProfile{Name: "a100-smoke"},
			Participants: []signalv1.Participant{{NodeName: "approved-a100"}},
			Script:       signalv1.ScriptSnapshot{Digest: "sha256:0123456789abcdef"},
		},
		Status: signalv1.CertificationRunStatus{
			Phase:          signalv1.RunPhaseSucceeded,
			ExecutionState: "Completed",
			Verdict:        "Pass",
			Reason:         "Healthy",
			Workflow:       signalv1.WorkflowStatus{UID: string(workflowUID), Attempt: 1},
			Cleanup:        signalv1.CleanupStatus{ReservationsReleased: true, SnapshotDeleted: true},
			Evidence: signalv1.EvidenceStatus{
				URI:    "kubernetes://project-signal/configmaps/run-example-evidence/evidence.json.gz",
				Digest: "sha256:evidence",
			},
			StartedAt:  &started,
			FinishedAt: &finished,
		},
	}

	profile := signalv1.CertificationProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "a100-smoke"},
		Spec: signalv1.CertificationProfileSpec{
			Recurring: &signalv1.RecurringPolicy{
				Enabled:   true,
				NodeNames: []string{"approved-a100"},
			},
		},
		Status: signalv1.CertificationProfileStatus{
			Nodes: []signalv1.RecurringNodeStatus{{
				NodeName:  "approved-a100",
				NextDueAt: ptrMetaTime(metav1.NewTime(fixedTime.Add(time.Hour))),
				Reason:    "NotDue",
			}},
		},
	}

	workflow := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Workflow",
		"metadata": map[string]any{
			"name":      "workflow-example",
			"namespace": "project-signal",
			"uid":       string(workflowUID),
			"ownerReferences": []any{
				map[string]any{"apiVersion": signalv1.GroupVersion.String(), "kind": "CertificationRun", "name": run.Name, "uid": string(runUID)},
			},
		},
		"status": map[string]any{
			"phase": "Succeeded",
			"nodes": map[string]any{
				"node-health-id": map[string]any{"displayName": "node-health", "templateName": "node-health", "type": "Pod", "phase": "Succeeded"},
				"mpi-id":         map[string]any{"displayName": "mpi-backend", "templateName": "mpi-backend", "type": "Resource", "phase": "Succeeded"},
			},
			"outputs": map[string]any{
				"parameters": []any{map[string]any{"name": "verdict", "value": "Pass"}},
			},
		},
	}}

	mpiJob := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubeflow.org/v2beta1",
		"kind":       "MPIJob",
		"metadata": map[string]any{
			"name":      "mpi-example",
			"namespace": "project-signal",
			"ownerReferences": []any{
				map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "Workflow", "name": workflow.GetName(), "uid": string(workflowUID)},
			},
		},
		"status": map[string]any{
			"conditions": []any{map[string]any{"type": "Succeeded", "status": "True", "reason": "Completed"}},
			"replicaStatuses": map[string]any{
				"Launcher": map[string]any{"succeeded": int64(1)},
				"Worker":   map[string]any{"succeeded": int64(1)},
			},
		},
	}}

	node := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "approved-a100",
			Labels: map[string]string{
				"nvidia.com/gpu.product": "NVIDIA-A100",
				signal.StateLabel:        "passed",
				signal.EligibilityLabel:  "production",
			},
			Annotations: map[string]string{
				signal.ProfileNameAnnotation: "a100-smoke",
			},
		},
	}

	readyPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "controller-example",
			Labels: map[string]string{"app.kubernetes.io/name": "project-signal-controller"},
		},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
	argoPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "argo-example",
			Labels: map[string]string{"app.kubernetes.io/name": "workflow-controller"},
		},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}

	return &fakeReader{
		runs:       []signalv1.CertificationRun{run},
		profiles:   []signalv1.CertificationProfile{profile},
		workflows:  []unstructured.Unstructured{workflow},
		mpiJobs:    []unstructured.Unstructured{mpiJob},
		pods:       []corev1.Pod{readyPod, argoPod},
		nodes:      map[string]corev1.Node{"approved-a100": node},
		listErrors: map[string]error{},
	}
}

func ptrMetaTime(value metav1.Time) *metav1.Time {
	return &value
}
