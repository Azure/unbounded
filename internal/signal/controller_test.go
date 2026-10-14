// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func TestAdmissionCreatesImmutableSnapshotAndPreventsDuplicate(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	node := readyNode("gpu-0", "example-gpu")
	node.Labels[RequestLabel] = "gpu-smoke"
	profile := testProfile("gpu-smoke", "example-gpu")

	const payload = "test payload\n"

	source := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "scripts", Namespace: "signal"},
		Data:       map[string]string{"payload.txt": payload},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, profile, source).Build()
	now := time.Unix(100, 0)
	r := &AdmissionReconciler{Client: kube, Namespace: "signal", ScriptNamespace: "signal", Now: func() time.Time { return now }}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKey{Name: node.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKey{Name: node.Name}}); err != nil {
		t.Fatalf("duplicate Reconcile: %v", err)
	}

	var runs signalv1.CertificationRunList
	if err := kube.List(context.Background(), &runs); err != nil {
		t.Fatalf("list runs: %v", err)
	}

	if len(runs.Items) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs.Items))
	}

	run := runs.Items[0]
	if run.Spec.Script.Digest != digestString(payload) {
		t.Fatalf("script digest = %q", run.Spec.Script.Digest)
	}

	if len(run.Spec.StagePlan) != 1 || run.Spec.StagePlan[0].ID != "workload" {
		t.Fatalf("stage plan = %#v, want immutable profile plan", run.Spec.StagePlan)
	}

	if run.Spec.Purpose != signalv1.PurposeQuickCertification {
		t.Fatalf("purpose = %q, want %q", run.Spec.Purpose, signalv1.PurposeQuickCertification)
	}

	if run.Annotations[RunTriggerAnnotation] != "manual" {
		t.Fatalf("trigger = %q, want manual", run.Annotations[RunTriggerAnnotation])
	}

	var snapshot corev1.ConfigMap
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "signal", Name: run.Spec.Script.ConfigMapName}, &snapshot); err != nil {
		t.Fatalf("get snapshot: %v", err)
	}

	if snapshot.Immutable == nil || !*snapshot.Immutable {
		t.Fatal("snapshot is mutable")
	}

	if snapshot.Data["payload.txt"] != payload {
		t.Fatalf("snapshot payload = %q", snapshot.Data["payload.txt"])
	}

	var locked corev1.Node
	if err := kube.Get(context.Background(), client.ObjectKey{Name: node.Name}, &locked); err != nil {
		t.Fatalf("get locked node: %v", err)
	}

	if locked.Annotations[ReservationLabel] != run.Name {
		t.Fatalf("reservation = %q, want %q", locked.Annotations[ReservationLabel], run.Name)
	}
}

func TestParticipantSelectionRejectsUnsupportedMultiNode(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	request := readyNode("gpu-0", "H200")
	peer := readyNode("gpu-1", "H200")
	locked := readyNode("gpu-2", "H200")
	locked.Annotations = map[string]string{ReservationLabel: "other-run"}
	wrong := readyNode("gpu-3", "A100")
	profile := testProfile("h200-pair", "H200")
	profile.Spec.Participants.Count = 2
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(request, peer, locked, wrong).Build()
	r := &AdmissionReconciler{Client: kube}

	if _, err := r.selectParticipants(context.Background(), request, profile); err == nil ||
		!strings.Contains(err.Error(), "multi-participant execution is not implemented") {
		t.Fatalf("selectParticipants error = %v", err)
	}
}

