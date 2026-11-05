// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func TestQualificationEvidenceRejectsEarlySuccessAndUnsupportedContinuity(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	node := qualificationNode("node", "node-uid", "boot-a")
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	reconciler := &RunReconciler{
		Client:          kube,
		APIReader:       kube,
		ControllerImage: pinnedImage("controller", "a"),
	}

	start := metav1.NewTime(time.Now().UTC())
	finish := metav1.NewTime(start.Add(time.Minute))
	run := qualificationRun("run", node, signalv1.QualificationVerdictPass, finish.Time)
	run.Spec.Checks[0].MinDuration = &metav1.Duration{Duration: 2 * time.Minute}
	refreshPlanDigest(run)
	run.Annotations = map[string]string{
		RunStartIdentityAnnotation: `[{"nodeUID":"node-uid","bootID":"boot-a"}]`,
	}

	statuses := []signalv1.CheckStatus{{
		ID:         signalv1.CheckID("R01"),
		Order:      0,
		Phase:      signalv1.CheckPhaseSucceeded,
		StartedAt:  &start,
		FinishedAt: &finish,
	}}

	identity, verdict, reason, err := reconciler.qualificationEvidenceIdentity(
		context.Background(),
		run,
		statuses,
		"Pass",
		"",
	)
	if err != nil {
		t.Fatalf("qualificationEvidenceIdentity() error = %v", err)
	}

	if identity == nil || verdict != "Inconclusive" ||
		!strings.Contains(reason, "MinimumDurationNotMet") {
		t.Fatalf("early success = identity %#v, verdict %q, reason %q", identity, verdict, reason)
	}

	run.Spec.Checks[0].MinDuration = nil
	run.Spec.Checks[0].Continuity = &signalv1.ContinuityPolicy{
		MaxSampleGap: &metav1.Duration{Duration: 10 * time.Second},
	}
	refreshPlanDigest(run)

	_, verdict, reason, err = reconciler.qualificationEvidenceIdentity(
		context.Background(),
		run,
		statuses,
		"Pass",
		"",
	)
	if err != nil {
		t.Fatalf("qualificationEvidenceIdentity() continuity error = %v", err)
	}

	if verdict != "Inconclusive" || !strings.Contains(reason, "sample-gap evidence") {
		t.Fatalf("unsupported continuity verdict = %q, reason %q", verdict, reason)
	}
}

func TestQualificationEvidenceRejectsSameBootViolation(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	node := qualificationNode("node", "node-uid", "boot-b")
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	reconciler := &RunReconciler{
		Client:          kube,
		APIReader:       kube,
		ControllerImage: pinnedImage("controller", "a"),
	}

	start := metav1.NewTime(time.Now().UTC())
	finish := metav1.NewTime(start.Add(time.Minute))
	run := qualificationRun("run", node, signalv1.QualificationVerdictPass, finish.Time)
	run.Spec.Checks[0].Continuity = &signalv1.ContinuityPolicy{RequireSameBootID: true}
	refreshPlanDigest(run)
	run.Annotations = map[string]string{
		RunStartIdentityAnnotation: `[{"nodeUID":"node-uid","bootID":"boot-a"}]`,
	}

	_, verdict, reason, err := reconciler.qualificationEvidenceIdentity(
		context.Background(),
		run,
		[]signalv1.CheckStatus{{
			ID:         signalv1.CheckID("R01"),
			Phase:      signalv1.CheckPhaseSucceeded,
			StartedAt:  &start,
			FinishedAt: &finish,
		}},
		"Pass",
		"",
	)
	if err != nil {
		t.Fatalf("qualificationEvidenceIdentity() error = %v", err)
	}

	if verdict != "Inconclusive" || !strings.Contains(reason, "same-boot evidence") {
		t.Fatalf("same-boot violation verdict = %q, reason %q", verdict, reason)
	}
}

func TestQualificationEvidenceDoesNotInferMultiParticipantVerdicts(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	requester := qualificationNode("requester", "requester-uid", "boot-a")
	helper := qualificationNode("helper", "helper-uid", "boot-a")
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(requester, helper).Build()
	reconciler := &RunReconciler{
		Client:          kube,
		APIReader:       kube,
		ControllerImage: pinnedImage("controller", "a"),
	}

	finishedAt := time.Now().UTC()
	run := qualificationRun("run", requester, signalv1.QualificationVerdictPass, finishedAt)
	run.Spec.Participants = append(run.Spec.Participants, signalv1.Participant{
		NodeName: helper.Name,
		NodeUID:  helper.UID,
	})
	run.Annotations = map[string]string{
		RunStartIdentityAnnotation: `[` +
			`{"nodeUID":"requester-uid","bootID":"boot-a"},` +
			`{"nodeUID":"helper-uid","bootID":"boot-a"}]`,
	}
	refreshPlanDigest(run)

	start := metav1.NewTime(finishedAt.Add(-time.Minute))
	finish := metav1.NewTime(finishedAt)

	identity, verdict, _, err := reconciler.qualificationEvidenceIdentity(
		context.Background(),
		run,
		[]signalv1.CheckStatus{{
			ID:         signalv1.CheckID("R01"),
			Phase:      signalv1.CheckPhaseSucceeded,
			StartedAt:  &start,
			FinishedAt: &finish,
		}},
		"Pass",
		"",
	)
	if err != nil {
		t.Fatalf("qualificationEvidenceIdentity() error = %v", err)
	}

	if identity != nil || verdict != "Pass" {
		t.Fatalf("multi-participant aggregate verdict produced qualification identity %#v, verdict %q", identity, verdict)
	}
}

