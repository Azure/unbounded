// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func TestStrictCompletionPersistsTestAndRawWorkflowEvidenceBeforeCleanup(t *testing.T) {
	t.Parallel()

	run := builtinTestRun(t, FullNodeCertificationWorkflow)
	workflow := builtinWorkflow(run, "Succeeded")
	node := reservedTestNode(run)
	kube := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run, workflow, node, runSnapshot(run)).
		Build()
	r := evidenceReconciler(t, kube)

	reconcileRun(t, r, run)

	got := getTestRun(t, kube, run)
	if got.Status.Phase != signalv1.RunPhaseSucceeded || len(got.Status.TestResults) != 4 ||
		len(got.Status.Attempts) != 1 || got.Status.Attempts[0].Evidence.URI == "" {
		t.Fatalf("status = %#v", got.Status)
	}

	assertTestReservation(t, kube, run, run.Name)

	for _, result := range got.Status.TestResults {
		if result.Evidence != got.Status.Attempts[0].Evidence {
			t.Fatalf("result evidence = %#v", result)
		}
	}

	doc := readTestEvidence(t, kube, got.Status.Evidence)
	nodes := doc["workflowNodes"].(map[string]any)
	mpi := nodes[fixtureLeafID("gpu-0", "mpi-rank-access")].(map[string]any)

	outputs, malformed := testNodeOutputs(mpi)
	if malformed || !strings.Contains(outputs["device-inventory"], "GPU-uuid") ||
		doc["participantPlan"] == nil || doc["testPlan"] == nil || doc["testResults"] == nil {
		t.Fatalf("evidence = %#v", doc)
	}

	inputs, found, err := unstructured.NestedSlice(mpi, "inputs", "parameters")
	if err != nil || !found || parameterValue(inputs, "gpu-count") != "8" {
		t.Fatalf("MPI inputs missing from evidence: %#v", mpi)
	}

	reconcileRun(t, r, run)

	got = getTestRun(t, kube, run)
	if !got.Status.Cleanup.ReservationsReleased || !got.Status.Cleanup.SnapshotDeleted {
		t.Fatalf("cleanup = %#v", got.Status.Cleanup)
	}

	assertTestReservation(t, kube, run, "")

	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(workflow), &unstructured.Unstructured{
		Object: map[string]any{"apiVersion": workflow.GetAPIVersion(), "kind": workflow.GetKind()},
	}); err != nil {
		t.Fatalf("safe successful Workflow was not retained: %v", err)
	}
}

func TestStrictFailureCannotBeHiddenByRootPassOrRetriedAsInfrastructure(t *testing.T) {
	t.Parallel()

	for _, phase := range []string{"Succeeded", "Error"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()

			run := builtinTestRun(t, FullNodeCertificationWorkflow)
			run.Spec.MaxAttempts = 2
			workflow := builtinWorkflow(run, phase)
			setTestOutput(workflow, fixtureLeafID("gpu-0", "preflight"), "verdict", "Fail")
			setTestOutput(workflow, fixtureLeafID("gpu-0", "preflight"), "reason", "NodeGPUCapacityMismatch")
			delete(fixtureNodes(workflow), fixtureLeafID("gpu-0", "gpu-health"))
			delete(fixtureNodes(workflow), fixtureLeafID("gpu-0", "mpi-rank-access"))
			kube := fake.NewClientBuilder().
				WithScheme(testScheme(t)).
				WithStatusSubresource(&signalv1.CertificationRun{}).
				WithObjects(run, workflow, reservedTestNode(run), runSnapshot(run)).
				Build()
			r := evidenceReconciler(t, kube)

			reconcileRun(t, r, run)

			got := getTestRun(t, kube, run)
			if got.Status.Phase != signalv1.RunPhaseFailed || got.Status.Verdict != "Fail" ||
				got.Status.Workflow.Attempt != 1 || !strings.Contains(got.Status.Reason, "NodeGPUCapacityMismatch") {
				t.Fatalf("status = %#v", got.Status)
			}

			if got.Status.TestResults[3].Verdict != "Pass" || got.Status.TestResults[1].ExecutionState != "Omitted" {
				t.Fatalf("per-test results = %#v", got.Status.TestResults)
			}
		})
	}
}