func TestAdmissionFailureMakesCreatedRunInertAndReleasesReservation(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name           string
		failSnapshot   bool
		failProjection bool
	}{
		{name: "snapshot binding", failSnapshot: true},
		{name: "pending projection", failProjection: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			scheme := testScheme(t)
			node := readyNode("gpu-0", "example-gpu")
			node.Labels[RequestLabel] = "gpu-smoke"
			profile := testProfile("gpu-smoke", "example-gpu")
			source := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "scripts", Namespace: "signal"},
				Data:       map[string]string{"payload.txt": "test payload\n"},
			}
			kube := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(node, profile, source).
				WithInterceptorFuncs(interceptor.Funcs{
					Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
						if snapshot, ok := obj.(*corev1.ConfigMap); ok &&
							tc.failSnapshot && snapshot.Name != source.Name {
							return errors.New("snapshot update failed")
						}

						return c.Update(ctx, obj, opts...)
					},
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						if projected, ok := obj.(*corev1.Node); ok &&
							tc.failProjection && projected.Labels[StateLabel] == "pending" {
							return errors.New("projection patch failed")
						}

						return c.Patch(ctx, obj, patch, opts...)
					},
				}).
				Build()
			r := &AdmissionReconciler{
				Client:          kube,
				Namespace:       "signal",
				ScriptNamespace: "signal",
				Now:             func() time.Time { return time.Unix(100, 0) },
			}

			if _, err := r.Reconcile(
				context.Background(),
				reconcile.Request{NamespacedName: client.ObjectKey{Name: node.Name}},
			); err == nil {
				t.Fatal("Reconcile error = nil, want failed admission")
			}

			var runs signalv1.CertificationRunList
			if err := kube.List(context.Background(), &runs); err != nil {
				t.Fatalf("list runs: %v", err)
			}

			for i := range runs.Items {
				if runs.Items[i].DeletionTimestamp.IsZero() {
					t.Fatalf("run %s remained active after failed admission", runs.Items[i].Name)
				}
			}

			var cleaned corev1.Node
			if err := kube.Get(context.Background(), client.ObjectKey{Name: node.Name}, &cleaned); err != nil {
				t.Fatalf("get cleaned node: %v", err)
			}

			if cleaned.Annotations[ReservationLabel] != "" {
				t.Fatalf("reservation remained after inerting failed run: %q", cleaned.Annotations[ReservationLabel])
			}
		})
	}
}

func TestDeletingRunClearsReservationAndRequest(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	run := testRun("deleting-run")
	run.Finalizers = []string{RunFinalizer}
	node := readyNode(run.Spec.RequestNodeName, "A100")
	node.Labels[RequestLabel] = run.Spec.Profile.Name
	node.Annotations = map[string]string{ReservationLabel: run.Name}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(run, node).Build()
	reconciler := &RunReconciler{Client: kube}

	if _, err := reconciler.reconcileDeleting(context.Background(), run); err != nil {
		t.Fatalf("reconcileDeleting: %v", err)
	}

	var cleaned corev1.Node
	if err := kube.Get(context.Background(), client.ObjectKey{Name: node.Name}, &cleaned); err != nil {
		t.Fatalf("get cleaned node: %v", err)
	}

	if cleaned.Annotations[ReservationLabel] != "" || cleaned.Labels[RequestLabel] != "" {
		t.Fatalf("deletion cleanup left trigger metadata: labels=%v annotations=%v", cleaned.Labels, cleaned.Annotations)
	}
}

func TestCancellationWaitsForWorkflowDeletionBeforeReleasingNode(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	run := testRun("canceling-run")
	run.Spec.Cancel = true
	run.Status.Phase = signalv1.RunPhaseRunning
	run.Status.Workflow = signalv1.WorkflowStatus{Name: "canceling-run-a1", Attempt: 1}
	node := readyNode(run.Spec.RequestNodeName, "A100")
	node.Annotations = map[string]string{ReservationLabel: run.Name}
	workflow := workflowObject(run.Namespace, run.Status.Workflow.Name, "Running", "", "")
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run, node, workflow).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(
				_ context.Context,
				_ client.WithWatch,
				_ client.Object,
				_ ...client.DeleteOption,
			) error {
				return nil
			},
		}).
		Build()
	reconciler := &RunReconciler{Client: kube}

	result, err := reconciler.Reconcile(
		context.Background(),
		reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)},
	)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if result.RequeueAfter == 0 {
		t.Fatal("cancellation did not wait for Workflow deletion")
	}

	var gotRun signalv1.CertificationRun
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), &gotRun); err != nil {
		t.Fatal(err)
	}

	if gotRun.Status.Phase.Terminal() {
		t.Fatalf("run became terminal before Workflow deletion: %#v", gotRun.Status)
	}

	var gotNode corev1.Node
	if err := kube.Get(context.Background(), client.ObjectKey{Name: node.Name}, &gotNode); err != nil {
		t.Fatal(err)
	}

	if gotNode.Annotations[ReservationLabel] != run.Name {
		t.Fatalf("reservation was released before Workflow deletion: %v", gotNode.Annotations)
	}
}

