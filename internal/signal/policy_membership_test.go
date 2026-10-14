// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func TestRecurringExplicitSetRequiresEveryMemberAndExactOptIn(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		change func(*signalv1.CertificationProfile, []*corev1.Node) []client.Object
		want   int
	}{
		{"all members due", nil, 1},
		{"peer busy", func(_ *signalv1.CertificationProfile, nodes []*corev1.Node) []client.Object {
			return []client.Object{&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "peer-workload", Namespace: "application"},
				Spec:       corev1.PodSpec{NodeName: nodes[1].Name},
				Status:     corev1.PodStatus{Phase: corev1.PodRunning},
			}}
		}, 0},
		{"peer not continuously idle", func(profile *signalv1.CertificationProfile, _ []*corev1.Node) []client.Object {
			profile.Status.Nodes[1].IdleSince = ptrMetaTime(membershipNow().Add(-5 * time.Minute))
			return nil
		}, 0},
		{"peer selector mismatch", func(_ *signalv1.CertificationProfile, nodes []*corev1.Node) []client.Object {
			nodes[1].Labels["node.kubernetes.io/instance-type"] = "other"
			return nil
		}, 0},
		{"peer wrong rack", func(_ *signalv1.CertificationProfile, nodes []*corev1.Node) []client.Object {
			nodes[1].Labels[rackTopologyKey] = "rack-b"
			return nil
		}, 0},
		{"partial recurring opt-in", func(profile *signalv1.CertificationProfile, _ []*corev1.Node) []client.Object {
			profile.Spec.Recurring.NodeNames = []string{"gpu-0"}
			return nil
		}, 0},
		{"different recurring opt-in", func(profile *signalv1.CertificationProfile, _ []*corev1.Node) []client.Object {
			profile.Spec.Recurring.NodeNames = []string{"gpu-0", "unapproved"}
			return nil
		}, 0},
		{"duplicate recurring opt-in", func(profile *signalv1.CertificationProfile, _ []*corev1.Node) []client.Object {
			profile.Spec.Recurring.NodeNames = []string{"gpu-0", "gpu-0"}
			return nil
		}, 0},
		{"policy expired", func(profile *signalv1.CertificationProfile, _ []*corev1.Node) []client.Object {
			profile.Spec.Recurring.ExpiresAt = metav1.NewTime(membershipNow())
			return nil
		}, 0},
		{"policy not yet allowed", func(profile *signalv1.CertificationProfile, _ []*corev1.Node) []client.Object {
			profile.Spec.Recurring.NotBefore = ptrMetaTime(membershipNow().Add(time.Hour))
			return nil
		}, 0},
		{"aggregate failure-domain limit", func(profile *signalv1.CertificationProfile, _ []*corev1.Node) []client.Object {
			profile.Spec.Recurring.MaxConcurrentPerFailureDomain = 1
			return nil
		}, 0},
		{"peer failure backoff", func(profile *signalv1.CertificationProfile, nodes []*corev1.Node) []client.Object {
			failed := testRun("peer-failed")
			failed.Spec.Profile.Name = profile.Name
			failed.Spec.Participants = []signalv1.Participant{{NodeName: nodes[1].Name, NodeUID: nodes[1].UID}}
			failed.Status.Phase = signalv1.RunPhaseFailed
			failed.Status.FinishedAt = ptrMetaTime(membershipNow().Add(-time.Minute))

			return []client.Object{failed}
		}, 0},
		{"profile concurrency", func(profile *signalv1.CertificationProfile, _ []*corev1.Node) []client.Object {
			active := testRun("active-other-set")
			active.Spec.Profile.Name = profile.Name
			active.Spec.Participants = []signalv1.Participant{{NodeName: "gpu-other", NodeUID: "gpu-other-uid"}}
			other := readyNode("gpu-other", "example-gpu")
			other.Labels[rackTopologyKey] = "rack-b"

			return []client.Object{active, other}
		}, 0},
		{"terminal cleanup still consumes concurrency", func(profile *signalv1.CertificationProfile, _ []*corev1.Node) []client.Object {
			active := testRun("cleaning-other-set")
			active.Spec.Profile.Name = profile.Name
			active.Spec.Participants = []signalv1.Participant{{NodeName: "gpu-other", NodeUID: "gpu-other-uid"}}
			active.Status.Phase = signalv1.RunPhaseFailed
			active.Status.Workflow = signalv1.WorkflowStatus{Name: "cleaning-workflow", UID: "workflow-uid"}
			other := readyNode("gpu-other", "example-gpu")
			other.Labels[rackTopologyKey] = "rack-b"

			return []client.Object{active, other}
		}, 0},
		{"other namespace does not consume profile budget", func(profile *signalv1.CertificationProfile, _ []*corev1.Node) []client.Object {
			active := testRun("outside-namespace")
			active.Namespace = "other"
			active.Spec.Profile.Name = profile.Name
			active.Spec.Participants = []signalv1.Participant{{NodeName: "gpu-other", NodeUID: "gpu-other-uid"}}

			return []client.Object{active}
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			profile, nodes := recurringRackFixture()

			objects := []client.Object{profile, nodes[0], nodes[1], &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "scripts", Namespace: "signal"},
				Data:       map[string]string{"payload.txt": "test payload\n"},
			}}
			if tc.change != nil {
				objects = append(objects, tc.change(profile, nodes)...)
			}

			kube := membershipClientBuilder(t, objects...).Build()
			admission := &AdmissionReconciler{Client: kube, Namespace: "signal", ScriptNamespace: "signal", Now: membershipNow}
			r := &RecurringReconciler{Client: kube, Reader: kube, Namespace: "signal", Admission: admission, Now: membershipNow}

			req := reconcile.Request{NamespacedName: client.ObjectKey{Name: profile.Name}}
			if _, err := r.Reconcile(t.Context(), req); err != nil {
				t.Fatal(err)
			}

			var runs signalv1.CertificationRunList
			if err := kube.List(t.Context(), &runs, client.InNamespace("signal")); err != nil {
				t.Fatal(err)
			}

			count := 0

			for _, run := range runs.Items {
				if run.Annotations[RunTriggerAnnotation] != "recurring" {
					continue
				}

				count++

				if len(run.Spec.Participants) != 2 || run.Spec.Admission == nil ||
					run.Spec.Admission.Recurring == nil || run.Spec.Admission.IdleSince == nil {
					t.Fatalf("recurring admission started a subset or omitted pinned policy: %#v", run.Spec)
				}
			}

			if count != tc.want {
				t.Fatalf("recurring group Runs = %d, want %d", count, tc.want)
			}

			assertRackWorkflowCount(t, kube, 0)
			assertRackReservations(t, kube, nodes, "")

			var evaluated signalv1.CertificationProfile
			if err := kube.Get(t.Context(), client.ObjectKey{Name: profile.Name}, &evaluated); err != nil {
				t.Fatal(err)
			}

			if tc.want == 0 {
				for _, status := range evaluated.Status.Nodes {
					if status.Reason == "Admitted" {
						t.Fatalf("member was admitted despite the group gate: %#v", evaluated.Status.Nodes)
					}
				}
			} else {
				for _, status := range evaluated.Status.Nodes {
					if status.Reason != "Admitted" {
						t.Fatalf("group did not admit every declared member: %#v", evaluated.Status.Nodes)
					}
				}

				if _, err := r.Reconcile(t.Context(), req); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestRecurringGroupPendingUsesPinnedPolicyAndChargesEveryFailureDomainMember(t *testing.T) {
	t.Parallel()

	profile, nodes := recurringRackFixture()
	kube := membershipClientBuilder(t, profile, nodes[0], nodes[1], &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "scripts", Namespace: "signal"},
		Data:       map[string]string{"payload.txt": "test payload\n"},
	}).Build()
	admission := &AdmissionReconciler{Client: kube, Namespace: "signal", ScriptNamespace: "signal", Now: membershipNow}

	recurring := &RecurringReconciler{Client: kube, Reader: kube, Namespace: "signal", Admission: admission, Now: membershipNow}
	if _, err := recurring.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKey{Name: profile.Name}}); err != nil {
		t.Fatal(err)
	}

	var runs signalv1.CertificationRunList
	if err := kube.List(t.Context(), &runs); err != nil || len(runs.Items) != 1 {
		t.Fatalf("pending recurring run: %d, %v", len(runs.Items), err)
	}

	run := &runs.Items[0]

	idle := membershipNow().Add(-18 * time.Minute)
	if run.Spec.Admission.IdleSince == nil || !run.Spec.Admission.IdleSince.Time.Equal(idle) {
		t.Fatalf("all-member idle evidence = %v, want %v", run.Spec.Admission.IdleSince, idle)
	}

	var changed signalv1.CertificationProfile
	if err := kube.Get(t.Context(), client.ObjectKey{Name: profile.Name}, &changed); err != nil {
		t.Fatal(err)
	}

	changed.Spec.Recurring.ExpiresAt = metav1.NewTime(membershipNow().Add(-time.Hour))
	changed.Spec.Recurring.MaxConcurrentPerFailureDomain = 1

	changed.Spec.Recurring.NodeNames = []string{"unapproved"}
	if err := kube.Update(t.Context(), &changed); err != nil {
		t.Fatal(err)
	}

	copy := run.DeepCopy()
	copy.Spec.Admission.Recurring.NodeNames[0] = "unapproved"

	copy.Spec.Admission.IdleSince.Time = membershipNow()
	if run.Spec.Admission.Recurring.NodeNames[0] == "unapproved" ||
		!run.Spec.Admission.IdleSince.Time.Equal(idle) {
		t.Fatal("deepcopy aliases frozen recurring admission evidence")
	}

	r := membershipReconciler(t, kube)
	if _, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatal(err)
	}

	got := getTestRun(t, kube, run)
	if !got.Status.AdmissionReady || got.Status.Workflow.Attempt != 1 {
		t.Fatalf("later profile edits changed pending admission: %#v", got.Status)
	}

	assertRackReservations(t, kube, nodes, run.Name)
	assertRackWorkflowCount(t, kube, 1)
}