func TestEvaluateLedgerQualificationAndMigrationRules(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	node := qualificationNode("node", "node-uid", "boot-a")
	policy := qualificationPolicy("policy", "policy-uid", signalv1.NonPassBehaviorIgnore)
	run := qualificationRun("pass", node, signalv1.QualificationVerdictPass, now)
	policy.Spec.Requirements[0].AcceptedPlans.Digests = []string{run.Spec.PlanDigest}

	scheme := qualificationScheme(t)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, policy, run).Build()
	reconciler := &QualificationReconciler{
		Client:    kube,
		APIReader: kube,
		Evidence:  missingEvidenceStore{},
		Namespace: "signal-system",
		Now:       func() time.Time { return now },
	}
	ledger := qualificationLedger(policy, node)

	status, err := reconciler.evaluateLedger(context.Background(), policy, node, ledger)
	if err != nil {
		t.Fatalf("evaluateLedger() error = %v", err)
	}

	if status.Phase != signalv1.NodeCertificationPhaseQualified ||
		len(status.ContributingRuns) != 1 ||
		status.PolicyDigest == "" {
		t.Fatalf("qualified status = %#v", status)
	}

	oldRun := run.DeepCopy()
	oldRun.Name = "old"
	oldRun.UID = "old-run-uid"
	oldRun.Spec.PlanDigest = ""
	oldRun.Spec.PlanProvenance = nil
	oldRun.Status.Evidence.Identity = nil
	kube = fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, policy, oldRun).Build()
	reconciler.Client = kube
	reconciler.APIReader = kube

	status, err = reconciler.evaluateLedger(context.Background(), policy, node, qualificationLedger(policy, node))
	if err != nil {
		t.Fatalf("evaluateLedger(old Run) error = %v", err)
	}

	if status.Phase == signalv1.NodeCertificationPhaseQualified {
		t.Fatalf("old Run without plan identity received qualification credit: %#v", status)
	}
}

func TestEvaluateLedgerRevocationExpiryAndFingerprintInvalidation(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	node := qualificationNode("node", "node-uid", "boot-a")
	policy := qualificationPolicy("policy", "policy-uid", signalv1.NonPassBehaviorRevoke)
	pass := qualificationRun("pass", node, signalv1.QualificationVerdictPass, now.Add(-time.Hour))
	fail := qualificationRun("fail", node, signalv1.QualificationVerdictFail, now)
	policy.Spec.Requirements[0].AcceptedPlans.Digests = []string{pass.Spec.PlanDigest}
	fail.Status.Phase = signalv1.RunPhaseFailed

	scheme := qualificationScheme(t)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, policy, pass, fail).Build()
	reconciler := &QualificationReconciler{
		Client:    kube,
		APIReader: kube,
		Evidence:  missingEvidenceStore{},
		Namespace: "signal-system",
		Now:       func() time.Time { return now },
	}

	status, err := reconciler.evaluateLedger(context.Background(), policy, node, qualificationLedger(policy, node))
	if err != nil {
		t.Fatalf("evaluateLedger(revoke) error = %v", err)
	}

	if status.Phase != signalv1.NodeCertificationPhaseRevoked {
		t.Fatalf("phase = %s, want Revoked", status.Phase)
	}

	policy.Spec.NonPassBehavior = signalv1.NonPassBehaviorIgnore
	policy.Spec.Validity = metav1.Duration{Duration: 30 * time.Minute}
	kube = fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, policy, pass).Build()
	reconciler.Client = kube
	reconciler.APIReader = kube

	status, err = reconciler.evaluateLedger(context.Background(), policy, node, qualificationLedger(policy, node))
	if err != nil {
		t.Fatalf("evaluateLedger(expiry) error = %v", err)
	}

	if status.Phase != signalv1.NodeCertificationPhaseExpired {
		t.Fatalf("phase = %s, want Expired", status.Phase)
	}

	policy.Spec.Validity = metav1.Duration{Duration: 2 * time.Hour}
	policy.Spec.InvalidationFingerprintFields = []signalv1.FingerprintField{
		signalv1.FingerprintFieldNodeUID,
		signalv1.FingerprintFieldBootID,
	}
	node.Status.NodeInfo.BootID = "boot-b"
	kube = fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, policy, pass).Build()
	reconciler.Client = kube
	reconciler.APIReader = kube

	status, err = reconciler.evaluateLedger(context.Background(), policy, node, qualificationLedger(policy, node))
	if err != nil {
		t.Fatalf("evaluateLedger(fingerprint) error = %v", err)
	}

	if status.Phase == signalv1.NodeCertificationPhaseQualified {
		t.Fatalf("changed fingerprint retained qualification: %#v", status)
	}
}

