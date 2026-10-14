// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
)

func TestSingleNodeCleanupRecoversWorkflowCreatedBeforeStatusCheckpoint(t *testing.T) {
	t.Parallel()

	for _, action := range []string{"cancel", "delete"} {
		for _, attempt := range []int32{1, 2} {
			t.Run(fmt.Sprintf("%s/attempt=%d", action, attempt), func(t *testing.T) {
				t.Parallel()

				run := testRun("uncheckpointed")

				run.Spec.Workflow.Name = "project-signal-gpu-certification"
				if attempt > 1 {
					run.Status.Phase = signalv1.RunPhaseRunning
					run.Status.Workflow = signalv1.WorkflowStatus{
						Name: run.Name + "-a1", UID: "previous-workflow-uid", Attempt: 1, Phase: "Failed",
					}
				}

				node := readyNode(run.Spec.RequestNodeName, "A100")
				node.Labels[RequestLabel] = run.Spec.Profile.Name
				node.Annotations = map[string]string{ReservationLabel: run.Name}
				failCheckpoint := true
				kube := fake.NewClientBuilder().
					WithScheme(testScheme(t)).
					WithStatusSubresource(&signalv1.CertificationRun{}).
					WithObjects(run, node, runSnapshot(run)).
					WithInterceptorFuncs(interceptor.Funcs{
						Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
							if workflow, ok := obj.(*unstructured.Unstructured); ok && workflow.GroupVersionKind() == workflowGVK {
								workflow.SetUID(types.UID("uncheckpointed-workflow-uid"))
								workflow.SetFinalizers([]string{"test.signal.example/foreground-descendants"})

								if err := unstructured.SetNestedField(workflow.Object, "Running", "status", "phase"); err != nil {
									return err
								}
							}

							return c.Create(ctx, obj, opts...)
						},
						SubResourcePatch: func(
							ctx context.Context,
							c client.Client,
							subresource string,
							obj client.Object,
							patch client.Patch,
							opts ...client.SubResourcePatchOption,
						) error {
							if updated, ok := obj.(*signalv1.CertificationRun); ok &&
								failCheckpoint && updated.Status.Workflow.Attempt == attempt {
								failCheckpoint = false

								return errors.New("injected Workflow checkpoint failure")
							}

							return c.SubResource(subresource).Patch(ctx, obj, patch, opts...)
						},
						Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
							if obj.GetObjectKind().GroupVersionKind() == workflowGVK {
								options := &client.DeleteOptions{}
								options.ApplyOptions(opts)

								if options.PropagationPolicy == nil || *options.PropagationPolicy != metav1.DeletePropagationForeground ||
									options.Preconditions == nil || options.Preconditions.UID == nil ||
									*options.Preconditions.UID != obj.GetUID() {
									return errors.New("workflow cleanup lost foreground propagation or pinned UID")
								}
							}

							return c.Delete(ctx, obj, opts...)
						},
					}).
					Build()

				r := &RunReconciler{Client: kube, Evidence: &memoryEvidenceStore{}}
				if _, err := r.startAttempt(t.Context(), run.DeepCopy(), attempt); err == nil ||
					!strings.Contains(err.Error(), "injected Workflow checkpoint failure") {
					t.Fatalf("create-before-checkpoint reproduction = %v", err)
				}

				var current signalv1.CertificationRun
				if err := kube.Get(t.Context(), client.ObjectKeyFromObject(run), &current); err != nil {
					t.Fatal(err)
				}

				if current.Status.Workflow.Attempt == attempt {
					t.Fatal("failed checkpoint unexpectedly persisted the Workflow identity")
				}

				if action == "cancel" {
					before := current.DeepCopy()

					current.Spec.Cancel = true
					if err := kube.Patch(t.Context(), &current, client.MergeFrom(before)); err != nil {
						t.Fatal(err)
					}
				} else if err := kube.Delete(t.Context(), &current); err != nil {
					t.Fatal(err)
				}

				req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}

				result, err := r.Reconcile(t.Context(), req)
				if err != nil || result.RequeueAfter == 0 {
					t.Fatalf("cleanup did not wait for foreground descendants: %#v, %v", result, err)
				}

				if err := kube.Get(t.Context(), req.NamespacedName, &current); err != nil {
					t.Fatal(err)
				}

				if current.Status.Workflow.Attempt != attempt || current.Status.Workflow.UID != "uncheckpointed-workflow-uid" ||
					current.Status.Phase.Terminal() || !containsString(current.Finalizers, RunFinalizer) {
					t.Fatalf("uncheckpointed Workflow was not recovered before cleanup: %#v", current.Status)
				}

				if current.Status.Evidence.URI == "" {
					t.Fatalf("interrupted Workflow evidence was not retained: %#v", current.Status)
				}

				var reserved corev1.Node
				if err := kube.Get(t.Context(), client.ObjectKeyFromObject(node), &reserved); err != nil {
					t.Fatal(err)
				}

				if reserved.Annotations[ReservationLabel] != run.Name {
					t.Fatal("reservation was released while foreground descendants remained")
				}

				workflow := &unstructured.Unstructured{}
				workflow.SetGroupVersionKind(workflowGVK)

				if err := kube.Get(t.Context(), client.ObjectKey{Namespace: run.Namespace, Name: current.Status.Workflow.Name}, workflow); err != nil {
					t.Fatal(err)
				}

				if workflow.GetDeletionTimestamp().IsZero() {
					t.Fatal("recovered Workflow was not foreground-deleted")
				}

				workflow.SetFinalizers(nil)

				if err := kube.Update(t.Context(), workflow); err != nil {
					t.Fatal(err)
				}

				for range 2 {
					if _, err := r.Reconcile(t.Context(), req); err != nil {
						t.Fatal(err)
					}
				}

				if err := kube.Get(t.Context(), client.ObjectKeyFromObject(node), &reserved); err != nil {
					t.Fatal(err)
				}

				if reserved.Annotations[ReservationLabel] != "" || reserved.Labels[RequestLabel] != "" {
					t.Fatalf("cleanup left reservation or request: %#v", reserved.ObjectMeta)
				}

				var snapshot corev1.ConfigMap
				if err := kube.Get(t.Context(), client.ObjectKey{Namespace: run.Namespace, Name: run.Spec.Script.ConfigMapName}, &snapshot); !apierrors.IsNotFound(err) {
					t.Fatalf("snapshot remained after finalization: %v", err)
				}
			})
		}
	}
}