func TestRecurringGroupPolicyExpiryBeforeWorkloadRejectsWholeSet(t *testing.T) {
	t.Parallel()

	run, nodes := rackTestRun(t, "expired-recurring-group", FullNodeCertificationWorkflow)
	profile, _ := recurringRackFixture()
	run.Spec.Admission.Trigger = "recurring"
	run.Spec.Admission.Recurring = profile.Spec.Recurring.DeepCopy()
	run.Spec.Admission.Recurring.ExpiresAt = metav1.NewTime(membershipNow())
	run.Spec.Admission.IdleSince = ptrMetaTime(membershipNow().Add(-time.Hour))
	kube := membershipClientBuilder(t, rackRunObjects(run, nodes)...).Build()

	r := membershipReconciler(t, kube)
	if _, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatal(err)
	}

	got := getTestRun(t, kube, run)
	if got.Status.Phase != signalv1.RunPhaseInconclusive || !strings.Contains(got.Status.Reason, "policy expired") {
		t.Fatalf("expired recurring group status = %#v", got.Status)
	}

	assertRackReservations(t, kube, nodes, "")
	assertRackWorkflowCount(t, kube, 0)
}

func recurringRackFixture() (*signalv1.CertificationProfile, []*corev1.Node) {
	profile, nodes := rackProfileAndNodes()
	profile.Spec.Recurring = &signalv1.RecurringPolicy{
		Enabled:                       true,
		NodeNames:                     []string{nodes[0].Name, nodes[1].Name},
		MinIdleDuration:               metav1.Duration{Duration: 15 * time.Minute},
		ValidFor:                      metav1.Duration{Duration: time.Hour},
		ExpiresAt:                     metav1.NewTime(membershipNow().Add(2 * time.Hour)),
		FailureBackoff:                metav1.Duration{Duration: 30 * time.Minute},
		MaxConsecutiveFailures:        3,
		MaxConcurrent:                 1,
		FailureDomainLabel:            rackTopologyKey,
		MaxConcurrentPerFailureDomain: 2,
	}
	profile.Status.Nodes = []signalv1.RecurringNodeStatus{
		{NodeName: nodes[0].Name, NodeUID: nodes[0].UID, IdleSince: ptrMetaTime(membershipNow().Add(-20 * time.Minute))},
		{NodeName: nodes[1].Name, NodeUID: nodes[1].UID, IdleSince: ptrMetaTime(membershipNow().Add(-18 * time.Minute))},
	}

	return profile, nodes
}