func TestWorkflowPinsAttemptInputs(t *testing.T) {
	t.Parallel()

	run := testRun("run-1")
	run.UID = types.UID("run-uid")
	run.Spec.Tolerations = []corev1.Toleration{{
		Key:      "nvidia.com/gpu",
		Operator: corev1.TolerationOpExists,
		Effect:   corev1.TaintEffectNoSchedule,
	}}
	run.Spec.Parameters = map[string]string{
		ObservedGPUCapacityParameter:    "8",
		ObservedGPUAllocatableParameter: "7",
		"burn-duration-seconds":         "21600",
		"sample-interval-seconds":       "60",
	}

	workflow, err := buildWorkflow(run, 2)
	if err != nil {
		t.Fatal(err)
	}

	if workflow.GetName() != "run-1-a2" {
		t.Fatalf("workflow name = %q", workflow.GetName())
	}

	if got, _, _ := unstructured.NestedInt64(workflow.Object, "spec", "ttlStrategy", "secondsAfterCompletion"); got != 21600 {
		t.Fatalf("workflow completion TTL = %d, want 21600", got)
	}

	if got, _, _ := unstructured.NestedString(workflow.Object, "spec", "podGC", "deleteDelayDuration"); got != "2h" {
		t.Fatalf("workflow pod GC delay = %q, want 2h", got)
	}

	if len(workflow.GetOwnerReferences()) != 1 || workflow.GetOwnerReferences()[0].UID != run.UID {
		t.Fatalf("owner references = %#v", workflow.GetOwnerReferences())
	}

	parameters, found, err := unstructured.NestedSlice(workflow.Object, "spec", "arguments", "parameters")
	if err != nil || !found {
		t.Fatalf("workflow parameters: found=%t err=%v", found, err)
	}

	values := map[string]string{}

	for _, raw := range parameters {
		parameter := raw.(map[string]any)
		values[parameter["name"].(string)] = parameter["value"].(string)
	}

	if values["script-digest"] != run.Spec.Script.Digest || values["backend-kind"] != "MPIJob" || values["attempt"] != "2" {
		t.Fatalf("workflow values = %#v", values)
	}

	if values["observed-gpu-capacity"] != "8" || values["observed-gpu-allocatable"] != "7" {
		t.Fatalf("workflow observed inventory = %#v", values)
	}

	if values["burn-duration-seconds"] != "21600" || values["sample-interval-seconds"] != "60" {
		t.Fatalf("workflow profile parameters = %#v", values)
	}

	if values["script-configmap"] != run.Spec.Script.ConfigMapName || values["script-configmap"] == "scripts" {
		t.Fatalf("workflow must mount the immutable snapshot, values = %#v", values)
	}

	if !strings.Contains(values["tolerations"], `"key":"nvidia.com/gpu"`) {
		t.Fatalf("workflow tolerations = %q", values["tolerations"])
	}

	var tolerations []corev1.Toleration
	if err := json.Unmarshal([]byte(values["tolerations"]), &tolerations); err != nil {
		t.Fatalf("decode workflow tolerations: %v", err)
	}

	if len(tolerations) != 1 {
		t.Fatalf("workflow tolerations = %#v, want one deduplicated GPU toleration", tolerations)
	}
}

func TestSnapshotParametersRecordsControllerObservedGPUInventory(t *testing.T) {
	t.Parallel()

	node := readyNode("gpu-0", "gpu")
	node.Status.Capacity = corev1.ResourceList{corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("8")}
	node.Status.Allocatable = corev1.ResourceList{corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("7")}

	got := snapshotParameters(map[string]string{
		"expected-gpu-count":            "8",
		ObservedGPUCapacityParameter:    "999",
		ObservedGPUAllocatableParameter: "999",
	}, node)

	if got["expected-gpu-count"] != "8" {
		t.Fatalf("expected parameter was not preserved: %#v", got)
	}

	if got[ObservedGPUCapacityParameter] != "8" || got[ObservedGPUAllocatableParameter] != "7" {
		t.Fatalf("controller-observed inventory did not override profile values: %#v", got)
	}
}