func TestSingleNodeWorkflowRecoveryReadFailureRetainsReservation(t *testing.T) {
	t.Parallel()

	run := testRun("recovery-read-failure")
	run.Spec.Cancel = true
	node := readyNode(run.Spec.RequestNodeName, "A100")
	node.Annotations = map[string]string{ReservationLabel: run.Name}
	kube := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run, node, runSnapshot(run)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if list.GetObjectKind().GroupVersionKind() == workflowGVK.GroupVersion().WithKind("WorkflowList") {
					return errors.New("injected Workflow recovery read failure")
				}

				return c.List(ctx, list, opts...)
			},
		}).
		Build()

	r := &RunReconciler{Client: kube, Evidence: &memoryEvidenceStore{}}
	if _, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err == nil ||
		!strings.Contains(err.Error(), "injected Workflow recovery read failure") {
		t.Fatalf("recovery read failure = %v", err)
	}

	var current corev1.Node
	if err := kube.Get(t.Context(), client.ObjectKeyFromObject(node), &current); err != nil {
		t.Fatal(err)
	}

	if current.Annotations[ReservationLabel] != run.Name {
		t.Fatal("recovery read failure released the reservation")
	}
}

func TestSingleNodeCleanupRecoversEvidenceWrittenBeforeStatusCheckpoint(t *testing.T) {
	t.Parallel()

	for _, action := range []string{"cancel", "delete"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()

			run := testRun("evidence-checkpoint")
			run.Spec.Cancel = action == "cancel"
			run.Status.Phase = signalv1.RunPhaseRunning
			workflow := workflowObject(run.Namespace, run.Name+"-a1", "Running", "", "")
			workflow.SetUID("evidence-workflow-uid")
			workflow.SetFinalizers([]string{"test.signal.example/foreground-descendants"})
			workflow.SetOwnerReferences([]metav1.OwnerReference{*metav1.NewControllerRef(run, signalv1.GroupVersion.WithKind("CertificationRun"))})
			run.Status.Workflow = signalv1.WorkflowStatus{Name: workflow.GetName(), UID: string(workflow.GetUID()), Attempt: 1, Phase: "Running"}
			node := readyNode(run.Spec.RequestNodeName, "A100")
			node.Labels[RequestLabel] = run.Spec.Profile.Name
			node.Annotations = map[string]string{ReservationLabel: run.Name}
			failCheckpoint := true
			kube := fake.NewClientBuilder().
				WithScheme(testScheme(t)).
				WithStatusSubresource(&signalv1.CertificationRun{}).
				WithObjects(run, node, workflow, runSnapshot(run)).
				WithInterceptorFuncs(interceptor.Funcs{
					SubResourcePatch: func(
						ctx context.Context,
						c client.Client,
						subresource string,
						obj client.Object,
						patch client.Patch,
						opts ...client.SubResourcePatchOption,
					) error {
						if updated, ok := obj.(*signalv1.CertificationRun); ok && failCheckpoint && updated.Status.Evidence.URI != "" {
							failCheckpoint = false

							return errors.New("injected evidence checkpoint failure")
						}

						return c.SubResource(subresource).Patch(ctx, obj, patch, opts...)
					},
				}).
				Build()

			if action == "delete" {
				if err := kube.Delete(t.Context(), run); err != nil {
					t.Fatal(err)
				}
			}

			store, err := NewKubernetesEvidenceStore(kube, run.Namespace)
			if err != nil {
				t.Fatal(err)
			}

			r := &RunReconciler{Client: kube, Evidence: store}

			req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}
			if _, err := r.Reconcile(t.Context(), req); err == nil || !strings.Contains(err.Error(), "injected evidence checkpoint failure") {
				t.Fatalf("evidence write-before-checkpoint reproduction = %v", err)
			}

			var evidence corev1.ConfigMap

			key := client.ObjectKey{Namespace: run.Namespace, Name: run.Name + "-evidence"}
			if err := kube.Get(t.Context(), key, &evidence); err != nil {
				t.Fatal(err)
			}

			original := bytes.Clone(evidence.BinaryData["evidence.json.gz"])

			if err := kube.Get(t.Context(), client.ObjectKeyFromObject(workflow), workflow); err != nil {
				t.Fatal(err)
			}

			if err := unstructured.SetNestedField(workflow.Object, "Succeeded", "status", "phase"); err != nil {
				t.Fatal(err)
			}

			if err := kube.Update(t.Context(), workflow); err != nil {
				t.Fatal(err)
			}

			result, err := r.Reconcile(t.Context(), req)
			if err != nil || result.RequeueAfter == 0 {
				t.Fatalf("saved evidence was not recovered before waiting for foreground descendants: %#v, %v", result, err)
			}

			var current signalv1.CertificationRun
			if err := kube.Get(t.Context(), req.NamespacedName, &current); err != nil {
				t.Fatal(err)
			}

			if current.Status.Evidence.URI == "" || current.Status.Phase.Terminal() {
				t.Fatalf("evidence checkpoint or foreground barrier was lost: %#v", current.Status)
			}

			if err := kube.Get(t.Context(), key, &evidence); err != nil {
				t.Fatal(err)
			}

			if !bytes.Equal(original, evidence.BinaryData["evidence.json.gz"]) {
				t.Fatal("Workflow progress rewrote the immutable interruption evidence")
			}

			if err := kube.Get(t.Context(), client.ObjectKeyFromObject(workflow), workflow); err != nil {
				t.Fatal(err)
			}

			workflow.SetFinalizers(nil)

			if err := kube.Update(t.Context(), workflow); err != nil {
				t.Fatal(err)
			}

			for range 2 {
				if _, err := r.Reconcile(t.Context(), req); err != nil {
					t.Fatal(err)
				}
			}

			if err := kube.Get(t.Context(), client.ObjectKeyFromObject(node), node); err != nil {
				t.Fatal(err)
			}

			if node.Annotations[ReservationLabel] != "" {
				t.Fatal("recovered evidence left the finalized reservation pinned")
			}
		})
	}
}

