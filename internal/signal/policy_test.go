// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

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
