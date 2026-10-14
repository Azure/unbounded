// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestExplicitNonmemberRunCannotMutateAnUndeclaredNodeDuringRejectionOrCleanup(t *testing.T) {
	t.Parallel()

	run, nodes := rackTestRun(t, "undeclared-group", FullNodeCertificationWorkflow)
	outsider := readyNode("unapproved-node", "example-gpu")
	outsider.Labels[RequestLabel] = run.Spec.Profile.Name
	outsider.Labels[EligibilityLabel] = "production"
	outsider.Annotations = map[string]string{ReservationLabel: run.Name}
	run.Spec.Participants[1].NodeName = outsider.Name
	run.Spec.Participants[1].NodeUID = outsider.UID
	run.Spec.ParticipantPlan[1] = participantSnapshot(outsider)
	objects := append(rackRunObjects(run, nodes), outsider)
	kube := membershipClientBuilder(t, objects...).Build()

	r := membershipReconciler(t, kube)
	for range 2 {
		if _, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
			t.Fatal(err)
		}
	}

	var untouched corev1.Node
	if err := kube.Get(t.Context(), client.ObjectKeyFromObject(outsider), &untouched); err != nil {
		t.Fatal(err)
	}

	if untouched.Annotations[ReservationLabel] != run.Name ||
		untouched.Labels[EligibilityLabel] != "production" || untouched.Labels[RequestLabel] != run.Spec.Profile.Name {
		t.Fatal("rejection or cleanup mutated an undeclared Node")
	}

	assertRackWorkflowCount(t, kube, 0)
}

func TestDirectRackRunCannotHideGroupedWorkInALegacyParticipantPlan(t *testing.T) {
	t.Parallel()

	run, nodes := rackTestRun(t, "hidden-group", FullNodeCertificationWorkflow)
	run.Spec.Admission = nil
	run.Spec.Participants = run.Spec.Participants[:1]
	run.Spec.TestPlan = nil
	kube := membershipClientBuilder(t, rackRunObjects(run, nodes)...).Build()

	r := membershipReconciler(t, kube)
	if _, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatal(err)
	}

	assertRackWorkflowCount(t, kube, 0)

	got := getTestRun(t, kube, run)
	if !strings.Contains(got.Status.Reason, "immutable admission snapshot") {
		t.Fatalf("hidden group admission reason = %q", got.Status.Reason)
	}
}

func TestLegacySingletonRackRunKeepsEmptyPlanCompatibility(t *testing.T) {
	t.Parallel()

	run, nodes := rackTestRun(t, "legacy-rack-singleton", FullNodeCertificationWorkflow)
	run.Spec.Admission = nil
	run.Spec.Participants = run.Spec.Participants[:1]
	run.Spec.ParticipantPlan = run.Spec.ParticipantPlan[:1]
	run.Spec.TestPlan = nil
	kube := membershipClientBuilder(t, rackRunObjects(run, nodes)...).Build()

	r := membershipReconciler(t, kube)
	if _, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatal(err)
	}

	assertRackWorkflowCount(t, kube, 1)

	if got := getTestRun(t, kube, run); len(got.Spec.TestPlan) != 0 {
		t.Fatal("legacy singleton rack admission inferred a new strict test plan")
	}
}

func TestExplicitTriggerCannotReuseAReservationForADifferentDeclaredSet(t *testing.T) {
	t.Parallel()

	run, nodes := rackTestRun(t, "other-set", FullNodeCertificationWorkflow)
	setRackActive(run, nodes)

	profile, _ := rackProfileAndNodes()
	profile.Spec.Participants.NodeNames = []string{nodes[0].Name, "gpu-2"}
	peer := nodes[1].DeepCopy()
	peer.Name, peer.UID = "gpu-2", "gpu-2-uid"
	peer.Annotations = nil
	objects := append(rackRunObjects(run, nodes), profile, peer)
	kube := membershipClientBuilder(t, objects...).Build()

	r := &AdmissionReconciler{Client: kube, Namespace: run.Namespace}
	if err := r.admitNode(t.Context(), nodes[0], profile.Name); err == nil || !strings.Contains(err.Error(), "different profile or member set") {
		t.Fatalf("changed-set admission error = %v", err)
	}

	assertRackReservations(t, kube, nodes, run.Name)
	assertRackNodeReservation(t, kube, peer.Name, "")
	assertRackWorkflowCount(t, kube, 0)
}

func TestExplicitWorkloadDeadlineUsesThePinnedRunDeadline(t *testing.T) {
	t.Parallel()

	run, nodes := rackTestRun(t, "deadline-group", FullNodeCertificationWorkflow)
	kube := membershipClientBuilder(t, rackRunObjects(run, nodes)...).Build()
	r := membershipReconciler(t, kube)
	start := membershipNow().Add(run.Spec.Deadline.Duration / 2)

	r.Now = func() time.Time { return start }
	if _, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatal(err)
	}

	got := getTestRun(t, kube, run)
	workflow := &unstructured.Unstructured{}
	workflow.SetGroupVersionKind(workflowGVK)

	if err := kube.Get(t.Context(), client.ObjectKey{Namespace: run.Namespace, Name: got.Status.Workflow.Name}, workflow); err != nil {
		t.Fatal(err)
	}

	seconds, _, err := unstructured.NestedInt64(workflow.Object, "spec", "activeDeadlineSeconds")
	if err != nil || seconds != int64(run.Spec.Deadline.Seconds()/2) {
		t.Fatalf("workload deadline = %d, %v; pending time must not reset the pinned deadline", seconds, err)
	}
}