func TestSingleNodeWorkflowRecoveryRejectsChangedUIDAndUnrecognizedOwnedIdentity(t *testing.T) {
	t.Parallel()

	for _, identity := range []string{"changed UID", "noncanonical name", "out-of-range attempt"} {
		t.Run(identity, func(t *testing.T) {
			t.Parallel()

			run := testRun("invalid-owned-workflow")
			run.Spec.Cancel = true
			workflow := workflowObject(run.Namespace, run.Name+"-a1", "Running", "", "")
			workflow.SetUID("actual-workflow-uid")
			workflow.SetOwnerReferences([]metav1.OwnerReference{*metav1.NewControllerRef(run, signalv1.GroupVersion.WithKind("CertificationRun"))})

			switch identity {
			case "changed UID":
				run.Status.Workflow = signalv1.WorkflowStatus{Name: workflow.GetName(), UID: "original-workflow-uid", Attempt: 1}
			case "noncanonical name":
				workflow.SetName(run.Name + "-unexpected")
			case "out-of-range attempt":
				workflow.SetName(run.Name + "-a3")
			}

			node := readyNode(run.Spec.RequestNodeName, "A100")
			node.Annotations = map[string]string{ReservationLabel: run.Name}
			kube := fake.NewClientBuilder().
				WithScheme(testScheme(t)).
				WithStatusSubresource(&signalv1.CertificationRun{}).
				WithObjects(run, node, workflow, runSnapshot(run)).
				Build()

			r := &RunReconciler{Client: kube, Evidence: &memoryEvidenceStore{}}
			if _, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err == nil {
				t.Fatalf("%s was treated as proof that owned execution stopped", identity)
			}

			if err := kube.Get(t.Context(), client.ObjectKeyFromObject(workflow), workflow); err != nil {
				t.Fatal(err)
			}

			if !workflow.GetDeletionTimestamp().IsZero() {
				t.Fatal("an unverified Workflow identity was deleted")
			}

			var reserved corev1.Node
			if err := kube.Get(t.Context(), client.ObjectKeyFromObject(node), &reserved); err != nil {
				t.Fatal(err)
			}

			if reserved.Annotations[ReservationLabel] != run.Name {
				t.Fatal("unverified Workflow identity released the reservation")
			}
		})
	}
}