func TestEvaluateLedgerRequalifiesAfterFingerprintChange(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	node := qualificationNode("node", "node-uid", "boot-b")
	policy := qualificationPolicy("policy", "policy-uid", signalv1.NonPassBehaviorIgnore)
	policy.Spec.InvalidationFingerprintFields = []signalv1.FingerprintField{
		signalv1.FingerprintFieldNodeUID,
		signalv1.FingerprintFieldBootID,
	}
	oldPass := qualificationRun("old-pass", node, signalv1.QualificationVerdictPass, now.Add(-time.Hour))
	oldPass.Status.Evidence.Identity.Participants[0].Start.BootID = "boot-a"
	oldPass.Status.Evidence.Identity.Participants[0].End.BootID = "boot-a"
	newPass := qualificationRun("new-pass", node, signalv1.QualificationVerdictPass, now)
	policy.Spec.Requirements[0].AcceptedPlans.Digests = []string{newPass.Spec.PlanDigest}

	scheme := qualificationScheme(t)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, policy, oldPass, newPass).Build()
	reconciler := &QualificationReconciler{
		Client:    kube,
		APIReader: kube,
		Evidence:  missingEvidenceStore{},
		Namespace: "signal-system",
		Now:       func() time.Time { return now },
	}

	status, err := reconciler.evaluateLedger(context.Background(), policy, node, qualificationLedger(policy, node))
	if err != nil {
		t.Fatalf("evaluateLedger() error = %v", err)
	}
	if status.Phase != signalv1.NodeCertificationPhaseQualified ||
		len(status.ContributingRuns) != 1 ||
		status.ContributingRuns[0].UID != newPass.UID {
		t.Fatalf("requalification status = %#v", status)
	}

	newPass.Status.Evidence.Identity.Participants[0].Start.BootID = "boot-a"
	kube = fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, policy, newPass).Build()
	reconciler.Client = kube
	reconciler.APIReader = kube

	status, err = reconciler.evaluateLedger(context.Background(), policy, node, qualificationLedger(policy, node))
	if err != nil {
		t.Fatalf("evaluateLedger(changed within Run) error = %v", err)
	}
	if status.Phase == signalv1.NodeCertificationPhaseQualified {
		t.Fatalf("same-Run fingerprint change received credit: %#v", status)
	}
}

func TestQualificationEvidenceRetriesTransientNodeRead(t *testing.T) {
	t.Parallel()

	node := qualificationNode("node", "node-uid", "boot-a")
	run := qualificationRun("run", node, signalv1.QualificationVerdictPass, time.Now().UTC())
	run.Annotations = map[string]string{
		RunStartIdentityAnnotation: `[{"nodeUID":"node-uid","bootID":"boot-a"}]`,
	}
	start := metav1.NewTime(time.Now().UTC().Add(-time.Minute))
	finish := metav1.NewTime(time.Now().UTC())
	reconciler := &RunReconciler{
		APIReader: failingNodeReader{
			err: apierrors.NewServiceUnavailable("temporary API failure"),
		},
		ControllerImage: pinnedImage("controller", "a"),
	}

	identity, verdict, reason, err := reconciler.qualificationEvidenceIdentity(
		context.Background(),
		run,
		[]signalv1.CheckStatus{{
			ID:         signalv1.CheckID("R01"),
			Phase:      signalv1.CheckPhaseSucceeded,
			StartedAt:  &start,
			FinishedAt: &finish,
		}},
		"Pass",
		"",
	)
	if err == nil || !apierrors.IsServiceUnavailable(err) {
		t.Fatalf("qualificationEvidenceIdentity() error = %v, want service unavailable", err)
	}
	if identity != nil || verdict != "Pass" || reason != "" {
		t.Fatalf("transient failure changed result: identity %#v, verdict %q, reason %q", identity, verdict, reason)
	}
}

func TestQualificationReconcileIsolatesCorruptEvidenceAndListsRunsOnce(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	policy := qualificationPolicy("policy", "policy-uid", signalv1.NonPassBehaviorIgnore)
	corruptNode := qualificationNode("corrupt", "corrupt-uid", "boot-a")
	cleanupNode := qualificationNode("cleanup", "cleanup-uid", "boot-a")
	cleanupNode.Labels = map[string]string{
		signalv1.NodeQualificationLabel: signalv1.NodeQualificationValue,
	}
	corruptLedger := qualificationLedger(policy, corruptNode)
	corruptLedger.Status.ContributingRuns = []signalv1.ContributingRunReference{{
		Name:           "corrupt-run",
		UID:            "corrupt-run-uid",
		PlanDigest:     "sha256:" + strings.Repeat("a", 64),
		EvidenceURI:    "kubernetes://signal-system/corrupt-run",
		EvidenceDigest: "sha256:" + strings.Repeat("b", 64),
	}}
	cleanupLedger := qualificationLedger(policy, cleanupNode)
	cleanupLedger.Status.Phase = signalv1.NodeCertificationPhaseRevoked

	scheme := qualificationScheme(t)
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&signalv1.NodeCertification{}).
		WithObjects(policy, corruptNode, cleanupNode, corruptLedger, cleanupLedger).
		Build()
	kube := &countingClient{Client: base}
	reconciler := &QualificationReconciler{
		Client:        kube,
		APIReader:     kube,
		Evidence:      corruptEvidenceStore{},
		Namespace:     "signal-system",
		GateProtected: true,
		Now:           func() time.Time { return now },
	}

	_, err := reconciler.Reconcile(
		context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Name: policy.Name}},
	)
	if err == nil {
		t.Fatal("Reconcile() error = nil, want corrupt evidence error")
	}
	if kube.runLists != 1 {
		t.Fatalf("CertificationRun lists = %d, want 1", kube.runLists)
	}

	var currentCleanup corev1.Node
	if err := kube.Get(
		context.Background(),
		types.NamespacedName{Name: cleanupNode.Name},
		&currentCleanup,
	); err != nil {
		t.Fatal(err)
	}
	if _, exists := currentCleanup.Labels[signalv1.NodeQualificationLabel]; exists {
		t.Fatalf("corrupt evidence prevented unrelated label cleanup: %#v", currentCleanup.Labels)
	}

	var currentCorrupt signalv1.NodeCertification
	if err := kube.Get(
		context.Background(),
		types.NamespacedName{Namespace: corruptLedger.Namespace, Name: corruptLedger.Name},
		&currentCorrupt,
	); err != nil {
		t.Fatal(err)
	}
	condition := apimeta.FindStatusCondition(currentCorrupt.Status.Conditions, "EvidenceHealthy")
	if condition == nil || condition.Status != metav1.ConditionFalse {
		t.Fatalf("corrupt evidence condition = %#v", condition)
	}
}