func TestInfrastructureRetryPreservesAttemptResultsAndRawProvenance(t *testing.T) {
	t.Parallel()

	run := builtinTestRun(t, FullNodeCertificationWorkflow)
	run.Spec.MaxAttempts = 2
	workflow := builtinWorkflow(run, "Error")
	fixtureNodes(workflow)[fixtureLeafID("gpu-0", "preflight")].(map[string]any)["phase"] = "Error"
	kube := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run, workflow, reservedTestNode(run), runSnapshot(run)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetObjectKind().GroupVersionKind() == workflowGVK && obj.GetUID() == "" {
					obj.SetUID(types.UID(obj.GetName() + "-uid"))
				}

				return c.Create(ctx, obj, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if obj.GetObjectKind().GroupVersionKind() == workflowGVK {
					var evidence corev1.ConfigMap
					if err := c.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: run.Name + "-a1-evidence"}, &evidence); err != nil {
						return fmt.Errorf("attempt evidence missing before Workflow deletion: %w", err)
					}
				}

				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()
	r := evidenceReconciler(t, kube)

	reconcileRun(t, r, run)

	got := getTestRun(t, kube, run)
	if got.Status.Workflow.Attempt != 2 || len(got.Status.Attempts) != 1 || len(got.Status.TestResults) != 4 ||
		got.Status.TestResults[0].Verdict != "Inconclusive" {
		t.Fatalf("retry status = %#v", got.Status)
	}

	firstEvidence := got.Status.Attempts[0].Evidence

	first := readTestEvidence(t, kube, firstEvidence)
	if first["executionState"] != "Error" || first["workflowNodes"] == nil {
		t.Fatalf("first attempt evidence = %#v", first)
	}

	retry := builtinWorkflow(got, "Succeeded")
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(workflowGVK)

	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(retry), existing); err != nil {
		t.Fatal(err)
	}

	retry.SetResourceVersion(existing.GetResourceVersion())

	if err := kube.Update(context.Background(), retry); err != nil {
		t.Fatal(err)
	}

	reconcileRun(t, r, run)

	got = getTestRun(t, kube, run)
	if got.Status.Phase != signalv1.RunPhaseSucceeded || len(got.Status.Attempts) != 2 || len(got.Status.TestResults) != 8 ||
		got.Status.Attempts[0].Evidence != firstEvidence || got.Status.TestResults[0].Evidence != firstEvidence ||
		got.Status.TestResults[4].Attempt != 2 || got.Status.TestResults[4].WorkflowUID == got.Status.TestResults[0].WorkflowUID {
		t.Fatalf("final retry status = %#v", got.Status)
	}

	readTestEvidence(t, kube, firstEvidence)
}

func TestCancellationSavesPartialSiblingResultsBeforeForegroundDeletion(t *testing.T) {
	t.Parallel()

	run := builtinTestRun(t, BurnCanaryWorkflow)
	run.Spec.Cancel = true
	workflow := builtinWorkflow(run, "Running")
	fixtureNodes(workflow)[fixtureLeafID("gpu-0", "mixed-burn")].(map[string]any)["phase"] = "Running"
	fixtureNodes(workflow)[fixtureContainerID("gpu-0", "main")].(map[string]any)["phase"] = "Running"
	fixtureNodes(workflow)[fixtureContainerID("gpu-0", "cpu-stress")].(map[string]any)["phase"] = "Failed"
	setTestOutput(workflow, fixtureLeafID("gpu-0", "mixed-burn"), "cpu-stress-verdict", "Fail")
	setTestOutput(workflow, fixtureLeafID("gpu-0", "mixed-burn"), "cpu-stress-reason", "CPUComputationError")

	holdDeletion := true
	kube := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run, workflow, reservedTestNode(run), runSnapshot(run)).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if obj.GetObjectKind().GroupVersionKind() == workflowGVK {
					options := &client.DeleteOptions{}
					for _, option := range opts {
						option.ApplyToDelete(options)
					}

					if options.PropagationPolicy == nil || *options.PropagationPolicy != metav1.DeletePropagationForeground {
						return errors.New("Workflow deletion was not foreground")
					}

					var evidence corev1.ConfigMap
					if err := c.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: run.Name + "-evidence"}, &evidence); err != nil {
						return fmt.Errorf("final evidence missing before cancellation: %w", err)
					}

					if holdDeletion {
						return nil
					}
				}

				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()
	r := evidenceReconciler(t, kube)
	result := reconcileRun(t, r, run)

	got := getTestRun(t, kube, run)
	if result.RequeueAfter == 0 || got.Status.Phase.Terminal() || len(got.Status.TestResults) != 4 ||
		got.Status.TestResults[1].Verdict != "Fail" || got.Status.TestResults[0].Verdict == "Pass" {
		t.Fatalf("cancel status = %#v, result = %#v", got.Status, result)
	}

	assertTestReservation(t, kube, run, run.Name)

	evidence := got.Status.Evidence

	doc := readTestEvidence(t, kube, evidence)
	if doc["executionState"] != "Interrupted" || doc["workflowNodes"] == nil {
		t.Fatalf("interruption evidence = %#v", doc)
	}

	holdDeletion = false

	reconcileRun(t, r, run)
	reconcileRun(t, r, run)

	got = getTestRun(t, kube, run)
	if got.Status.Phase != signalv1.RunPhaseCancelled || got.Status.Evidence != evidence ||
		got.Status.TestResults[1].Verdict != "Fail" || !got.Status.Cleanup.ReservationsReleased {
		t.Fatalf("cleaned cancel status = %#v", got.Status)
	}

	assertTestReservation(t, kube, run, "")
}