func TestSingleNodeEvidenceRecoveryRejectsForeignRunOrChangedSnapshot(t *testing.T) {
	t.Parallel()

	for _, identity := range []string{"run UID", "Workflow UID", "parameters"} {
		t.Run(identity, func(t *testing.T) {
			t.Parallel()

			run := testRun("foreign-evidence")
			run.Spec.Cancel = true
			run.Status.Phase = signalv1.RunPhaseRunning
			workflow := workflowObject(run.Namespace, run.Name+"-a1", "Running", "", "")
			workflow.SetUID("evidence-workflow-uid")
			workflow.SetOwnerReferences([]metav1.OwnerReference{*metav1.NewControllerRef(run, signalv1.GroupVersion.WithKind("CertificationRun"))})
			run.Status.Workflow = signalv1.WorkflowStatus{Name: workflow.GetName(), UID: string(workflow.GetUID()), Attempt: 1, Phase: "Running"}
			node := readyNode(run.Spec.RequestNodeName, "A100")
			node.Annotations = map[string]string{ReservationLabel: run.Name}
			kube := fake.NewClientBuilder().
				WithScheme(testScheme(t)).
				WithStatusSubresource(&signalv1.CertificationRun{}).
				WithObjects(run, node, workflow, runSnapshot(run)).
				Build()

			store, err := NewKubernetesEvidenceStore(kube, run.Namespace)
			if err != nil {
				t.Fatal(err)
			}

			r := &RunReconciler{Client: kube, Evidence: store}

			data, err := r.evidenceDocument(t.Context(), run, "Interrupted", "Inconclusive", "canceled by operator")
			if err != nil {
				t.Fatal(err)
			}

			var doc map[string]any
			if err := json.Unmarshal(data, &doc); err != nil {
				t.Fatal(err)
			}

			switch identity {
			case "run UID":
				doc["runID"] = "another-run-uid"
			case "Workflow UID":
				if err := unstructured.SetNestedField(doc, "another-workflow-uid", "workflow", "uid"); err != nil {
					t.Fatal(err)
				}
			case "parameters":
				if err := unstructured.SetNestedField(doc, map[string]any{"untrusted": "input"}, "runSpec", "parameters"); err != nil {
					t.Fatal(err)
				}
			}

			data, err = json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}

			if _, _, err := store.Put(t.Context(), run.Spec.EvidencePath, data); err != nil {
				t.Fatal(err)
			}

			if _, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err == nil ||
				!strings.Contains(err.Error(), "existing evidence does not match") {
				t.Fatalf("mismatched %s evidence was adopted: %v", identity, err)
			}

			if err := kube.Get(t.Context(), client.ObjectKeyFromObject(workflow), workflow); err != nil {
				t.Fatal(err)
			}

			if !workflow.GetDeletionTimestamp().IsZero() {
				t.Fatal("mismatched evidence permitted Workflow cleanup")
			}

			if err := kube.Get(t.Context(), client.ObjectKeyFromObject(node), node); err != nil {
				t.Fatal(err)
			}

			if node.Annotations[ReservationLabel] != run.Name {
				t.Fatal("mismatched evidence released the reservation")
			}
		})
	}
}