func TestEvaluateLedgerAccumulatesSeparatedRunsAndResetsAfterNonPass(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	node := qualificationNode("node", "node-uid", "boot-a")
	policy := qualificationPolicy("policy", "policy-uid", signalv1.NonPassBehaviorReset)
	policy.Spec.Requirements[0].RequiredPassCount = 2
	policy.Spec.Requirements[0].Variation = &signalv1.QualificationVariation{
		MinDistinctPlanDigests: 1,
		MinRunSeparation:       &metav1.Duration{Duration: 30 * time.Minute},
	}
	passA := qualificationRun("pass-a", node, signalv1.QualificationVerdictPass, now.Add(-2*time.Hour))
	passB := qualificationRun("pass-b", node, signalv1.QualificationVerdictPass, now.Add(-time.Hour))
	policy.Spec.Requirements[0].AcceptedPlans.Digests = []string{passA.Spec.PlanDigest}

	scheme := qualificationScheme(t)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, policy, passA, passB).Build()
	reconciler := &QualificationReconciler{
		Client:    kube,
		APIReader: kube,
		Evidence:  missingEvidenceStore{},
		Namespace: "signal-system",
		Now:       func() time.Time { return now },
	}

	status, err := reconciler.evaluateLedger(context.Background(), policy, node, qualificationLedger(policy, node))
	if err != nil {
		t.Fatalf("evaluateLedger(accumulate) error = %v", err)
	}

	if status.Phase != signalv1.NodeCertificationPhaseQualified ||
		len(status.ContributingRuns) != 2 {
		t.Fatalf("accumulated status = %#v", status)
	}

	nonPass := qualificationRun("non-pass", node, signalv1.QualificationVerdictFail, now.Add(-30*time.Minute))
	nonPass.Status.Phase = signalv1.RunPhaseFailed
	passC := qualificationRun("pass-c", node, signalv1.QualificationVerdictPass, now.Add(-10*time.Minute))
	kube = fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		node,
		policy,
		passA,
		passB,
		nonPass,
		passC,
	).Build()
	reconciler.Client = kube
	reconciler.APIReader = kube

	status, err = reconciler.evaluateLedger(context.Background(), policy, node, qualificationLedger(policy, node))
	if err != nil {
		t.Fatalf("evaluateLedger(reset) error = %v", err)
	}

	if status.Phase == signalv1.NodeCertificationPhaseQualified {
		t.Fatalf("pre-failure Runs retained credit after Reset: %#v", status)
	}
}

func TestEvaluateLedgerRevokeWatermarkSurvivesFailedRunDeletion(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	node := qualificationNode("node", "node-uid", "boot-a")
	policy := qualificationPolicy("policy", "policy-uid", signalv1.NonPassBehaviorRevoke)
	oldPass := qualificationRun("old-pass", node, signalv1.QualificationVerdictPass, now.Add(-time.Hour))
	failed := qualificationRun("failed", node, signalv1.QualificationVerdictFail, now.Add(-30*time.Minute))
	failed.Status.Phase = signalv1.RunPhaseFailed
	policy.Spec.Requirements[0].AcceptedPlans.Digests = []string{oldPass.Spec.PlanDigest}

	scheme := qualificationScheme(t)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, policy, oldPass, failed).Build()
	reconciler := &QualificationReconciler{
		Client:    kube,
		APIReader: kube,
		Evidence:  missingEvidenceStore{},
		Namespace: "signal-system",
		Now:       func() time.Time { return now },
	}
	ledger := qualificationLedger(policy, node)

	status, err := reconciler.evaluateLedger(context.Background(), policy, node, ledger)
	if err != nil {
		t.Fatalf("evaluateLedger(with failed Run) error = %v", err)
	}
	if status.Phase != signalv1.NodeCertificationPhaseRevoked ||
		status.LatestNonPass == nil ||
		status.LatestNonPass.UID != failed.UID {
		t.Fatalf("revoked status = %#v", status)
	}

	ledger.Status = status
	kube = fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, policy, oldPass).Build()
	reconciler.Client = kube
	reconciler.APIReader = kube

	status, err = reconciler.evaluateLedger(context.Background(), policy, node, ledger)
	if err != nil {
		t.Fatalf("evaluateLedger(after failed Run deletion) error = %v", err)
	}
	if status.Phase != signalv1.NodeCertificationPhaseRevoked ||
		status.LatestNonPass == nil ||
		status.LatestNonPass.UID != failed.UID {
		t.Fatalf("failed Run deletion laundered revocation: %#v", status)
	}

	newPass := qualificationRun("new-pass", node, signalv1.QualificationVerdictPass, now)
	kube = fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, policy, oldPass, newPass).Build()
	reconciler.Client = kube
	reconciler.APIReader = kube

	status, err = reconciler.evaluateLedger(context.Background(), policy, node, ledger)
	if err != nil {
		t.Fatalf("evaluateLedger(with later pass) error = %v", err)
	}
	if status.Phase != signalv1.NodeCertificationPhaseQualified ||
		len(status.ContributingRuns) != 1 ||
		status.ContributingRuns[0].UID != newPass.UID {
		t.Fatalf("later pass did not requalify after revocation: %#v", status)
	}
}