func TestStartAttemptRecoversExistingOwnedWorkflow(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	run := testRun("run-recovered")

	workflow, err := buildWorkflow(run, 1)
	if err != nil {
		t.Fatal(err)
	}

	workflow.SetUID("workflow-uid")
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run, workflow).
		Build()
	r := &RunReconciler{Client: kube, Evidence: &memoryEvidenceStore{}}

	if _, err := r.startAttempt(context.Background(), run, 1); err != nil {
		t.Fatalf("startAttempt: %v", err)
	}

	var got signalv1.CertificationRun
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), &got); err != nil {
		t.Fatalf("get run: %v", err)
	}

	if got.Status.Workflow.Name != workflow.GetName() ||
		got.Status.Workflow.UID != string(workflow.GetUID()) ||
		got.Status.Workflow.Attempt != 1 {
		t.Fatalf("workflow status = %#v", got.Status.Workflow)
	}
}

func TestWorkflowParameterFallsBackToEntrypointNodeOutputs(t *testing.T) {
	t.Parallel()

	workflow := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "run-a1"},
		"status": map[string]any{
			"nodes": map[string]any{
				"root": map[string]any{
					"displayName": "run-a1",
					"outputs": map[string]any{
						"parameters": []any{map[string]any{"name": "verdict", "value": "Fail"}},
					},
				},
				"health-after": map[string]any{
					"displayName": "health-after",
					"outputs": map[string]any{
						"parameters": []any{map[string]any{"name": "verdict", "value": "Pass"}},
					},
				},
			},
		},
	}}

	if got := workflowParameter(workflow, "verdict"); got != "Fail" {
		t.Fatalf("verdict = %q", got)
	}
}

func TestWorkflowParameterPrioritizesRootBeforeConflictingTaskOutputs(t *testing.T) {
	t.Parallel()

	const workflowName = "gpu-07-13-dapple-b200-full-node-replacement-a1"

	workflow := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": workflowName},
		"status": map[string]any{
			"nodes": map[string]any{
				workflowName: map[string]any{
					"name":        workflowName,
					"displayName": workflowName,
					"type":        "DAG",
					"phase":       "Succeeded",
					"outputs": map[string]any{"parameters": []any{
						map[string]any{"name": "verdict", "value": "Fail"},
						map[string]any{"name": "reason", "value": "NodeGPUCapacityMismatch"},
					}},
				},
				workflowName + "-preflight": map[string]any{
					"displayName": "node-health",
					"phase":       "Succeeded",
					"outputs": map[string]any{"parameters": []any{
						map[string]any{"name": "verdict", "value": "Fail"},
						map[string]any{"name": "reason", "value": "NodeGPUCapacityMismatch"},
					}},
				},
				workflowName + "-workload": map[string]any{
					"displayName": "workload",
					"phase":       "Skipped",
				},
				workflowName + "-mpi": map[string]any{
					"displayName": "mpi-backend",
					"phase":       "Skipped",
				},
				workflowName + "-health": map[string]any{
					"displayName": "health-after-preflight-failure",
					"phase":       "Succeeded",
					"outputs": map[string]any{"parameters": []any{
						map[string]any{"name": "verdict", "value": "Pass"},
						map[string]any{"name": "reason", "value": "FinalHealthPassed"},
					}},
				},
			},
		},
	}}

	for range 100 {
		if got := workflowParameter(workflow, "verdict"); got != "Fail" {
			t.Fatalf("verdict = %q", got)
		}

		if got := workflowParameter(workflow, "reason"); got != "NodeGPUCapacityMismatch" {
			t.Fatalf("reason = %q", got)
		}
	}
}

