// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"encoding/json"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func TestLegacyBuiltinWorkflowKeepsEmptyTestPlan(t *testing.T) {
	t.Parallel()

	for _, name := range []string{GPUCertificationWorkflow, FullNodeCertificationWorkflow, BurnCanaryWorkflow} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			for _, empty := range [][]signalv1.TestInvocation{nil, {}} {
				run := builtinTestRun(t, name)
				run.Spec.TestPlan = empty
				run.Spec.ParticipantPlan = nil
				run.Spec.Parameters[TestPlanParameter] = `[{"id":"untrusted"}]`
				before := run.DeepCopy()

				workflow, err := buildWorkflow(run, 2)
				if err != nil {
					t.Fatal(err)
				}

				if value := workflowArgument(workflow, TestPlanParameter); value != "[]" {
					t.Fatalf("legacy runtime plan = %q, want exactly []", value)
				}

				var participants []signalv1.ParticipantSnapshot
				if err := json.Unmarshal([]byte(workflowArgument(workflow, ParticipantPlanParameter)), &participants); err != nil {
					t.Fatal(err)
				}

				if len(participants) != 1 || participants[0].NodeName != "gpu-0" || participants[0].NodeUID != "gpu-0-uid" ||
					participants[0].ObservedGPUCapacity != "8" || participants[0].ObservedGPUAllocatable != "8" {
					t.Fatalf("legacy participant arguments = %#v", participants)
				}

				if plan, strict, err := effectiveTestPlan(run); err != nil || strict || len(plan) != 0 {
					t.Fatalf("legacy collection = %#v, strict = %t, err = %v", plan, strict, err)
				}

				if !reflect.DeepEqual(run, before) {
					t.Fatal("building legacy arguments mutated the immutable run")
				}
			}
		})
	}
}

func TestLegacyBuiltinRetryKeepsOpaqueImmutableSnapshot(t *testing.T) {
	t.Parallel()

	for _, name := range []string{GPUCertificationWorkflow, FullNodeCertificationWorkflow, BurnCanaryWorkflow} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			run := builtinTestRun(t, name)
			run.Spec.TestPlan = nil
			run.Spec.ParticipantPlan = nil
			run.Spec.MaxAttempts = 2
			content := "#!/bin/bash\n# Original opaque legacy entrypoint.\nexit 0\n"
			run.Spec.Script.Digest = digestString(content)
			before := run.Spec
			snapshot := runSnapshot(run)
			immutable := true
			snapshot.Immutable = &immutable
			snapshot.Data[run.Spec.Script.Key] = content
			currentScript := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "scripts", Namespace: run.Namespace},
				Data:       map[string]string{run.Spec.Script.Key: "new component-aware entrypoint\n"},
			}
			workflow := builtinWorkflow(run, "Error")
			kube := fake.NewClientBuilder().
				WithScheme(testScheme(t)).
				WithStatusSubresource(&signalv1.CertificationRun{}).
				WithObjects(run, workflow, snapshot, currentScript, reservedTestNode(run)).
				Build()
			r := evidenceReconciler(t, kube)

			reconcileRun(t, r, run)

			got := getTestRun(t, kube, run)
			if got.Status.Workflow.Attempt != 2 || !reflect.DeepEqual(got.Spec, before) ||
				len(got.Status.TestResults) != 0 || len(got.Status.Attempts) != 1 {
				t.Fatalf("legacy retry status/spec = %#v", got)
			}

			retry := &unstructured.Unstructured{}
			retry.SetGroupVersionKind(workflowGVK)

			if err := kube.Get(t.Context(), client.ObjectKey{Namespace: run.Namespace, Name: got.Status.Workflow.Name}, retry); err != nil {
				t.Fatal(err)
			}

			if workflowArgument(retry, TestPlanParameter) != "[]" ||
				workflowArgument(retry, "script-configmap") != run.Spec.Script.ConfigMapName ||
				workflowArgument(retry, "script-key") != run.Spec.Script.Key ||
				workflowArgument(retry, "script-digest") != run.Spec.Script.Digest ||
				workflowArgument(retry, "participants") != "gpu-0" || workflowArgument(retry, "attempt") != "2" {
				t.Fatalf("legacy retry arguments = %#v", retry.Object["spec"])
			}

			template, _, err := unstructured.NestedString(retry.Object, "spec", "workflowTemplateRef", "name")
			if err != nil || template != name {
				t.Fatalf("retry template = %q, err = %v", template, err)
			}

			var pinned corev1.ConfigMap
			if err := kube.Get(t.Context(), client.ObjectKeyFromObject(snapshot), &pinned); err != nil {
				t.Fatal(err)
			}

			if pinned.Immutable == nil || !*pinned.Immutable || pinned.Data[run.Spec.Script.Key] != content {
				t.Fatal("legacy snapshot was replaced or made mutable")
			}

			doc := readTestEvidence(t, kube, got.Status.Attempts[0].Evidence)
			if doc["scriptContent"] != content || doc["testResults"] != nil {
				t.Fatal("legacy retry evidence inferred new tests or used the current source script")
			}

			assertTestReservation(t, kube, run, run.Name)
		})
	}
}