func TestEvaluateLedgerResetWatermarkSurvivesFailedRunDeletion(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	node := qualificationNode("node", "node-uid", "boot-a")
	policy := qualificationPolicy("policy", "policy-uid", signalv1.NonPassBehaviorReset)
	policy.Spec.Requirements[0].RequiredPassCount = 2
	oldPassA := qualificationRun("old-pass-a", node, signalv1.QualificationVerdictPass, now.Add(-2*time.Hour))
	oldPassB := qualificationRun("old-pass-b", node, signalv1.QualificationVerdictPass, now.Add(-time.Hour))
	failed := qualificationRun("failed", node, signalv1.QualificationVerdictFail, now.Add(-30*time.Minute))
	failed.Status.Phase = signalv1.RunPhaseFailed
	policy.Spec.Requirements[0].AcceptedPlans.Digests = []string{oldPassA.Spec.PlanDigest}

	scheme := qualificationScheme(t)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		node,
		policy,
		oldPassA,
		oldPassB,
		failed,
	).Build()
	reconciler := &QualificationReconciler{
		Client:    kube,
		APIReader: kube,
		Evidence:  missingEvidenceStore{},
		Namespace: "signal-system",
		Now:       func() time.Time { return now },
	}
	ledger := qualificationLedger(policy, node)

	status, err := reconciler.evaluateLedger(context.Background(), policy, node, ledger)
	if err != nil {
		t.Fatalf("evaluateLedger(with failed Run) error = %v", err)
	}
	if status.Phase == signalv1.NodeCertificationPhaseQualified ||
		status.LatestNonPass == nil ||
		status.LatestNonPass.UID != failed.UID ||
		len(status.ContributingRuns) != 0 ||
		len(status.PlanDigests) != 0 {
		t.Fatalf("reset status = %#v", status)
	}

	ledger.Status = status
	newPassA := qualificationRun("new-pass-a", node, signalv1.QualificationVerdictPass, now.Add(-10*time.Minute))
	kube = fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		node,
		policy,
		oldPassA,
		oldPassB,
		newPassA,
	).Build()
	reconciler.Client = kube
	reconciler.APIReader = kube

	status, err = reconciler.evaluateLedger(context.Background(), policy, node, ledger)
	if err != nil {
		t.Fatalf("evaluateLedger(after failed Run deletion) error = %v", err)
	}
	if status.Phase == signalv1.NodeCertificationPhaseQualified ||
		status.LatestNonPass == nil ||
		status.LatestNonPass.UID != failed.UID ||
		len(status.ContributingRuns) != 0 {
		t.Fatalf("failed Run deletion laundered reset: %#v", status)
	}

	ledger.Status = status
	newPassB := qualificationRun("new-pass-b", node, signalv1.QualificationVerdictPass, now)
	kube = fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		node,
		policy,
		oldPassA,
		oldPassB,
		newPassA,
		newPassB,
	).Build()
	reconciler.Client = kube
	reconciler.APIReader = kube

	status, err = reconciler.evaluateLedger(context.Background(), policy, node, ledger)
	if err != nil {
		t.Fatalf("evaluateLedger(with replacement passes) error = %v", err)
	}
	if status.Phase != signalv1.NodeCertificationPhaseQualified ||
		len(status.ContributingRuns) != 2 {
		t.Fatalf("replacement passes did not qualify after reset: %#v", status)
	}
}

func TestEvaluateLedgerAcceptedPlanAndHelperCredit(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	node := qualificationNode("helper", "helper-uid", "boot-a")
	policy := qualificationPolicy("policy", "policy-uid", signalv1.NonPassBehaviorIgnore)
	policy.Spec.Requirements[0].CreditRule = signalv1.QualificationCreditRequester
	policy.Spec.Requirements[0].AcceptedPlans.Digests = []string{"sha256:" + strings.Repeat("f", 64)}
	run := qualificationRun("run", node, signalv1.QualificationVerdictPass, now)
	run.Status.Evidence.Identity.Participants[0].Role = signalv1.ParticipantRoleParticipant

	scheme := qualificationScheme(t)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, policy, run).Build()
	reconciler := &QualificationReconciler{
		Client:    kube,
		APIReader: kube,
		Evidence:  missingEvidenceStore{},
		Namespace: "signal-system",
		Now:       func() time.Time { return now },
	}

	status, err := reconciler.evaluateLedger(context.Background(), policy, node, qualificationLedger(policy, node))
	if err != nil {
		t.Fatalf("evaluateLedger() error = %v", err)
	}

	if status.Phase == signalv1.NodeCertificationPhaseQualified {
		t.Fatalf("helper or unlisted plan received requester credit: %#v", status)
	}
}

