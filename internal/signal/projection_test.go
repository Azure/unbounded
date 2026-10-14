// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func TestNodeProjectionTransitionsAndRetainsTerminalSummary(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	node := readyNode("gpu-0", "A100")
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	run := testRun("run-pass")

	if err := projectPending(
		context.Background(),
		kube,
		run.Spec.Participants,
		run.Spec.Profile,
		run.Spec.Purpose,
	); err != nil {
		t.Fatal(err)
	}

	assertNodeProjection(t, kube, node.Name, "pending", "blocked")

	if err := projectTesting(context.Background(), kube, run); err != nil {
		t.Fatal(err)
	}

	assertNodeProjection(t, kube, node.Name, "testing", "blocked")

	run.Status.Phase = signalv1.RunPhaseSucceeded
	run.Status.ExecutionState = "Completed"
	run.Status.Verdict = "Pass"
	run.Status.Reason = "Healthy"
	run.Status.Evidence.Digest = "sha256:evidence"
	run.Status.FinishedAt = ptrTime(metav1.NewTime(time.Date(2026, 10, 11, 5, 0, 0, 0, time.UTC)))

	if err := projectTerminal(context.Background(), kube, run); err != nil {
		t.Fatal(err)
	}

	var got corev1.Node
	if err := kube.Get(context.Background(), client.ObjectKey{Name: node.Name}, &got); err != nil {
		t.Fatal(err)
	}

	if got.Labels[StateLabel] != "passed" || got.Labels[EligibilityLabel] != "production" {
		t.Fatalf("terminal labels = %v", got.Labels)
	}

	if got.Annotations[LastRunAnnotation] != run.Name ||
		got.Annotations[LastSuccessfulRunAnnotation] != run.Name ||
		got.Annotations[EvidenceDigestAnnotation] != run.Status.Evidence.Digest ||
		got.Annotations[ScriptDigestAnnotation] != run.Spec.Script.Digest {
		t.Fatalf("terminal annotations = %v", got.Annotations)
	}
}

func TestDiagnosticProjectionCannotOverrideFullCertificationFailure(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	node := readyNode("gpu-0", "A100")
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()

	full := testRun("full-fail")
	full.Spec.Purpose = signalv1.PurposeFullCertification
	full.Status.Phase = signalv1.RunPhaseFailed
	full.Status.FinishedAt = ptrTime(metav1.NewTime(time.Date(2026, 10, 11, 5, 0, 0, 0, time.UTC)))
	full.Status.Evidence.Digest = "sha256:full"

	if err := projectTerminal(context.Background(), kube, full); err != nil {
		t.Fatal(err)
	}

	diagnostic := testRun("burn-pass")
	diagnostic.Spec.Purpose = signalv1.PurposeDiagnostic
	diagnostic.Status.Phase = signalv1.RunPhaseSucceeded
	diagnostic.Status.FinishedAt = ptrTime(metav1.NewTime(time.Date(2026, 10, 11, 6, 0, 0, 0, time.UTC)))
	diagnostic.Status.Evidence.Digest = "sha256:burn"

	if err := projectPending(
		context.Background(),
		kube,
		diagnostic.Spec.Participants,
		diagnostic.Spec.Profile,
		diagnostic.Spec.Purpose,
	); err != nil {
		t.Fatal(err)
	}

	if err := projectTesting(context.Background(), kube, diagnostic); err != nil {
		t.Fatal(err)
	}

	if err := projectTerminal(context.Background(), kube, diagnostic); err != nil {
		t.Fatal(err)
	}

	var got corev1.Node
	if err := kube.Get(context.Background(), client.ObjectKey{Name: node.Name}, &got); err != nil {
		t.Fatal(err)
	}

	if got.Labels[StateLabel] != "failed" ||
		got.Labels[EligibilityLabel] != "blocked" ||
		got.Annotations[LastRunAnnotation] != full.Name ||
		got.Annotations[EvidenceDigestAnnotation] != "sha256:full" ||
		got.Annotations[ProjectionPurposeAnnotation] != string(signalv1.PurposeFullCertification) {
		t.Fatalf("diagnostic overwrote full certification projection: labels=%v annotations=%v", got.Labels, got.Annotations)
	}
}

func TestNodeProjectionFailureAndInconclusiveBlockEligibility(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		phase signalv1.RunPhase
		state string
	}{
		{name: "failed", phase: signalv1.RunPhaseFailed, state: "failed"},
		{name: "inconclusive", phase: signalv1.RunPhaseInconclusive, state: "inconclusive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := testScheme(t)
			node := readyNode("gpu-0", "A100")
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
			run := testRun("run-" + tc.name)
			run.Status.Phase = tc.phase
			run.Status.FinishedAt = ptrTime(metav1.Now())

			if err := projectTerminal(context.Background(), kube, run); err != nil {
				t.Fatal(err)
			}

			assertNodeProjection(t, kube, node.Name, tc.state, "blocked")
		})
	}
}

