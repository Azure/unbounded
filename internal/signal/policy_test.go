// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	machinav1 "github.com/Azure/unbounded/api/machina/v1alpha3"
	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func TestRecurringReconcileAdmitsOnlyContinuouslyIdleOptedInNodes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		reason    string
		wantRun   bool
		busy      bool
		readError bool
		notIdle   bool
		expired   bool
		disabled  bool
	}{
		{name: "idle admission", reason: "Admitted", wantRun: true},
		{name: "active workload", reason: "WorkloadActive", busy: true},
		{name: "unreadable workload state", reason: "EvaluationError:", readError: true},
		{name: "idle floor", reason: "NotDue", notIdle: true},
		{name: "expired opt in", reason: "PolicyExpired", expired: true},
		{name: "disabled opt in", disabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			now := time.Date(2026, 10, 10, 20, 0, 0, 0, time.UTC)
			node := readyNode("gpu-0", "example-gpu")
			profile := testProfile("gpu-smoke", "example-gpu")
			profile.Spec.Recurring = &signalv1.RecurringPolicy{
				Enabled:                !tc.disabled,
				NodeNames:              []string{node.Name},
				MinIdleDuration:        metav1.Duration{Duration: 15 * time.Minute},
				ValidFor:               metav1.Duration{Duration: time.Hour},
				ExpiresAt:              metav1.NewTime(now.Add(time.Hour)),
				FailureBackoff:         metav1.Duration{Duration: 30 * time.Minute},
				MaxConsecutiveFailures: 3,
				MaxConcurrent:          1,
			}

			idleSince := metav1.NewTime(now.Add(-15 * time.Minute))
			if tc.notIdle {
				idleSince = metav1.NewTime(now.Add(-15*time.Minute + time.Second))
			}

			if tc.expired {
				profile.Spec.Recurring.ExpiresAt = metav1.NewTime(now)
			}

			profile.Status.Nodes = []signalv1.RecurringNodeStatus{{NodeName: node.Name, IdleSince: &idleSince}}
			source := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "scripts", Namespace: "signal"},
				Data:       map[string]string{"payload.txt": "test payload\n"},
			}
			controller := true
			daemon := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "daemon", Namespace: "signal",
					OwnerReferences: []metav1.OwnerReference{{Kind: "DaemonSet", Controller: &controller}},
				},
				Spec:   corev1.PodSpec{NodeName: node.Name},
				Status: corev1.PodStatus{Phase: corev1.PodRunning},
			}

			workload := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: "signal"},
				Spec:       corev1.PodSpec{NodeName: node.Name},
				Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
			}
			if tc.busy {
				workload.Status.Phase = corev1.PodRunning
			}

			scheme := testScheme(t)
			if err := machinav1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}

			kube := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&signalv1.CertificationProfile{}, &signalv1.CertificationRun{}).
				WithIndex(&corev1.Pod{}, "spec.nodeName", func(obj client.Object) []string {
					return []string{obj.(*corev1.Pod).Spec.NodeName}
				}).
				WithObjects(node, profile, source, daemon, workload).
				Build()
			reader := interceptor.NewClient(kube, interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, pods := list.(*corev1.PodList); pods && tc.readError {
						return errors.New("injected workload read failure")
					}

					return c.List(ctx, list, opts...)
				},
			})
			admission := &AdmissionReconciler{
				Client: kube, Reader: reader, Namespace: "signal", ScriptNamespace: "signal",
				Now: func() time.Time { return now },
			}
			r := &RecurringReconciler{
				Client: kube, Reader: reader, Namespace: "signal", Admission: admission,
				Now: func() time.Time { return now },
			}
			req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(profile)}

			if _, err := r.Reconcile(t.Context(), req); err != nil {
				t.Fatal(err)
			}

			var current signalv1.CertificationProfile
			if err := kube.Get(t.Context(), client.ObjectKeyFromObject(profile), &current); err != nil {
				t.Fatal(err)
			}

			if len(current.Status.Nodes) != 1 || (tc.reason == "" && current.Status.Nodes[0].Reason != "") ||
				(tc.reason != "" && !strings.HasPrefix(current.Status.Nodes[0].Reason, tc.reason)) {
				t.Fatalf("recurring status = %#v, want reason %q", current.Status, tc.reason)
			}

			var runs signalv1.CertificationRunList
			if err := kube.List(t.Context(), &runs); err != nil {
				t.Fatal(err)
			}

			if !tc.wantRun {
				if len(runs.Items) != 0 {
					t.Fatalf("unsafe recurring admission created %d Runs", len(runs.Items))
				}

				return
			}

			if len(runs.Items) != 1 {
				t.Fatalf("recurring admission created %d Runs, want one", len(runs.Items))
			}

			run := &runs.Items[0]
			if run.Annotations[RunTriggerAnnotation] != "recurring" ||
				run.Spec.Profile.Generation != profile.Generation || run.Spec.Purpose != profile.Spec.Purpose ||
				len(run.Spec.StagePlan) != len(profile.Spec.StagePlan) || run.Spec.Script.Digest != digestString(source.Data["payload.txt"]) {
				t.Fatalf("recurring admission did not pin its inputs: %#v", run)
			}

			assertNodeProjection(t, kube, node.Name, "pending", "blocked")

			if _, err := r.Reconcile(t.Context(), req); err != nil {
				t.Fatal(err)
			}

			if err := kube.List(t.Context(), &runs); err != nil || len(runs.Items) != 1 {
				t.Fatalf("recurring admission was duplicated: %d Runs, %v", len(runs.Items), err)
			}
		})
	}
}