func TestProtectedQualificationLabelGateAndMultiplePolicies(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	node := qualificationNode("node", "node-uid", "boot-a")
	node.Labels = map[string]string{signalv1.NodeQualificationLabel: signalv1.NodeQualificationValue}
	policyA := qualificationPolicy("policy-a", "policy-a-uid", signalv1.NonPassBehaviorIgnore)
	policyB := qualificationPolicy("policy-b", "policy-b-uid", signalv1.NonPassBehaviorIgnore)
	ledgerA := qualificationLedger(policyA, node)
	ledgerA.Status.Phase = signalv1.NodeCertificationPhaseRevoked
	ledgerB := qualificationLedger(policyB, node)
	setQualifiedLedger(t, ledgerB, policyB, node, now.Add(time.Hour))

	scheme := qualificationScheme(t)
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&signalv1.NodeCertification{}).
		WithObjects(node, policyA, policyB, ledgerA, ledgerB).
		Build()
	reconciler := &QualificationReconciler{
		Client:        kube,
		APIReader:     kube,
		Namespace:     "signal-system",
		GateProtected: true,
		Now:           func() time.Time { return now },
	}

	if err := reconciler.reconcileQualificationLabel(context.Background(), node); err != nil {
		t.Fatalf("reconcileQualificationLabel() error = %v", err)
	}

	var current corev1.Node
	if err := kube.Get(context.Background(), types.NamespacedName{Name: node.Name}, &current); err != nil {
		t.Fatal(err)
	}

	if _, exists := current.Labels[signalv1.NodeQualificationLabel]; exists {
		t.Fatalf("one unqualified ProtectedLabel Policy did not remove the shared label: %#v", current.Labels)
	}

	var currentLedgerA signalv1.NodeCertification
	if err := kube.Get(
		context.Background(),
		types.NamespacedName{Namespace: ledgerA.Namespace, Name: ledgerA.Name},
		&currentLedgerA,
	); err != nil {
		t.Fatal(err)
	}

	setQualifiedLedger(t, &currentLedgerA, policyA, node, now.Add(time.Hour))

	if err := kube.Status().Update(context.Background(), &currentLedgerA); err != nil {
		t.Fatal(err)
	}

	if err := reconciler.reconcileQualificationLabel(context.Background(), &current); err != nil {
		t.Fatalf("reconcileQualificationLabel(all policies qualified) error = %v", err)
	}

	if err := kube.Get(context.Background(), types.NamespacedName{Name: node.Name}, &current); err != nil {
		t.Fatal(err)
	}

	if current.Labels[signalv1.NodeQualificationLabel] != signalv1.NodeQualificationValue {
		t.Fatalf("all qualified ProtectedLabel Policies did not project the shared label: %#v", current.Labels)
	}

	if err := kube.Delete(context.Background(), ledgerB); err != nil {
		t.Fatal(err)
	}

	if err := reconciler.reconcileQualificationLabel(context.Background(), &current); err != nil {
		t.Fatalf("reconcileQualificationLabel(deleted ledger) error = %v", err)
	}

	if err := kube.Get(context.Background(), types.NamespacedName{Name: node.Name}, &current); err != nil {
		t.Fatal(err)
	}

	if _, exists := current.Labels[signalv1.NodeQualificationLabel]; exists {
		t.Fatalf("deleted ledger left an unbacked qualification label")
	}

	if current.Labels == nil {
		current.Labels = map[string]string{}
	}

	current.Labels[signalv1.NodeQualificationLabel] = signalv1.NodeQualificationValue
	if err := kube.Update(context.Background(), &current); err != nil {
		t.Fatal(err)
	}

	reconciler.GateProtected = false
	if err := reconciler.reconcileQualificationLabel(context.Background(), &current); err != nil {
		t.Fatalf("reconcileQualificationLabel(disabled) error = %v", err)
	}

	if err := kube.Get(context.Background(), types.NamespacedName{Name: node.Name}, &current); err != nil {
		t.Fatal(err)
	}

	if _, exists := current.Labels[signalv1.NodeQualificationLabel]; exists {
		t.Fatalf("disabled protection gate retained %q", signalv1.NodeQualificationLabel)
	}

	if err := kube.Get(
		context.Background(),
		types.NamespacedName{Namespace: ledgerA.Namespace, Name: ledgerA.Name},
		&currentLedgerA,
	); err != nil {
		t.Fatal(err)
	}

	gateCondition := apimeta.FindStatusCondition(
		currentLedgerA.Status.Conditions,
		signalv1.NodeCertificationConditionGateProjected,
	)
	if gateCondition == nil ||
		gateCondition.Status != metav1.ConditionFalse ||
		gateCondition.Reason != signalv1.NodeCertificationReasonProtectionUnverified {
		t.Fatalf("disabled gate condition = %#v", gateCondition)
	}

	if signalv1.NodeQualificationLabel != "node-restriction.kubernetes.io/project-signal-qualified" {
		t.Fatalf("qualification label key = %q", signalv1.NodeQualificationLabel)
	}
}

func TestNodeCertificationPolicyGenerationAdvancesMonotonically(t *testing.T) {
	t.Parallel()

	node := qualificationNode("node", "node-uid", "boot-a")
	policy := qualificationPolicy("policy", "policy-uid", signalv1.NonPassBehaviorIgnore)
	policy.Generation = 2
	ledger := qualificationLedger(policy, node)
	ledger.Spec.Policy.Generation = 1

	scheme := qualificationScheme(t)
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&signalv1.NodeCertification{}).
		WithObjects(node, policy, ledger).
		Build()
	reconciler := &QualificationReconciler{
		Client:    kube,
		APIReader: kube,
		Evidence:  missingEvidenceStore{},
		Namespace: "signal-system",
	}

	if err := reconciler.reconcileNodeLedger(context.Background(), policy, node); err != nil {
		t.Fatalf("reconcileNodeLedger() error = %v", err)
	}

	var current signalv1.NodeCertification
	if err := kube.Get(
		context.Background(),
		types.NamespacedName{Namespace: ledger.Namespace, Name: ledger.Name},
		&current,
	); err != nil {
		t.Fatal(err)
	}

	if current.Spec.Policy.Generation != policy.Generation ||
		current.Status.ObservedGeneration != policy.Generation {
		t.Fatalf(
			"policy generation = spec %d, observed %d, want %d",
			current.Spec.Policy.Generation,
			current.Status.ObservedGeneration,
			policy.Generation,
		)
	}
}