func TestLegacyBuiltinUsesAggregateRootOutputsNotLaterHealth(t *testing.T) {
	t.Parallel()

	for _, name := range []string{GPUCertificationWorkflow, FullNodeCertificationWorkflow, BurnCanaryWorkflow} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			for _, tc := range []struct {
				name     string
				verdict  string
				rootNode bool
				want     signalv1.RunPhase
			}{
				{"root pass", "Pass", false, signalv1.RunPhaseSucceeded},
				{"root failure", "Fail", false, signalv1.RunPhaseFailed},
				{"root node failure", "Fail", true, signalv1.RunPhaseFailed},
				{"missing root aggregate", "", false, signalv1.RunPhaseInconclusive},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()

					run := builtinTestRun(t, name)
					run.Spec.TestPlan = nil
					run.Spec.ParticipantPlan = nil
					workflow := builtinWorkflow(run, "Succeeded")
					status := workflow.Object["status"].(map[string]any)
					aggregate := map[string]any{"parameters": []any{
						map[string]any{"name": "verdict", "value": tc.verdict},
						map[string]any{"name": "reason", "value": "LegacyAggregateOutcome"},
					}}
					status["outputs"] = aggregate
					fixtureNodes(workflow)["late-health"] = map[string]any{
						"id": "late-health", "name": workflow.GetName() + ".health-after", "type": "Pod", "phase": "Succeeded",
						"outputs": map[string]any{"parameters": []any{
							map[string]any{"name": "verdict", "value": "Pass"},
							map[string]any{"name": "reason", "value": "HealthyFinalStage"},
						}},
					}

					if tc.rootNode {
						delete(status, "outputs")

						fixtureNodes(workflow)[workflow.GetName()] = map[string]any{
							"id": workflow.GetName(), "name": workflow.GetName(), "type": "Steps", "phase": "Succeeded",
							"outputs": aggregate,
						}
					} else if tc.verdict == "" {
						delete(status, "outputs")

						fixtureNodes(workflow)["missing-workload-result"] = map[string]any{
							"id": "missing-workload-result", "name": workflow.GetName() + ".workload", "type": "Pod", "phase": "Failed",
						}
					}

					kube := fake.NewClientBuilder().
						WithScheme(testScheme(t)).
						WithStatusSubresource(&signalv1.CertificationRun{}).
						WithObjects(run, workflow, reservedTestNode(run), runSnapshot(run)).
						Build()
					r := evidenceReconciler(t, kube)

					reconcileRun(t, r, run)

					got := getTestRun(t, kube, run)
					if got.Status.Phase != tc.want || len(got.Status.TestResults) != 0 ||
						(tc.verdict != "" && (got.Status.Verdict != tc.verdict || got.Status.Reason != "LegacyAggregateOutcome")) {
						t.Fatalf("legacy aggregate status = %#v", got.Status)
					}

					if results := ObserveTestResults(got, workflow); len(results) != 0 {
						t.Fatalf("legacy observation invented per-test verdicts: %#v", results)
					}
				})
			}
		})
	}
}

func TestPresentBuiltinPlanNeverFallsBackToRootPass(t *testing.T) {
	t.Parallel()

	for _, name := range []string{GPUCertificationWorkflow, FullNodeCertificationWorkflow, BurnCanaryWorkflow} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			for _, invalidPlan := range []bool{false, true} {
				run := builtinTestRun(t, name)
				if invalidPlan {
					run.Spec.TestPlan = run.Spec.TestPlan[:1]
				}

				workflow := builtinWorkflow(run, "Succeeded")
				workflow.Object["status"].(map[string]any)["nodes"] = map[string]any{}

				collection := collectTestResults(run, workflow, true)
				if !collection.strict || collection.verdict != "Inconclusive" || len(collection.results) == 0 {
					t.Fatalf("present plan selected legacy root fallback: %#v", collection)
				}

				kube := fake.NewClientBuilder().
					WithScheme(testScheme(t)).
					WithStatusSubresource(&signalv1.CertificationRun{}).
					WithObjects(run, workflow, reservedTestNode(run), runSnapshot(run)).
					Build()

				reconcileRun(t, evidenceReconciler(t, kube), run)

				if got := getTestRun(t, kube, run); got.Status.Phase != signalv1.RunPhaseInconclusive {
					t.Fatalf("root Pass certified a present/incomplete plan: %#v", got.Status)
				}
			}
		})
	}
}

func TestCustomWorkflowKeepsTaskOutputFallback(t *testing.T) {
	t.Parallel()

	for _, planned := range []bool{false, true} {
		run := builtinTestRun(t, FullNodeCertificationWorkflow)

		run.Spec.Workflow.Name = "custom-workflow"
		if !planned {
			run.Spec.TestPlan = nil
		}

		workflow := builtinWorkflow(run, "Succeeded")
		delete(workflow.Object["status"].(map[string]any), "outputs")
		fixtureNodes(workflow)["custom-task"] = map[string]any{
			"id": "custom-task", "name": workflow.GetName() + ".custom", "type": "Pod", "phase": "Succeeded",
			"outputs": map[string]any{"parameters": []any{
				map[string]any{"name": "verdict", "value": "Pass"},
				map[string]any{"name": "reason", "value": "Healthy"},
			}},
		}
		kube := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithStatusSubresource(&signalv1.CertificationRun{}).
			WithObjects(run, workflow, reservedTestNode(run), runSnapshot(run)).
			Build()

		reconcileRun(t, evidenceReconciler(t, kube), run)

		if got := getTestRun(t, kube, run); got.Status.Phase != signalv1.RunPhaseSucceeded || len(got.Status.TestResults) != 0 {
			t.Fatalf("custom workflow fallback changed: %#v", got.Status)
		}
	}
}