func TestSingleNodeTerminalEvidenceRecoversAfterStatusCheckpointFailure(t *testing.T) {
	t.Parallel()

	run := testRun("terminal-evidence-checkpoint")
	run.Status.Phase = signalv1.RunPhaseRunning
	workflow := workflowObject(run.Namespace, run.Name+"-a1", "Succeeded", "Pass", "Certified")
	workflow.SetUID("terminal-workflow-uid")
	workflow.SetOwnerReferences([]metav1.OwnerReference{*metav1.NewControllerRef(run, signalv1.GroupVersion.WithKind("CertificationRun"))})
	run.Status.Workflow = signalv1.WorkflowStatus{Name: workflow.GetName(), UID: string(workflow.GetUID()), Attempt: 1, Phase: "Succeeded"}
	node := readyNode(run.Spec.RequestNodeName, "A100")
	node.Annotations = map[string]string{ReservationLabel: run.Name}
	failCheckpoint := true
	kube := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run, node, workflow, runSnapshot(run)).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(
				ctx context.Context,
				c client.Client,
				subresource string,
				obj client.Object,
				patch client.Patch,
				opts ...client.SubResourcePatchOption,
			) error {
				if updated, ok := obj.(*signalv1.CertificationRun); ok && failCheckpoint && updated.Status.Phase.Terminal() {
					failCheckpoint = false

					return errors.New("injected terminal evidence checkpoint failure")
				}

				return c.SubResource(subresource).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	store, err := NewKubernetesEvidenceStore(kube, run.Namespace)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	r := &RunReconciler{Client: kube, Evidence: store, Now: func() time.Time { return now }}

	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}
	if _, err := r.Reconcile(t.Context(), req); err == nil ||
		!strings.Contains(err.Error(), "injected terminal evidence checkpoint failure") {
		t.Fatalf("terminal evidence write-before-checkpoint reproduction = %v", err)
	}

	original, uri, digest, err := store.Get(t.Context(), run.Spec.EvidencePath)
	if err != nil {
		t.Fatal(err)
	}

	now = now.Add(time.Second)

	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatal(err)
	}

	var current signalv1.CertificationRun
	if err := kube.Get(t.Context(), req.NamespacedName, &current); err != nil {
		t.Fatal(err)
	}

	if current.Status.Phase != signalv1.RunPhaseSucceeded || current.Status.Evidence.URI != uri || current.Status.Evidence.Digest != digest {
		t.Fatalf("terminal evidence was not recovered: %#v", current.Status)
	}

	recovered, _, _, err := store.Get(t.Context(), run.Spec.EvidencePath)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(recovered, original) {
		t.Fatal("terminal evidence was rewritten with the retry's completion time")
	}
}

func TestSingleNodeWorkflowRecoveryDoesNotAdoptForeignWorkflow(t *testing.T) {
	t.Parallel()

	run := testRun("foreign-workflow")
	run.Spec.Cancel = true
	workflow := workflowObject(run.Namespace, run.Name+"-a1", "Running", "", "")
	workflow.SetUID("foreign-workflow-uid")

	other := testRun("other-run")
	workflow.SetOwnerReferences([]metav1.OwnerReference{*metav1.NewControllerRef(other, signalv1.GroupVersion.WithKind("CertificationRun"))})

	node := readyNode(run.Spec.RequestNodeName, "A100")
	node.Annotations = map[string]string{ReservationLabel: run.Name}
	kube := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run, node, workflow, runSnapshot(run)).
		Build()
	r := &RunReconciler{Client: kube, Evidence: &memoryEvidenceStore{}}

	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}
	for range 2 {
		if _, err := r.Reconcile(t.Context(), req); err != nil {
			t.Fatal(err)
		}
	}

	if err := kube.Get(t.Context(), client.ObjectKeyFromObject(workflow), workflow); err != nil {
		t.Fatal(err)
	}

	if !workflow.GetDeletionTimestamp().IsZero() {
		t.Fatal("another Run's Workflow was deleted")
	}

	var current signalv1.CertificationRun
	if err := kube.Get(t.Context(), client.ObjectKeyFromObject(run), &current); err != nil {
		t.Fatal(err)
	}

	if current.Status.Workflow.Name != "" || !current.Status.Cleanup.ReservationsReleased {
		t.Fatalf("foreign Workflow was adopted or blocked unrelated cleanup: %#v", current.Status)
	}
}