func TestQualificationCreditsAndPlanDigestsBoundedAt64(t *testing.T) {
	t.Parallel()

	records := make([]qualificationRecord, signalv1.MaxContributingRuns+1)
	for index := range records {
		records[index] = qualificationRecord{
			Reference: signalv1.ContributingRunReference{
				Name: "run-" + strconv.Itoa(index),
				UID:  types.UID("run-" + strconv.Itoa(index)),
			},
			Identity: signalv1.QualificationEvidenceIdentity{
				PlanDigest: "sha256:" + fmt.Sprintf("%064x", index),
			},
			FinishedAt: time.Unix(int64(index), 0),
		}
	}

	if got := len(recordReferences(records)); got != signalv1.MaxContributingRuns {
		t.Fatalf("contributing references = %d, want %d", got, signalv1.MaxContributingRuns)
	}

	if got := len(recordPlanDigests(records)); got != signalv1.MaxContributingRuns {
		t.Fatalf("plan digests = %d, want %d", got, signalv1.MaxContributingRuns)
	}
}

func TestRunProjectionDoesNotGrantProductionEligibility(t *testing.T) {
	t.Parallel()

	scheme := qualificationScheme(t)
	node := qualificationNode("node", "node-uid", "boot-a")
	node.Annotations = map[string]string{ReservationLabel: "run"}
	node.Labels = map[string]string{EligibilityLabel: "production"}
	run := qualificationRun("run", node, signalv1.QualificationVerdictPass, time.Now().UTC())
	run.Status.Phase = signalv1.RunPhaseSucceeded

	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	if err := projectTerminal(context.Background(), kube, run, time.Now().UTC()); err != nil {
		t.Fatalf("projectTerminal() error = %v", err)
	}

	var current corev1.Node
	if err := kube.Get(context.Background(), types.NamespacedName{Name: node.Name}, &current); err != nil {
		t.Fatal(err)
	}

	if _, exists := current.Labels[EligibilityLabel]; exists {
		t.Fatalf("terminal Run retained direct eligibility projection: %#v", current.Labels)
	}
}

type missingEvidenceStore struct{}

func (missingEvidenceStore) Put(context.Context, string, []byte) (string, string, error) {
	return "", "", nil
}

func (missingEvidenceStore) Get(context.Context, string) ([]byte, string, string, bool, error) {
	return nil, "", "", false, nil
}

type corruptEvidenceStore struct{}

func (corruptEvidenceStore) Put(context.Context, string, []byte) (string, string, error) {
	return "", "", nil
}

func (corruptEvidenceStore) Get(context.Context, string) ([]byte, string, string, bool, error) {
	return []byte("{"), "", "sha256:" + strings.Repeat("b", 64), true, nil
}

type failingNodeReader struct {
	client.Reader
	err error
}

func (r failingNodeReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return r.err
}

type countingClient struct {
	client.Client
	runLists int
}

func (c *countingClient) List(
	ctx context.Context,
	list client.ObjectList,
	options ...client.ListOption,
) error {
	if _, ok := list.(*signalv1.CertificationRunList); ok {
		c.runLists++
	}

	return c.Client.List(ctx, list, options...)
}

func qualificationScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	if err := signalv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	return scheme
}

func qualificationNode(name string, uid types.UID, bootID string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid},
		Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{
			BootID:         bootID,
			KernelVersion:  "kernel",
			OSImage:        "os",
			KubeletVersion: "kubelet",
			SystemUUID:     "system",
		}},
	}
}

func qualificationPolicy(
	name string,
	uid types.UID,
	nonPass signalv1.NonPassBehavior,
) *signalv1.CertificationPolicy {
	return &signalv1.CertificationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid, Generation: 1},
		Spec: signalv1.CertificationPolicySpec{
			Requirements: []signalv1.CertificationRequirement{{
				Name:              "baseline",
				Profile:           signalv1.PolicyProfileReference{Name: "profile"},
				RequiredPassCount: 1,
				CreditRule:        signalv1.QualificationCreditPerParticipant,
				AcceptedPlans: signalv1.AcceptedPlanPolicy{
					Mode:    signalv1.AcceptedPlanModeListed,
					Digests: []string{"sha256:" + strings.Repeat("d", 64)},
				},
			}},
			Validity:                      metav1.Duration{Duration: 2 * time.Hour},
			RenewBefore:                   metav1.Duration{Duration: time.Hour},
			InvalidationFingerprintFields: []signalv1.FingerprintField{signalv1.FingerprintFieldNodeUID},
			NonPassBehavior:               nonPass,
			GateProjection:                signalv1.GateProjection{Mode: signalv1.GateProjectionProtectedLabel},
		},
	}
}