func TestEvidenceFailureDoesNotDeleteWorkflowStartRetryOrReleaseReservations(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"completion", "retry", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			run := builtinTestRun(t, BurnCanaryWorkflow)

			phase := "Succeeded"
			if mode == "retry" {
				phase = "Error"
				run.Spec.MaxAttempts = 2
			}

			if mode == "cancel" {
				phase = "Running"
				run.Spec.Cancel = true
			}

			workflow := builtinWorkflow(run, phase)
			kube := fake.NewClientBuilder().
				WithScheme(testScheme(t)).
				WithStatusSubresource(&signalv1.CertificationRun{}).
				WithObjects(run, workflow, reservedTestNode(run), runSnapshot(run)).
				Build()

			r := &RunReconciler{Client: kube, Evidence: &unavailableEvidenceStore{}}
			if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err == nil {
				t.Fatal("missing evidence did not stop reconciliation")
			}

			got := getTestRun(t, kube, run)
			if got.Status.Phase.Terminal() || got.Status.Workflow.Attempt != 1 {
				t.Fatalf("unsafe status = %#v", got.Status)
			}

			assertTestReservation(t, kube, run, run.Name)

			existing := &unstructured.Unstructured{}
			existing.SetGroupVersionKind(workflowGVK)

			if err := kube.Get(context.Background(), client.ObjectKeyFromObject(workflow), existing); err != nil {
				t.Fatalf("Workflow deleted before evidence: %v", err)
			}
		})
	}
}

func TestImmutableEvidenceRecoversAfterPostWriteStatusFailure(t *testing.T) {
	t.Parallel()

	run := builtinTestRun(t, BurnCanaryWorkflow)
	workflow := builtinWorkflow(run, "Succeeded")
	failOnce := true
	kube := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run, workflow, reservedTestNode(run), runSnapshot(run)).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(
				ctx context.Context,
				c client.Client,
				subresource string,
				obj client.Object,
				patch client.Patch,
				opts ...client.SubResourcePatchOption,
			) error {
				updated, ok := obj.(*signalv1.CertificationRun)
				if ok && failOnce && len(updated.Status.Attempts) == 1 && updated.Status.Attempts[0].Evidence.URI != "" {
					failOnce = false

					return errors.New("status write failed after immutable evidence was saved")
				}

				return c.SubResource(subresource).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	r := evidenceReconciler(t, kube)
	now := time.Date(2026, 10, 13, 1, 2, 3, 456789, time.UTC)
	r.Now = func() time.Time { return now }

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err == nil {
		t.Fatal("expected injected status failure")
	}

	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(workflowGVK)

	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(workflow), current); err != nil {
		t.Fatal(err)
	}

	setTestOutput(current, fixtureLeafID("gpu-0", "mixed-burn"), "gpu-burn-measurements", "later mutable Workflow output")

	if err := kube.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}

	now = now.Add(time.Minute)

	reconcileRun(t, r, run)

	got := getTestRun(t, kube, run)
	if got.Status.Phase != signalv1.RunPhaseSucceeded || got.Status.Attempts[0].Evidence.URI == "" ||
		got.Status.Attempts[0].FinishedAt.Nanosecond() != 0 {
		t.Fatalf("recovered status = %#v", got.Status)
	}

	doc := readTestEvidence(t, kube, got.Status.Evidence)
	nodes := doc["workflowNodes"].(map[string]any)

	outputs, malformed := testNodeOutputs(nodes[fixtureLeafID("gpu-0", "mixed-burn")].(map[string]any))
	if malformed || outputs["gpu-burn-measurements"] == "later mutable Workflow output" {
		t.Fatal("immutable attempt snapshot was replaced by later Workflow output")
	}
}