func TestTerminalVerdictUsesFailingEntrypointOutputOverLaterHealthPass(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	run := testRun("run-preflight-fail")
	run.Status = signalv1.CertificationRunStatus{
		Phase:    signalv1.RunPhaseRunning,
		Workflow: signalv1.WorkflowStatus{Name: "run-preflight-fail-a1", Attempt: 1},
	}
	node := readyNode("gpu-0", "A100")
	node.Annotations = map[string]string{ReservationLabel: run.Name}
	snapshot := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: run.Spec.Script.ConfigMapName, Namespace: run.Namespace},
		Data:       map[string]string{run.Spec.Script.Key: "test payload\n"},
	}
	workflow := workflowObject(run.Namespace, run.Status.Workflow.Name, "Succeeded", "", "")
	workflow.Object["status"].(map[string]any)["nodes"] = map[string]any{
		workflow.GetName(): map[string]any{
			"name":        workflow.GetName(),
			"displayName": workflow.GetName(),
			"type":        "DAG",
			"phase":       "Succeeded",
			"outputs": map[string]any{"parameters": []any{
				map[string]any{"name": "verdict", "value": "Fail"},
				map[string]any{"name": "reason", "value": "NodeGPUCapacityMismatch"},
			}},
		},
		"preflight": map[string]any{
			"displayName": "node-health",
			"phase":       "Succeeded",
			"outputs": map[string]any{"parameters": []any{
				map[string]any{"name": "verdict", "value": "Fail"},
				map[string]any{"name": "reason", "value": "NodeGPUCapacityMismatch"},
			}},
		},
		"workload": map[string]any{
			"displayName": "workload",
			"phase":       "Skipped",
		},
		"mpi": map[string]any{
			"displayName": "mpi-backend",
			"phase":       "Skipped",
		},
		"health-after-preflight-failure": map[string]any{
			"displayName": "health-after-preflight-failure",
			"phase":       "Succeeded",
			"outputs": map[string]any{"parameters": []any{
				map[string]any{"name": "verdict", "value": "Pass"},
				map[string]any{"name": "reason", "value": "FinalHealthPassed"},
			}},
		},
	}

	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run, node, snapshot, workflow).
		Build()
	r := &RunReconciler{Client: kube, Evidence: &memoryEvidenceStore{}, Now: time.Now}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatalf("terminal Reconcile: %v", err)
	}

	var got signalv1.CertificationRun
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), &got); err != nil {
		t.Fatalf("get run: %v", err)
	}

	if got.Status.Phase != signalv1.RunPhaseFailed ||
		got.Status.Verdict != "Fail" ||
		got.Status.Reason != "NodeGPUCapacityMismatch" {
		t.Fatalf("status = %#v", got.Status)
	}
}

func TestValidateStagePlanRejectsInvalidAndDuplicateStages(t *testing.T) {
	t.Parallel()

	if err := validateStagePlan([]signalv1.CertificationStage{{DisplayName: "Missing ID"}}); err == nil {
		t.Fatal("expected empty stage ID to fail validation")
	}

	if err := validateStagePlan([]signalv1.CertificationStage{
		{ID: "workload", DisplayName: "First"},
		{ID: "workload", DisplayName: "Duplicate"},
	}); err == nil {
		t.Fatal("expected duplicate stage IDs to fail validation")
	}
}

