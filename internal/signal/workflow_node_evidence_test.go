// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func TestCompressedAttemptEvidenceRetainsRepresentationAndMPIInventory(t *testing.T) {
	t.Parallel()

	run := builtinTestRun(t, FullNodeCertificationWorkflow)
	workflow := builtinWorkflow(run, "Succeeded")
	encoded := compressFixtureNodes(t, workflow)
	kube := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run, workflow, reservedTestNode(run), runSnapshot(run)).
		Build()
	r := evidenceReconciler(t, kube)

	reconcileRun(t, r, run)

	got := getTestRun(t, kube, run)
	if got.Status.Phase != signalv1.RunPhaseSucceeded {
		t.Fatalf("compressed run status = %#v", got.Status)
	}

	for _, evidence := range []signalv1.EvidenceStatus{got.Status.Attempts[0].Evidence, got.Status.Evidence} {
		doc := readTestEvidence(t, kube, evidence)

		markers := doc["workflowNodeStatus"].(map[string]any)
		if !reflect.DeepEqual(markers, map[string]any{"compressedNodes": encoded}) {
			t.Fatalf("compressed status representation changed: %#v", markers)
		}

		nodes := doc["workflowNodes"].(map[string]any)

		outputs, malformed := testNodeOutputs(nodes[fixtureLeafID("gpu-0", "mpi-rank-access")].(map[string]any))
		if malformed || !strings.Contains(outputs["device-inventory"], "GPU-uuid") {
			t.Fatal("compressed MPI provenance was lost from immutable evidence")
		}
	}

	reconcileRun(t, r, run)
	assertTestReservation(t, kube, run, "")

	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(workflowGVK)

	if err := kube.Get(t.Context(), client.ObjectKeyFromObject(workflow), current); err != nil {
		t.Fatalf("safe compressed Workflow was unnecessarily deleted: %v", err)
	}
}

func TestCompressedStatusSurvivesImmutableCheckpointRecovery(t *testing.T) {
	t.Parallel()

	run := builtinTestRun(t, BurnCanaryWorkflow)
	workflow := builtinWorkflow(run, "Succeeded")
	encoded := compressFixtureNodes(t, workflow)
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

					return errors.New("injected post-evidence status failure")
				}

				return c.SubResource(subresource).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	r := evidenceReconciler(t, kube)

	if _, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err == nil {
		t.Fatal("expected post-evidence status failure")
	}

	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(workflowGVK)

	if err := kube.Get(t.Context(), client.ObjectKeyFromObject(workflow), current); err != nil {
		t.Fatal(err)
	}

	nodes, err := workflowNodes(current)
	if err != nil {
		t.Fatal(err)
	}

	current.Object["status"].(map[string]any)["nodes"] = nodes
	delete(current.Object["status"].(map[string]any), "compressedNodes")
	setTestOutput(current, fixtureLeafID("gpu-0", "mixed-burn"), "gpu-burn-measurements", "later mutable Workflow output")

	if err := kube.Update(t.Context(), current); err != nil {
		t.Fatal(err)
	}

	reconcileRun(t, r, run)

	got := getTestRun(t, kube, run)
	if got.Status.Phase != signalv1.RunPhaseSucceeded || got.Status.Attempts[0].Evidence.URI == "" {
		t.Fatalf("recovered status = %#v", got.Status)
	}

	for _, evidence := range []signalv1.EvidenceStatus{got.Status.Attempts[0].Evidence, got.Status.Evidence} {
		doc := readTestEvidence(t, kube, evidence)
		if !reflect.DeepEqual(doc["workflowNodeStatus"], map[string]any{"compressedNodes": encoded}) {
			t.Fatal("later Workflow representation replaced immutable compressed provenance")
		}

		savedNodes := doc["workflowNodes"].(map[string]any)

		outputs, _ := testNodeOutputs(savedNodes[fixtureLeafID("gpu-0", "mixed-burn")].(map[string]any))
		if outputs["gpu-burn-measurements"] == "later mutable Workflow output" {
			t.Fatal("later Workflow output replaced the immutable attempt snapshot")
		}
	}
}

func TestUntrustedNodeStatusIsPreservedBeforeForegroundCleanup(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"offloaded", "malformed compressed", "canceled offloaded"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			run := builtinTestRun(t, BurnCanaryWorkflow)
			workflow := builtinWorkflow(run, "Succeeded")
			workflow.SetFinalizers([]string{"test.unbounded-cloud.io/hold-deletion"})
			status := workflow.Object["status"].(map[string]any)
			reason := "WorkflowNodesOffloaded"

			if mode == "malformed compressed" {
				status["compressedNodes"] = "%%%"
				reason = "WorkflowNodesCompressedMalformed"
			} else {
				status["offloadNodeStatusVersion"] = "unhydrated-version"
			}

			if mode == "canceled offloaded" {
				run.Spec.Cancel = true
				status["phase"] = "Running"
			}

			expectedMarkers := workflowNodeStatus(workflow)
			kube := fake.NewClientBuilder().
				WithScheme(testScheme(t)).
				WithStatusSubresource(&signalv1.CertificationRun{}).
				WithObjects(run, workflow, reservedTestNode(run), runSnapshot(run)).
				WithInterceptorFuncs(interceptor.Funcs{
					Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
						if obj.GetObjectKind().GroupVersionKind() == workflowGVK {
							current := getTestRun(t, c, run)
							if len(current.Status.Attempts) != 1 || current.Status.Attempts[0].Evidence.URI == "" ||
								current.Status.Evidence.URI == "" {
								return errors.New("Workflow deletion attempted before preserving representation evidence")
							}
						}

						return c.Delete(ctx, obj, opts...)
					},
				}).
				Build()
			r := evidenceReconciler(t, kube)

			reconcileRun(t, r, run)

			got := getTestRun(t, kube, run)
			if len(got.Status.Attempts) != 1 || got.Status.Attempts[0].Verdict != "Inconclusive" ||
				got.Status.Cleanup.ReservationsReleased ||
				mode != "canceled offloaded" && got.Status.Verdict != "Inconclusive" {
				t.Fatalf("unhydrated status completed unsafely: %#v", got.Status)
			}

			for _, evidence := range []signalv1.EvidenceStatus{got.Status.Attempts[0].Evidence, got.Status.Evidence} {
				doc := readTestEvidence(t, kube, evidence)
				if !reflect.DeepEqual(doc["workflowNodeStatus"], expectedMarkers) ||
					!strings.HasPrefix(doc["workflowNodesError"].(string), reason) || doc["workflowNodes"] != nil {
					t.Fatalf("untrusted representation was lost or treated as complete: %#v", doc)
				}
			}

			reconcileRun(t, r, run)
			assertTestReservation(t, kube, run, run.Name)

			current := &unstructured.Unstructured{}
			current.SetGroupVersionKind(workflowGVK)

			if err := kube.Get(t.Context(), client.ObjectKeyFromObject(workflow), current); err != nil {
				t.Fatal(err)
			}

			if current.GetDeletionTimestamp().IsZero() {
				t.Fatal("unavailable node status did not force the foreground deletion barrier")
			}

			current.SetFinalizers(nil)

			if err := kube.Update(t.Context(), current); err != nil {
				t.Fatal(err)
			}

			reconcileRun(t, r, run)
			reconcileRun(t, r, run)
			assertTestReservation(t, kube, run, "")
		})
	}
}