func TestExistingEvidenceFromAnotherRunFailsClosed(t *testing.T) {
	t.Parallel()

	run := builtinTestRun(t, BurnCanaryWorkflow)
	workflow := builtinWorkflow(run, "Succeeded")
	kube := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run, workflow, reservedTestNode(run), runSnapshot(run)).
		Build()

	r := evidenceReconciler(t, kube)
	if _, _, err := r.Evidence.Put(context.Background(), "signal/"+run.Name+"-a1.json", []byte(`{"runID":"other-run"}`)); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err == nil ||
		!strings.Contains(err.Error(), "does not match") {
		t.Fatalf("unrelated evidence was accepted: %v", err)
	}

	assertTestReservation(t, kube, run, run.Name)
}

func TestStrictCleanupAndRetryWaitForOwnedBackendDescendants(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"cleanup", "retry"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			run := builtinTestRun(t, FullNodeCertificationWorkflow)
			run.Spec.MaxAttempts = 2

			phase := "Succeeded"
			if mode == "retry" {
				phase = "Error"
			}

			workflow := builtinWorkflow(run, phase)
			fixtureNodes(workflow)["cleanup-hook"] = map[string]any{
				"id": "cleanup-hook", "name": workflow.GetName() + ".hooks.exit", "type": "Pod", "phase": "Error",
			}
			backend := testOwnedBackend(run)
			kube := fake.NewClientBuilder().
				WithScheme(testScheme(t)).
				WithStatusSubresource(&signalv1.CertificationRun{}).
				WithObjects(run, workflow, backend, reservedTestNode(run), runSnapshot(run)).
				Build()
			r := evidenceReconciler(t, kube)

			reconcileRun(t, r, run)

			if mode == "cleanup" {
				got := getTestRun(t, kube, run)
				if got.Status.Phase != signalv1.RunPhaseInconclusive {
					t.Fatalf("failed cleanup hook certified the run: %#v", got.Status)
				}

				reconcileRun(t, r, run)
			}

			got := getTestRun(t, kube, run)
			if got.Status.Cleanup.ReservationsReleased || got.Status.Workflow.Attempt != 1 ||
				len(got.Status.Attempts) != 1 || got.Status.Attempts[0].Evidence.URI == "" {
				t.Fatalf("unsafe cleanup/retry status = %#v", got.Status)
			}

			assertTestReservation(t, kube, run, run.Name)
			readTestEvidence(t, kube, got.Status.Attempts[0].Evidence)

			var node corev1.Node
			if err := kube.Get(context.Background(), client.ObjectKey{Name: run.Spec.RequestNodeName}, &node); err != nil {
				t.Fatal(err)
			}

			if node.Labels[EligibilityLabel] == "production" {
				t.Fatal("projected production eligibility before descendant cleanup")
			}

			if err := kube.Delete(context.Background(), backend); err != nil {
				t.Fatal(err)
			}

			reconcileRun(t, r, run)

			got = getTestRun(t, kube, run)
			if mode == "cleanup" {
				assertTestReservation(t, kube, run, "")
			} else if got.Status.Workflow.Attempt != 2 {
				t.Fatalf("retry did not resume after descendants were gone: %#v", got.Status)
			}
		})
	}
}

func TestBackendCleanupDoesNotTouchOtherWorkflowOwners(t *testing.T) {
	t.Parallel()

	run := builtinTestRun(t, FullNodeCertificationWorkflow)
	workflow := builtinWorkflow(run, "Succeeded")
	backend := testOwnedBackend(run)
	owners := backend.GetOwnerReferences()
	owners[0].UID = "other-workflow-uid"
	backend.SetOwnerReferences(owners)
	kube := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run, workflow, backend, reservedTestNode(run), runSnapshot(run)).
		Build()
	r := evidenceReconciler(t, kube)

	reconcileRun(t, r, run)
	reconcileRun(t, r, run)
	assertTestReservation(t, kube, run, "")

	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(backend), backend); err != nil {
		t.Fatalf("another Workflow's backend was touched: %v", err)
	}
}

type unavailableEvidenceStore struct{}