func TestTerminalVerdictPersistsEvidenceAndCleanupReleasesNode(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	run := testRun("run-pass")
	run.Status = signalv1.CertificationRunStatus{
		Phase:     signalv1.RunPhaseRunning,
		Workflow:  signalv1.WorkflowStatus{Name: "run-pass-a1", Attempt: 1},
		StartedAt: ptrTime(metav1.NewTime(time.Unix(100, 0))),
	}
	node := readyNode("gpu-0", "A100")
	node.Labels[RequestLabel] = run.Spec.Profile.Name
	node.Annotations = map[string]string{ReservationLabel: run.Name}
	snapshot := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: run.Spec.Script.ConfigMapName, Namespace: run.Namespace},
		Data:       map[string]string{run.Spec.Script.Key: "test payload\n"},
	}
	workflow := workflowObject(run.Namespace, run.Status.Workflow.Name, "Succeeded", "Pass", "Healthy")
	store := &memoryEvidenceStore{}
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run, node, snapshot, workflow).
		Build()
	r := &RunReconciler{Client: kube, Evidence: store, Now: func() time.Time { return time.Unix(200, 0) }}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatalf("terminal Reconcile: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatalf("cleanup Reconcile: %v", err)
	}

	var got signalv1.CertificationRun
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), &got); err != nil {
		t.Fatalf("get run: %v", err)
	}

	if got.Status.Phase != signalv1.RunPhaseSucceeded ||
		got.Status.Verdict != "Pass" ||
		got.Status.Workflow.Phase != "Succeeded" {
		t.Fatalf("status = %#v", got.Status)
	}

	if got.Status.Evidence.URI == "" || len(store.data) == 0 {
		t.Fatal("evidence was not persisted")
	}

	var evidence struct {
		Workflow signalv1.WorkflowStatus `json:"workflow"`
	}
	if err := json.Unmarshal(store.data, &evidence); err != nil {
		t.Fatalf("decode evidence: %v", err)
	}

	if evidence.Workflow.Phase != "Succeeded" {
		t.Fatalf("evidence workflow phase = %q, want Succeeded", evidence.Workflow.Phase)
	}

	if !got.Status.Cleanup.ReservationsReleased || !got.Status.Cleanup.SnapshotDeleted {
		t.Fatalf("cleanup status = %#v", got.Status.Cleanup)
	}

	if containsString(got.Finalizers, RunFinalizer) {
		t.Fatal("cleanup finalizer remains")
	}

	var released corev1.Node
	if err := kube.Get(context.Background(), client.ObjectKey{Name: node.Name}, &released); err != nil {
		t.Fatalf("get released node: %v", err)
	}

	if released.Annotations[ReservationLabel] != "" || released.Labels[RequestLabel] != "" {
		t.Fatalf("node was not released: labels=%v annotations=%v", released.Labels, released.Annotations)
	}

	if released.Labels[StateLabel] != "passed" ||
		released.Labels[EligibilityLabel] != "production" ||
		released.Annotations[LastSuccessfulRunAnnotation] != run.Name {
		t.Fatalf("terminal projection was not retained: labels=%v annotations=%v", released.Labels, released.Annotations)
	}
}

func TestWorkflowFailureRetriesWithoutSwitchingPinnedInputs(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	run := testRun("run-retry")
	run.Spec.MaxAttempts = 2
	run.Status = signalv1.CertificationRunStatus{
		Phase:    signalv1.RunPhaseRunning,
		Workflow: signalv1.WorkflowStatus{Name: "run-retry-a1", Attempt: 1},
	}
	workflow := workflowObject(run.Namespace, run.Status.Workflow.Name, "Failed", "", "")
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run, workflow).
		Build()
	r := &RunReconciler{Client: kube, Evidence: &memoryEvidenceStore{}}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got signalv1.CertificationRun
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), &got); err != nil {
		t.Fatalf("get run: %v", err)
	}

	if got.Status.Workflow.Attempt != 2 || got.Status.Workflow.Name != "run-retry-a2" {
		t.Fatalf("workflow status = %#v", got.Status.Workflow)
	}

	var retry unstructured.Unstructured
	retry.SetGroupVersionKind(workflowGVK)

	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: run.Namespace, Name: "run-retry-a2"}, &retry); err != nil {
		t.Fatalf("get retry workflow: %v", err)
	}

	if value := workflowArgument(&retry, "script-digest"); value != run.Spec.Script.Digest {
		t.Fatalf("retry script digest = %q", value)
	}

	if value := workflowArgument(&retry, "backend-kind"); value != run.Spec.Backend.Kind {
		t.Fatalf("retry backend = %q", value)
	}
}

func TestCanceledRunCleansUpDespiteCancelFlag(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	run := testRun("run-canceled")
	run.Spec.Cancel = true
	run.Status.Phase = signalv1.RunPhaseCancelled
	node := readyNode("gpu-0", "A100")
	node.Labels[RequestLabel] = run.Spec.Profile.Name
	node.Annotations = map[string]string{ReservationLabel: run.Name}
	snapshot := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: run.Spec.Script.ConfigMapName, Namespace: run.Namespace},
		Data:       map[string]string{run.Spec.Script.Key: "test payload\n"},
	}
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run, node, snapshot).
		Build()
	r := &RunReconciler{Client: kube, Evidence: &memoryEvidenceStore{}}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got signalv1.CertificationRun
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), &got); err != nil {
		t.Fatalf("get run: %v", err)
	}

	if containsString(got.Finalizers, RunFinalizer) || !got.Status.Cleanup.ReservationsReleased {
		t.Fatalf("run was not cleaned up: %#v", got)
	}
}