func TestNodeProjectionDoesNotRegressAfterRestartOrOlderRetry(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	node := readyNode("gpu-0", "A100")
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	newer := testRun("newer")
	newer.Status.Phase = signalv1.RunPhaseSucceeded
	newer.Status.FinishedAt = ptrTime(metav1.NewTime(time.Date(2026, 10, 11, 6, 0, 0, 0, time.UTC)))
	newer.Status.Evidence.Digest = "sha256:newer"

	if err := projectTerminal(context.Background(), kube, newer); err != nil {
		t.Fatal(err)
	}

	older := testRun("older")
	older.Status.Phase = signalv1.RunPhaseFailed
	older.Status.FinishedAt = ptrTime(metav1.NewTime(time.Date(2026, 10, 11, 5, 0, 0, 0, time.UTC)))
	older.Status.Evidence.Digest = "sha256:older"

	if err := projectTerminal(context.Background(), kube, older); err != nil {
		t.Fatal(err)
	}

	var got corev1.Node
	if err := kube.Get(context.Background(), client.ObjectKey{Name: node.Name}, &got); err != nil {
		t.Fatal(err)
	}

	if got.Labels[StateLabel] != "passed" ||
		got.Annotations[LastRunAnnotation] != newer.Name ||
		got.Annotations[EvidenceDigestAnnotation] != "sha256:newer" {
		t.Fatalf("projection regressed: labels=%v annotations=%v", got.Labels, got.Annotations)
	}
}

func TestNodeProjectionDoesNotCertifyReplacementNode(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	replacement := readyNode("gpu-0", "A100")
	replacement.UID = "replacement-uid"
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(replacement).Build()
	run := testRun("run-pass")
	run.Status.Phase = signalv1.RunPhaseSucceeded
	run.Status.Verdict = "Pass"
	run.Status.FinishedAt = ptrTime(metav1.Now())

	if err := projectTerminal(context.Background(), kube, run); err != nil {
		t.Fatal(err)
	}

	var got corev1.Node
	if err := kube.Get(context.Background(), client.ObjectKey{Name: replacement.Name}, &got); err != nil {
		t.Fatal(err)
	}

	if got.Labels[StateLabel] != "" || got.Labels[EligibilityLabel] != "" {
		t.Fatalf("replacement Node was certified: labels=%v", got.Labels)
	}
}

func TestProjectionValuesAreBoundedAndValid(t *testing.T) {
	t.Parallel()

	longValue := strings.Repeat("Invalid/Profile:", 200)

	label := projectionLabelValue(longValue)
	if len(label) > 63 {
		t.Fatalf("label value is too long: %d", len(label))
	}

	if bounded := boundedAnnotationValue(longValue); len(bounded) > len("sha256:")+64 {
		t.Fatalf("annotation value is unbounded: %d", len(bounded))
	}

	if got := projectionLabelValue("gpu-smoke-example"); got != "gpu-smoke-example" {
		t.Fatalf("valid profile label changed to %q", got)
	}
}

func TestRecurringProjectionPublishesNextDueAndSuspension(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	node := readyNode("gpu-0", "A100")
	profile := testProfile("gpu-smoke-example", "example-gpu")
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	nextDue := metav1.NewTime(time.Date(2026, 10, 11, 7, 0, 0, 0, time.UTC))
	status := signalv1.RecurringNodeStatus{
		NodeName:          node.Name,
		NextDueAt:         &nextDue,
		LastSuccessfulRun: "run-pass",
		LastSuccessfulAt:  ptrTime(metav1.NewTime(time.Date(2026, 10, 11, 6, 0, 0, 0, time.UTC))),
		Suspended:         true,
		Reason:            "CircuitOpen",
	}

	if err := projectRecurring(context.Background(), kube, profile, status); err != nil {
		t.Fatal(err)
	}

	var got corev1.Node
	if err := kube.Get(context.Background(), client.ObjectKey{Name: node.Name}, &got); err != nil {
		t.Fatal(err)
	}

	if got.Annotations[NextDueAtAnnotation] != "2026-10-11T07:00:00Z" ||
		got.Annotations[RecurringReasonAnnotation] != "CircuitOpen" ||
		got.Annotations[RecurringSuspendedAnnotation] != "true" {
		t.Fatalf("recurring annotations = %v", got.Annotations)
	}
}

func assertNodeProjection(
	t *testing.T,
	kube client.Client,
	nodeName string,
	state string,
	eligibility string,
) {
	t.Helper()

	var node corev1.Node
	if err := kube.Get(context.Background(), client.ObjectKey{Name: nodeName}, &node); err != nil {
		t.Fatal(err)
	}

	if node.Labels[StateLabel] != state || node.Labels[EligibilityLabel] != eligibility {
		t.Fatalf("projection = state %q eligibility %q", node.Labels[StateLabel], node.Labels[EligibilityLabel])
	}
}