func TestRecurringDueUsesContinuousIdleAndFreshness(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 10, 10, 20, 0, 0, 0, time.UTC)
	idleSince := metav1.NewTime(start)
	policy := &signalv1.RecurringPolicy{
		MinIdleDuration: metav1.Duration{Duration: 15 * time.Minute},
		ValidFor:        metav1.Duration{Duration: time.Hour},
	}

	if got := dueAt(policy, &idleSince, nil); !got.Equal(start.Add(15 * time.Minute)) {
		t.Fatalf("initial due = %s", got)
	}

	success := testRun("success")
	success.Status.Phase = signalv1.RunPhaseSucceeded
	success.Status.FinishedAt = ptrTime(metav1.NewTime(start.Add(10 * time.Minute)))

	if got := dueAt(policy, &idleSince, success); !got.Equal(start.Add(70 * time.Minute)) {
		t.Fatalf("freshness due = %s", got)
	}
}

func TestRecurringIdleStateSurvivesRestart(t *testing.T) {
	t.Parallel()

	idleSince := metav1.NewTime(time.Date(2026, 10, 10, 20, 0, 0, 0, time.UTC))
	statuses := []signalv1.RecurringNodeStatus{{NodeName: "gpu-0", IdleSince: &idleSince, Reason: "NotDue"}}

	got := recurringNodeStatus(statuses, "gpu-0")
	if got.IdleSince == nil || !got.IdleSince.Equal(&idleSince) {
		t.Fatalf("idleSince was not recovered: %#v", got)
	}
}

func TestRecurringExpiryFailsClosed(t *testing.T) {
	t.Parallel()

	expiry := metav1.NewTime(time.Date(2026, 10, 12, 14, 0, 0, 0, time.UTC))
	policy := &signalv1.RecurringPolicy{ExpiresAt: expiry}

	if policyExpired(policy, expiry.Add(-time.Nanosecond)) {
		t.Fatal("policy expired early")
	}

	if !policyExpired(policy, expiry.Time) {
		t.Fatal("policy remained active at its expiry")
	}
}

func TestRecurringConcurrencyAndDuplicatePrevention(t *testing.T) {
	t.Parallel()

	active := *testRun("active")
	active.Status.Phase = signalv1.RunPhaseRunning
	active.Spec.Profile.Name = "short"
	profile := testProfile("short", "A100")

	count, _ := activeBudgets([]signalv1.CertificationRun{active}, profile)
	if count != 1 || !activeRunForNode([]signalv1.CertificationRun{active}, "gpu-0") {
		t.Fatalf("active budget=%d duplicate=%t", count, activeRunForNode([]signalv1.CertificationRun{active}, "gpu-0"))
	}
}

func TestRecurringShortLongMutualExclusion(t *testing.T) {
	t.Parallel()

	longRun := *testRun("long")
	longRun.Spec.Profile.Name = "gpu-long"
	longRun.Status.Phase = signalv1.RunPhaseRunning

	if !excludedRunActive(
		[]signalv1.CertificationRun{longRun},
		"gpu-0",
		[]string{"gpu-long"},
	) {
		t.Fatal("active long run did not exclude the short profile")
	}
}

func TestRecurringFailureBackoffAndCircuitBreaker(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 10, 20, 0, 0, 0, time.UTC)
	failed := *testRun("failed")
	failed.Status.Phase = signalv1.RunPhaseFailed
	failed.Status.FinishedAt = ptrTime(metav1.NewTime(now.Add(-5 * time.Minute)))
	inconclusive := *testRun("inconclusive")
	inconclusive.Status.Phase = signalv1.RunPhaseInconclusive
	inconclusive.Status.FinishedAt = ptrTime(metav1.NewTime(now.Add(-10 * time.Minute)))
	runs := []signalv1.CertificationRun{failed, inconclusive}

	_, count, latest := runHistory(runs)
	policy := &signalv1.RecurringPolicy{
		FailureBackoff:         metav1.Duration{Duration: 30 * time.Minute},
		MaxConsecutiveFailures: 3,
	}

	suspended, reason := failureGate(policy, count, latest, now)
	if suspended || reason != "FailureBackoff" {
		t.Fatalf("backoff gate = (%t, %q)", suspended, reason)
	}

	policy.MaxConsecutiveFailures = 2

	suspended, reason = failureGate(policy, count, latest, now)
	if !suspended || reason != "CircuitOpen" {
		t.Fatalf("circuit gate = (%t, %q)", suspended, reason)
	}
}

func TestIdleDetectionIgnoresOnlyDaemonSetsAndTerminalPods(t *testing.T) {
	t.Parallel()

	controller := true
	daemon := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
			APIVersion: appsv1.SchemeGroupVersion.String(),
			Kind:       "DaemonSet",
			Controller: &controller,
		}}},
	}
	deployment := daemon.DeepCopy()
	deployment.OwnerReferences[0].Kind = "ReplicaSet"

	if !controlledByDaemonSet(daemon) {
		t.Fatal("DaemonSet pod was treated as workload activity")
	}

	if controlledByDaemonSet(deployment) {
		t.Fatal("non-DaemonSet pod was treated as disruptable")
	}
}