type memoryEvidenceStore struct {
	data []byte
}

func (s *memoryEvidenceStore) Put(_ context.Context, path string, data []byte) (string, string, error) {
	s.data = append([]byte(nil), data...)

	return "memory://" + path, digestString(string(data)), nil
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	if err := signalv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	scheme.AddKnownTypeWithName(workflowGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(workflowGVK.GroupVersion().WithKind("WorkflowList"), &unstructured.UnstructuredList{})

	return scheme
}

func readyNode(name, sku string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			UID:  types.UID(name + "-uid"),
			Labels: map[string]string{
				"node.kubernetes.io/instance-type": sku,
			},
		},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("8")},
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

func testProfile(name, sku string) *signalv1.CertificationProfile {
	return &signalv1.CertificationProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name + "-uid"), Generation: 3},
		Spec: signalv1.CertificationProfileSpec{
			Participants: signalv1.ParticipantPolicy{
				Count: 1,
				Selector: metav1.LabelSelector{MatchLabels: map[string]string{
					"node.kubernetes.io/instance-type": sku,
				}},
				RequiredResources: corev1.ResourceList{corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1")},
			},
			Script:       signalv1.ScriptReference{Name: "scripts", Key: "payload.txt"},
			Workflow:     signalv1.WorkflowReference{Name: "gpu-cert"},
			Backend:      signalv1.ExecutionBackend{APIVersion: "kubeflow.org/v2beta1", Kind: "MPIJob"},
			RunnerImage:  "runner@sha256:abc",
			Purpose:      signalv1.PurposeQuickCertification,
			StagePlan:    []signalv1.CertificationStage{{ID: "workload", DisplayName: "Workload", Category: "compute", Required: true}},
			EvidencePath: "signal/" + sku,
			Deadline:     metav1.Duration{Duration: 10 * time.Minute},
			MaxAttempts:  2,
		},
	}
}

func testRun(name string) *signalv1.CertificationRun {
	return &signalv1.CertificationRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  "signal",
			UID:        types.UID(name + "-uid"),
			Finalizers: []string{RunFinalizer},
		},
		Spec: signalv1.CertificationRunSpec{
			Profile:         signalv1.PinnedProfile{Name: "gpu-profile", UID: "profile-uid", Generation: 3},
			Participants:    []signalv1.Participant{{NodeName: "gpu-0", NodeUID: "gpu-0-uid"}},
			Script:          signalv1.ScriptSnapshot{ConfigMapName: name + "-scripts", Key: "payload.txt", Digest: digestString("test payload\n")},
			Workflow:        signalv1.WorkflowReference{Name: "gpu-cert"},
			Backend:         signalv1.ExecutionBackend{APIVersion: "kubeflow.org/v2beta1", Kind: "MPIJob"},
			RunnerImage:     "runner@sha256:abc",
			EvidencePath:    "signal/" + name + ".json",
			Deadline:        metav1.Duration{Duration: 10 * time.Minute},
			MaxAttempts:     2,
			RequestNodeName: "gpu-0",
		},
	}
}

func workflowObject(namespace, name, phase, verdict, reason string) *unstructured.Unstructured {
	workflow := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": workflowGVK.GroupVersion().String(),
		"kind":       workflowGVK.Kind,
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"status": map[string]any{
			"phase":   phase,
			"message": "test workflow message",
			"outputs": map[string]any{"parameters": []any{
				map[string]any{"name": "verdict", "value": verdict},
				map[string]any{"name": "reason", "value": reason},
			}},
		},
	}}

	return workflow
}

func workflowArgument(workflow *unstructured.Unstructured, name string) string {
	parameters, _, _ := unstructured.NestedSlice(workflow.Object, "spec", "arguments", "parameters")
	for _, raw := range parameters {
		parameter := raw.(map[string]any)
		if parameter["name"] == name {
			return fmt.Sprint(parameter["value"])
		}
	}

	return ""
}

func ptrTime(value metav1.Time) *metav1.Time {
	return &value
}