func (*unavailableEvidenceStore) Put(context.Context, string, []byte) (string, string, error) {
	return "", "", errors.New("evidence storage unavailable")
}

func evidenceReconciler(t *testing.T, kube client.Client) *RunReconciler {
	t.Helper()

	store, err := NewKubernetesEvidenceStore(kube, "signal")
	if err != nil {
		t.Fatal(err)
	}

	return &RunReconciler{Client: kube, Evidence: store}
}

func reservedTestNode(run *signalv1.CertificationRun) *corev1.Node {
	node := readyNode(run.Spec.RequestNodeName, "example-gpu")
	node.Annotations = map[string]string{ReservationLabel: run.Name}
	node.Labels[RequestLabel] = run.Spec.Profile.Name
	node.Labels[StateLabel] = "testing"
	node.Labels[EligibilityLabel] = "blocked"

	return node
}

func reconcileRun(t *testing.T, r *RunReconciler, run *signalv1.CertificationRun) reconcile.Result {
	t.Helper()

	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	return result
}

func getTestRun(t *testing.T, kube client.Client, run *signalv1.CertificationRun) *signalv1.CertificationRun {
	t.Helper()

	var got signalv1.CertificationRun
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), &got); err != nil {
		t.Fatal(err)
	}

	return &got
}

func assertTestReservation(t *testing.T, kube client.Client, run *signalv1.CertificationRun, expected string) {
	t.Helper()

	var node corev1.Node
	if err := kube.Get(context.Background(), client.ObjectKey{Name: run.Spec.RequestNodeName}, &node); err != nil {
		t.Fatal(err)
	}

	if node.Annotations[ReservationLabel] != expected {
		t.Fatalf("reservation = %q, want %q", node.Annotations[ReservationLabel], expected)
	}
}

func readTestEvidence(t *testing.T, kube client.Client, evidence signalv1.EvidenceStatus) map[string]any {
	t.Helper()

	parts := strings.Split(evidence.URI, "/")
	if len(parts) != 6 || parts[0] != "kubernetes:" {
		t.Fatalf("invalid evidence URI %q", evidence.URI)
	}

	var configMap corev1.ConfigMap
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: parts[2], Name: parts[4]}, &configMap); err != nil {
		t.Fatal(err)
	}

	reader, err := gzip.NewReader(bytes.NewReader(configMap.BinaryData["evidence.json.gz"]))
	if err != nil {
		t.Fatal(err)
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}

	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}

	if digestString(string(data)) != evidence.Digest {
		t.Fatal("persisted evidence digest does not match its reference")
	}

	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}

	return doc
}

func testOwnedBackend(run *signalv1.CertificationRun) *unstructured.Unstructured {
	backend := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": run.Spec.Backend.APIVersion,
		"kind":       run.Spec.Backend.Kind,
		"metadata": map[string]any{
			"name":      "owned-mpi-backend",
			"namespace": run.Namespace,
			"uid":       "backend-uid",
		},
	}}
	backend.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: workflowGVK.GroupVersion().String(),
		Kind:       workflowGVK.Kind,
		Name:       run.Status.Workflow.Name,
		UID:        types.UID(run.Status.Workflow.UID),
	}})

	return backend
}

func TestDeletingActiveStrictRunPreservesEvidenceAndWaitsForBackendCleanup(t *testing.T) {
	t.Parallel()

	run := builtinTestRun(t, FullNodeCertificationWorkflow)
	workflow := builtinWorkflow(run, "Running")
	backend := testOwnedBackend(run)
	kube := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run, workflow, backend, reservedTestNode(run), runSnapshot(run)).
		Build()

	r := evidenceReconciler(t, kube)
	if err := kube.Delete(context.Background(), run); err != nil {
		t.Fatal(err)
	}

	reconcileRun(t, r, run)

	got := getTestRun(t, kube, run)
	if got.Status.Evidence.URI == "" || !containsString(got.Finalizers, RunFinalizer) {
		t.Fatalf("deleting status = %#v", got)
	}

	assertTestReservation(t, kube, run, run.Name)
	readTestEvidence(t, kube, got.Status.Evidence)

	if err := kube.Delete(context.Background(), backend); err != nil {
		t.Fatal(err)
	}

	reconcileRun(t, r, run)

	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), &signalv1.CertificationRun{}); !apierrors.IsNotFound(err) {
		t.Fatalf("run remains after deletion cleanup: %v", err)
	}

	assertTestReservation(t, kube, run, "")
}