func TestLegacyBuiltinRetryStillWaitsForOwnedBackendCleanup(t *testing.T) {
	t.Parallel()

	run := builtinTestRun(t, FullNodeCertificationWorkflow)
	run.Spec.TestPlan = nil
	run.Spec.ParticipantPlan = nil
	run.Spec.MaxAttempts = 2
	workflow := builtinWorkflow(run, "Error")
	backend := testOwnedBackend(run)
	kube := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run, workflow, backend, reservedTestNode(run), runSnapshot(run)).
		Build()
	r := evidenceReconciler(t, kube)

	result := reconcileRun(t, r, run)

	got := getTestRun(t, kube, run)
	if result.RequeueAfter == 0 || got.Status.Workflow.Attempt != 1 || len(got.Status.Attempts) != 1 ||
		got.Status.Attempts[0].Evidence.URI == "" || len(got.Status.TestResults) != 0 {
		t.Fatalf("legacy retry bypassed evidence or backend cleanup: %#v", got.Status)
	}

	assertTestReservation(t, kube, run, run.Name)

	if err := kube.Delete(t.Context(), backend); err != nil {
		t.Fatal(err)
	}

	reconcileRun(t, r, run)

	if got := getTestRun(t, kube, run); got.Status.Workflow.Attempt != 2 {
		t.Fatalf("legacy retry did not resume after backend cleanup: %#v", got.Status)
	}
}

func TestLegacyBuiltinRootPassDoesNotReleaseBeforeBackendCleanup(t *testing.T) {
	t.Parallel()

	run := builtinTestRun(t, FullNodeCertificationWorkflow)
	run.Spec.TestPlan = nil
	run.Spec.ParticipantPlan = nil
	workflow := builtinWorkflow(run, "Succeeded")
	backend := testOwnedBackend(run)
	kube := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run, workflow, backend, reservedTestNode(run), runSnapshot(run)).
		Build()
	r := evidenceReconciler(t, kube)

	reconcileRun(t, r, run)
	reconcileRun(t, r, run)
	assertTestReservation(t, kube, run, run.Name)

	var node corev1.Node
	if err := kube.Get(t.Context(), client.ObjectKey{Name: run.Spec.RequestNodeName}, &node); err != nil {
		t.Fatal(err)
	}

	if node.Labels[EligibilityLabel] == "production" {
		t.Fatal("legacy root Pass projected production before backend cleanup")
	}

	if err := kube.Delete(t.Context(), backend); err != nil {
		t.Fatal(err)
	}

	reconcileRun(t, r, run)
	assertTestReservation(t, kube, run, "")
}

func TestEveryBuiltinAdmissionFreezesRequiredTestPlan(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		count int
	}{
		{GPUCertificationWorkflow, 2},
		{FullNodeCertificationWorkflow, 4},
		{BurnCanaryWorkflow, 4},
		{RackCertificationWorkflow, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			node := readyNode("gpu-0", "example-gpu")
			node.Labels[RequestLabel] = "planned"
			profile := testProfile("planned", "example-gpu")

			profile.Spec.Workflow.Name = tc.name
			if tc.name == RackCertificationWorkflow {
				profile.Spec.Parameters = map[string]string{NodeWorkflowTemplateParameter: FullNodeCertificationWorkflow}
			}

			source := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "scripts", Namespace: "signal"},
				Data:       map[string]string{"payload.txt": "new runner\n"},
			}
			kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(node, profile, source).Build()

			r := &AdmissionReconciler{Client: kube, Namespace: "signal", ScriptNamespace: "signal"}
			if _, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKey{Name: node.Name}}); err != nil {
				t.Fatal(err)
			}

			var runs signalv1.CertificationRunList
			if err := kube.List(t.Context(), &runs); err != nil {
				t.Fatal(err)
			}

			if len(runs.Items) != 1 || len(runs.Items[0].Spec.TestPlan) != tc.count {
				t.Fatalf("new admission did not freeze its required plan: %#v", runs.Items)
			}

			run := &runs.Items[0]
			if _, strict, err := effectiveTestPlan(run); err != nil || !strict || len(run.Spec.ParticipantPlan) != 1 {
				t.Fatalf("new builtin admission became legacy: strict = %t, err = %v", strict, err)
			}

			workflow, err := buildWorkflow(run, 1)
			if err != nil || workflowArgument(workflow, TestPlanParameter) == "[]" {
				t.Fatalf("new builtin selected legacy arguments: workflow = %#v, err = %v", workflow, err)
			}
		})
	}
}