func TestSingleNodeCleanupUsesAuthoritativeWorkflowReadWhenCacheMisses(t *testing.T) {
	t.Parallel()

	run := testRun("uncached-cleanup")
	run.Spec.Workflow.Name = "project-signal-gpu-certification"
	run.Spec.Cancel = true
	node := readyNode(run.Spec.RequestNodeName, "A100")
	node.Annotations = map[string]string{ReservationLabel: run.Name}
	workflow := workflowObject(run.Namespace, run.Name+"-a1", "Running", "", "")
	workflow.SetUID("uncached-workflow-uid")
	workflow.SetOwnerReferences([]metav1.OwnerReference{*metav1.NewControllerRef(run, signalv1.GroupVersion.WithKind("CertificationRun"))})
	workflow.SetFinalizers([]string{"test.signal.example/foreground-descendants"})

	if err := unstructured.SetNestedMap(workflow.Object, map[string]any{
		"leaf": map[string]any{"id": "leaf", "name": "running-task", "type": "Pod", "phase": "Running"},
	}, "status", "nodes"); err != nil {
		t.Fatal(err)
	}

	authoritative := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run, node, workflow, runSnapshot(run)).
		Build()
	cached := interceptor.NewClient(authoritative, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if obj.GetObjectKind().GroupVersionKind() == workflowGVK {
				return apierrors.NewNotFound(schema.GroupResource{Group: workflowGVK.Group, Resource: "workflows"}, key.Name)
			}

			return c.Get(ctx, key, obj, opts...)
		},
	})
	evidence := &memoryEvidenceStore{}
	r := &RunReconciler{Client: cached, Reader: authoritative, Evidence: evidence}

	result, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("stale cache bypassed authoritative foreground cleanup: %#v, %v", result, err)
	}

	if err := authoritative.Get(t.Context(), client.ObjectKeyFromObject(workflow), workflow); err != nil {
		t.Fatal(err)
	}

	if workflow.GetDeletionTimestamp().IsZero() {
		t.Fatal("live Workflow was not foreground-deleted")
	}

	var reserved corev1.Node
	if err := authoritative.Get(t.Context(), client.ObjectKeyFromObject(node), &reserved); err != nil {
		t.Fatal(err)
	}

	if reserved.Annotations[ReservationLabel] != run.Name {
		t.Fatal("stale Workflow cache released the reservation")
	}

	var retained struct {
		WorkflowNodes map[string]any `json:"workflowNodes"`
	}
	if err := json.Unmarshal(evidence.data, &retained); err != nil {
		t.Fatal(err)
	}

	leaf, ok := retained.WorkflowNodes["leaf"].(map[string]any)
	if !ok || leaf["name"] != "running-task" || leaf["phase"] != "Running" {
		t.Fatalf("stale cache discarded the interrupted Workflow nodes: %#v", retained.WorkflowNodes)
	}
}

func TestSingleNodeCleanupReadErrorsRetainInputs(t *testing.T) {
	t.Parallel()

	for _, failure := range []string{"interrupted evidence", "deletion confirmation"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()

			run := testRun("cleanup-read-error")
			run.Spec.Cancel = true
			node := readyNode(run.Spec.RequestNodeName, "A100")
			node.Annotations = map[string]string{ReservationLabel: run.Name}
			workflow := workflowObject(run.Namespace, run.Name+"-a1", "Running", "", "")
			workflow.SetUID("workflow-uid")
			workflow.SetOwnerReferences([]metav1.OwnerReference{*metav1.NewControllerRef(run, signalv1.GroupVersion.WithKind("CertificationRun"))})
			workflow.SetFinalizers([]string{"test.signal.example/foreground-descendants"})

			kube := fake.NewClientBuilder().
				WithScheme(testScheme(t)).
				WithStatusSubresource(&signalv1.CertificationRun{}).
				WithObjects(run, node, workflow, runSnapshot(run)).
				Build()
			reader := interceptor.NewClient(kube, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if err := c.Get(ctx, key, obj, opts...); err != nil {
						return err
					}

					if obj.GetObjectKind().GroupVersionKind() == workflowGVK &&
						(failure == "interrupted evidence" || !obj.GetDeletionTimestamp().IsZero()) {
						return errors.New("injected authoritative " + failure + " read failure")
					}

					return nil
				},
			})
			store := &memoryEvidenceStore{}
			r := &RunReconciler{Client: kube, Reader: reader, Evidence: store}

			if _, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err == nil ||
				!strings.Contains(err.Error(), "injected authoritative "+failure+" read failure") {
				t.Fatalf("authoritative read error = %v", err)
			}

			var current signalv1.CertificationRun
			if err := kube.Get(t.Context(), client.ObjectKeyFromObject(run), &current); err != nil {
				t.Fatal(err)
			}

			if current.Status.Phase.Terminal() || !containsString(current.Finalizers, RunFinalizer) {
				t.Fatal("read failure completed or finalized the Run")
			}

			var reserved corev1.Node
			if err := kube.Get(t.Context(), client.ObjectKeyFromObject(node), &reserved); err != nil {
				t.Fatal(err)
			}

			if reserved.Annotations[ReservationLabel] != run.Name {
				t.Fatal("read failure released the reservation")
			}

			var snapshot corev1.ConfigMap
			if err := kube.Get(t.Context(), client.ObjectKey{Namespace: run.Namespace, Name: run.Spec.Script.ConfigMapName}, &snapshot); err != nil {
				t.Fatalf("read failure discarded the script snapshot: %v", err)
			}

			if failure == "interrupted evidence" && (len(store.data) != 0 || current.Status.Evidence.URI != "") {
				t.Fatal("unreadable Workflow was replaced by success-shaped evidence")
			}
		})
	}
}