func TestRecurringGroupRecreatedMemberCannotReuseThePreviousUIDIdleObservation(t *testing.T) {
	t.Parallel()

	profile, nodes := recurringRackFixture()
	profile.Status.Nodes[1].NodeUID = "previous-node-uid"
	profile.Status.Nodes[1].LastSuccessfulRun = "previous-node-success"
	profile.Status.Nodes[1].LastSuccessfulAt = ptrMetaTime(membershipNow().Add(-time.Hour))
	previous := testRun("previous-node-success")
	previous.Spec.Profile.Name = profile.Name
	previous.Spec.Participants = []signalv1.Participant{{NodeName: nodes[1].Name, NodeUID: "previous-node-uid"}}
	previous.Status.Phase = signalv1.RunPhaseSucceeded
	previous.Status.FinishedAt = ptrMetaTime(membershipNow().Add(-time.Hour))
	kube := membershipClientBuilder(t, profile, previous, nodes[0], nodes[1], &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "scripts", Namespace: "signal"},
		Data:       map[string]string{"payload.txt": "test payload\n"},
	}).Build()

	r := &RecurringReconciler{
		Client: kube, Reader: kube, Namespace: "signal", Now: membershipNow,
		Admission: &AdmissionReconciler{Client: kube, Namespace: "signal", ScriptNamespace: "signal", Now: membershipNow},
	}
	if _, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKey{Name: profile.Name}}); err != nil {
		t.Fatal(err)
	}

	var runs signalv1.CertificationRunList
	if err := kube.List(t.Context(), &runs); err != nil {
		t.Fatal(err)
	}

	if len(runs.Items) != 1 || runs.Items[0].Name != previous.Name {
		t.Fatal("a recreated member reused the prior Node UID's continuous idle observation")
	}

	var evaluated signalv1.CertificationProfile
	if err := kube.Get(t.Context(), client.ObjectKey{Name: profile.Name}, &evaluated); err != nil {
		t.Fatal(err)
	}

	status := recurringNodeStatus(evaluated.Status.Nodes, nodes[1].Name)
	if status.NodeUID != nodes[1].UID || status.IdleSince == nil || !status.IdleSince.Time.Equal(membershipNow()) {
		t.Fatalf("recreated member did not restart its continuous idle observation: %#v", status)
	}

	if status.LastSuccessfulRun != "" || status.LastSuccessfulAt != nil {
		t.Fatal("a recreated member inherited certification evidence from the previous Node UID")
	}

	var recreated corev1.Node
	if err := kube.Get(t.Context(), client.ObjectKey{Name: nodes[1].Name}, &recreated); err != nil {
		t.Fatal(err)
	}

	if recreated.Labels[StateLabel] == "passed" || recreated.Labels[EligibilityLabel] == "production" {
		t.Fatal("recurrence projected a previous UID's certification onto the recreated Node")
	}

	assertRackReservations(t, kube, nodes, "")
	assertRackWorkflowCount(t, kube, 0)
}