func qualificationLedger(
	policy *signalv1.CertificationPolicy,
	node *corev1.Node,
) *signalv1.NodeCertification {
	return &signalv1.NodeCertification{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nodeCertificationName(policy.UID, node.UID),
			Namespace: "signal-system",
			Labels: map[string]string{
				qualificationPolicyUIDLabel: string(policy.UID),
				qualificationNodeUIDLabel:   string(node.UID),
			},
		},
		Spec: signalv1.NodeCertificationSpec{
			Policy: signalv1.PinnedPolicy{
				Name:       policy.Name,
				UID:        policy.UID,
				Generation: policy.Generation,
			},
			NodeName: node.Name,
			NodeUID:  node.UID,
		},
	}
}

func setQualifiedLedger(
	t *testing.T,
	ledger *signalv1.NodeCertification,
	policy *signalv1.CertificationPolicy,
	node *corev1.Node,
	expiresAt time.Time,
) {
	t.Helper()

	digest, err := policyDigest(policy.Spec)
	if err != nil {
		t.Fatal(err)
	}

	fingerprint := nodeFingerprint(node)
	expires := metav1.NewTime(expiresAt)
	ledger.Spec.Policy.Generation = policy.Generation
	ledger.Status.Phase = signalv1.NodeCertificationPhaseQualified
	ledger.Status.ObservedGeneration = policy.Generation
	ledger.Status.PolicyDigest = digest
	ledger.Status.Fingerprint = &fingerprint
	ledger.Status.ExpiresAt = &expires
}

func qualificationRun(
	name string,
	node *corev1.Node,
	verdict signalv1.QualificationVerdict,
	finishedAt time.Time,
) *signalv1.CertificationRun {
	startedAt := metav1.NewTime(finishedAt.Add(-time.Minute))
	finished := metav1.NewTime(finishedAt)
	run := &signalv1.CertificationRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "signal-system",
			UID:       types.UID(name + "-uid"),
		},
		Spec: signalv1.CertificationRunSpec{
			Profile: signalv1.PinnedProfile{Name: "profile", UID: "profile-uid", Generation: 1},
			Suite:   signalv1.PinnedSuite{Name: "suite", UID: "suite-uid", Generation: 1},
			Participants: []signalv1.Participant{{
				NodeName: node.Name,
				NodeUID:  node.UID,
			}},
			ScriptBundle: signalv1.ScriptBundleReference{
				Name:   "scripts",
				UID:    "scripts-uid",
				Digest: "sha256:" + strings.Repeat("b", 64),
			},
			Checks: []signalv1.CertificationCheckSnapshot{{
				ID:                    signalv1.CheckID("R01"),
				Script:                signalv1.ScriptSnapshot{Key: "check.sh", Digest: "sha256:" + strings.Repeat("c", 64)},
				RunnerImage:           pinnedImage("runner", "e"),
				Backend:               signalv1.ExecutionBackendPod,
				BackendActiveDeadline: metav1.Duration{Duration: time.Minute},
			}},
			PlanProvenance: &signalv1.ExecutionPlanProvenance{
				AdapterImage:           pinnedImage("adapter", "f"),
				WorkflowTemplateDigest: "sha256:" + strings.Repeat("a", 64),
			},
		},
		Status: signalv1.CertificationRunStatus{
			Phase: signalv1.RunPhaseSucceeded,
			Evidence: signalv1.EvidenceStatus{
				URI:    "kubernetes://signal-system/" + name,
				Digest: "sha256:" + strings.Repeat("9", 64),
				Identity: &signalv1.QualificationEvidenceIdentity{
					Participants: []signalv1.ParticipantEvidenceIdentity{{
						Index:   0,
						Role:    signalv1.ParticipantRoleRequester,
						Verdict: verdict,
						Start:   nodeFingerprint(node),
						End:     nodeFingerprint(node),
					}},
					Checks: []signalv1.CheckContinuitySummary{{
						Order:            0,
						StartedAt:        startedAt,
						FinishedAt:       finished,
						ObservedDuration: metav1.Duration{Duration: time.Minute},
						Completed:        true,
						SameBootID:       true,
					}},
					Provenance: signalv1.EvidenceProvenance{
						ControllerImage:        pinnedImage("controller", "a"),
						AdapterImage:           pinnedImage("adapter", "f"),
						WorkflowTemplateDigest: "sha256:" + strings.Repeat("a", 64),
						RunnerImages:           []string{pinnedImage("runner", "e")},
					},
				},
			},
		},
	}

	planDigest, err := signalv1.DeriveExecutionPlanDigest(
		1,
		run.Spec.ScriptBundle.Digest,
		run.Spec.Checks,
		*run.Spec.PlanProvenance,
	)
	if err != nil {
		panic(err)
	}

	run.Spec.PlanDigest = planDigest
	run.Status.Evidence.Identity.PlanDigest = planDigest

	return run
}

func pinnedImage(name, character string) string {
	return "registry.invalid/" + name + "@sha256:" + strings.Repeat(character, 64)
}

func refreshPlanDigest(run *signalv1.CertificationRun) {
	planDigest, err := signalv1.DeriveExecutionPlanDigest(
		int32(len(run.Spec.Participants)),
		run.Spec.ScriptBundle.Digest,
		run.Spec.Checks,
		*run.Spec.PlanProvenance,
	)
	if err != nil {
		panic(err)
	}

	run.Spec.PlanDigest = planDigest
	run.Status.Evidence.Identity.PlanDigest = planDigest
}