func TestSingleNodeRecoveryPreservesCheckpointedLegacyWorkflowIdentity(t *testing.T) {
	t.Parallel()

	run := testRun("legacy-workflow")
	run.Spec.Cancel = true
	run.Status.Phase = signalv1.RunPhaseRunning
	run.Status.Workflow = signalv1.WorkflowStatus{Name: "previously-checkpointed-name", UID: "legacy-uid", Attempt: 1, Phase: "Running"}
	workflow := workflowObject(run.Namespace, run.Status.Workflow.Name, "Running", "", "")
	workflow.SetUID(types.UID(run.Status.Workflow.UID))
	workflow.SetOwnerReferences([]metav1.OwnerReference{*metav1.NewControllerRef(run, signalv1.GroupVersion.WithKind("CertificationRun"))})
	workflow.SetFinalizers([]string{"test.signal.example/foreground-descendants"})

	node := readyNode(run.Spec.RequestNodeName, "A100")
	node.Annotations = map[string]string{ReservationLabel: run.Name}
	kube := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run, node, workflow, runSnapshot(run)).
		Build()
	r := &RunReconciler{Client: kube, Evidence: &memoryEvidenceStore{}}

	result, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("checkpointed legacy identity was rejected or bypassed: %#v, %v", result, err)
	}

	var current signalv1.CertificationRun
	if err := kube.Get(t.Context(), client.ObjectKeyFromObject(run), &current); err != nil {
		t.Fatal(err)
	}

	if current.Status.Workflow != run.Status.Workflow || current.Status.Evidence.URI == "" || current.Status.Phase.Terminal() {
		t.Fatalf("legacy identity changed during foreground cleanup: %#v", current.Status)
	}
}

func TestFailedAdmissionRetainsSnapshotAndReservationUntilRunFinalizes(t *testing.T) {
	t.Parallel()

	for _, failure := range []string{"snapshot binding", "pending projection"} {
		for _, denyDelete := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/delete-denied=%t", failure, denyDelete), func(t *testing.T) {
				t.Parallel()

				node := readyNode("gpu-0", "example-gpu")
				node.Labels[RequestLabel] = "gpu-smoke"
				profile := testProfile("gpu-smoke", "example-gpu")
				source := &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Name: "scripts", Namespace: "signal"},
					Data:       map[string]string{"payload.txt": "test payload\n"},
				}
				granted := controllerRunDeleteGranted(t, "signal")
				deleteAllowed := granted && !denyDelete
				kube := fake.NewClientBuilder().
					WithScheme(testScheme(t)).
					WithStatusSubresource(&signalv1.CertificationRun{}).
					WithObjects(node, profile, source).
					WithInterceptorFuncs(interceptor.Funcs{
						Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
							if run, ok := obj.(*signalv1.CertificationRun); ok {
								run.UID = "failed-admission-run-uid"
							}

							return c.Create(ctx, obj, opts...)
						},
						Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
							if snapshot, ok := obj.(*corev1.ConfigMap); ok && snapshot.Name != source.Name && failure == "snapshot binding" {
								return errors.New("injected snapshot binding failure")
							}

							return c.Update(ctx, obj, opts...)
						},
						Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
							if projected, ok := obj.(*corev1.Node); ok &&
								failure == "pending projection" && projected.Labels[StateLabel] == "pending" {
								return errors.New("injected pending projection failure")
							}

							return c.Patch(ctx, obj, patch, opts...)
						},
						Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
							if run, ok := obj.(*signalv1.CertificationRun); ok {
								if !deleteAllowed || run.Namespace != "signal" {
									return apierrors.NewForbidden(schema.GroupResource{
										Group: signalv1.GroupVersion.Group, Resource: "certificationruns",
									}, run.Name, errors.New("controller Run delete permission denied"))
								}

								options := &client.DeleteOptions{}
								options.ApplyOptions(opts)

								if run.DeletionTimestamp.IsZero() && (options.Preconditions == nil ||
									options.Preconditions.UID == nil || *options.Preconditions.UID != run.UID) {
									return errors.New("failed admission Run deletion lost its pinned UID")
								}
							}

							return c.Delete(ctx, obj, opts...)
						},
					}).
					Build()
				admission := &AdmissionReconciler{
					Client: kube, Namespace: "signal", ScriptNamespace: "signal",
					Now: func() time.Time { return time.Unix(100, 0) },
				}

				_, err := admission.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(node)})
				if err == nil || !strings.Contains(err.Error(), "injected "+failure+" failure") {
					t.Fatalf("admission failure = %v", err)
				}

				if denyDelete && !apierrors.IsForbidden(err) && !strings.Contains(err.Error(), "delete permission denied") {
					t.Fatalf("Run deletion denial was hidden: %v", err)
				}

				var runs signalv1.CertificationRunList
				if err := kube.List(t.Context(), &runs); err != nil || len(runs.Items) != 1 {
					t.Fatalf("surviving finalizer Run = %d, %v", len(runs.Items), err)
				}

				run := &runs.Items[0]

				var snapshot corev1.ConfigMap
				if err := kube.Get(t.Context(), client.ObjectKey{Namespace: run.Namespace, Name: run.Spec.Script.ConfigMapName}, &snapshot); err != nil {
					t.Fatalf("failed admission discarded a surviving Run's snapshot: %v", err)
				}

				if snapshot.Data[run.Spec.Script.Key] != source.Data[profile.Spec.Script.Key] {
					t.Fatal("retained immutable snapshot content changed")
				}

				var reserved corev1.Node
				if err := kube.Get(t.Context(), client.ObjectKeyFromObject(node), &reserved); err != nil {
					t.Fatal(err)
				}

				if reserved.Annotations[ReservationLabel] != run.Name {
					t.Fatal("failed admission released a surviving Run's reservation")
				}

				if !granted {
					t.Fatal("rendered controller RBAC does not grant namespaced CertificationRun deletion")
				}

				if !denyDelete && run.DeletionTimestamp.IsZero() {
					t.Fatal("failed admission left its Run active")
				}

				deleteAllowed = granted

				if run.DeletionTimestamp.IsZero() {
					if err := kube.Delete(t.Context(), run, client.Preconditions{UID: &run.UID}); err != nil {
						t.Fatal(err)
					}
				}

				r := &RunReconciler{Client: kube}
				if _, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
					t.Fatal(err)
				}

				if err := kube.Get(t.Context(), client.ObjectKeyFromObject(&snapshot), &snapshot); !apierrors.IsNotFound(err) {
					t.Fatalf("unbound snapshot survived Run finalization: %v", err)
				}

				if err := kube.Get(t.Context(), client.ObjectKeyFromObject(node), &reserved); err != nil {
					t.Fatal(err)
				}

				if reserved.Annotations[ReservationLabel] != "" || reserved.Labels[RequestLabel] != "" {
					t.Fatal("finalized failed admission retained its reservation or request")
				}
			})
		}
	}
}

func controllerRunDeleteGranted(t *testing.T, namespace string) bool {
	t.Helper()

	output := t.TempDir()
	if err := render.Render("../../deploy/signal", output, map[string]string{"Namespace": namespace}); err != nil {
		t.Fatal(err)
	}

	file, err := os.Open(filepath.Join(output, "02-rbac.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	granted := false
	decoder := utilyaml.NewYAMLOrJSONDecoder(file, 4096)

	for {
		var object struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Rules []struct {
				APIGroups []string `json:"apiGroups"`
				Resources []string `json:"resources"`
				Verbs     []string `json:"verbs"`
			} `json:"rules"`
		}

		err := decoder.Decode(&object)
		if err == io.EOF {
			break
		}

		if err != nil {
			t.Fatal(err)
		}

		if object.Metadata.Name != "project-signal-controller" {
			continue
		}

		for _, rule := range object.Rules {
			if !slices.Contains(rule.APIGroups, signalv1.GroupVersion.Group) ||
				!slices.Contains(rule.Resources, "certificationruns") || !slices.Contains(rule.Verbs, "delete") {
				continue
			}

			if object.Kind != "Role" || object.Metadata.Namespace != namespace {
				t.Fatal("CertificationRun deletion was granted outside the controller namespace")
			}

			granted = true
		}
	}

	return granted
}

func runSnapshot(run *signalv1.CertificationRun) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: run.Spec.Script.ConfigMapName, Namespace: run.Namespace},
		Data:       map[string]string{run.Spec.Script.Key: "test payload\n"},
	}
}
