// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func TestCampaignEnrollmentCreatesReadyMatchingCampaign(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.November, 17, 22, 0, 0, 0, time.UTC)
	node := readyCampaignNode()
	profile := campaignProfile()
	suite := campaignSuite()
	policy := campaignPolicy()
	kube := fake.NewClientBuilder().
		WithScheme(campaignTestScheme(t)).
		WithObjects(node, profile, suite, policy).
		Build()

	reconciler := &CampaignEnrollmentReconciler{
		Client:    kube,
		Namespace: "project-signal",
		Now:       func() time.Time { return now },
	}

	if _, err := reconciler.Reconcile(
		context.Background(),
		ctrl.Request{NamespacedName: client.ObjectKey{Name: node.Name}},
	); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	var campaigns signalv1.CertificationCampaignList
	if err := kube.List(context.Background(), &campaigns); err != nil {
		t.Fatalf("list campaigns: %v", err)
	}

	if len(campaigns.Items) != 1 {
		t.Fatalf("campaigns = %d, want 1", len(campaigns.Items))
	}

	campaign := campaigns.Items[0]
	if campaign.Spec.NodeUID != node.UID ||
		campaign.Spec.Profile.UID != profile.UID ||
		campaign.Spec.Suite.UID != suite.UID ||
		campaign.Spec.Policy.UID != policy.UID {
		t.Fatalf("campaign identity = %#v", campaign.Spec)
	}

	if got, want := campaign.Spec.Deadline.Time, now.Add(24*time.Hour); !got.Equal(want) {
		t.Fatalf("deadline = %s, want %s", got, want)
	}

	if campaign.Spec.StopOnFailure {
		t.Fatal("StopOnFailure = true, want default false")
	}
}

func TestCampaignEnrollmentSkipsUnreadyNode(t *testing.T) {
	t.Parallel()

	node := readyCampaignNode()
	node.Status.Conditions[0].Status = corev1.ConditionFalse
	kube := fake.NewClientBuilder().
		WithScheme(campaignTestScheme(t)).
		WithObjects(node, campaignProfile(), campaignSuite(), campaignPolicy()).
		Build()

	reconciler := &CampaignEnrollmentReconciler{
		Client:    kube,
		Namespace: "project-signal",
	}

	if _, err := reconciler.Reconcile(
		context.Background(),
		ctrl.Request{NamespacedName: client.ObjectKey{Name: node.Name}},
	); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	var campaigns signalv1.CertificationCampaignList
	if err := kube.List(context.Background(), &campaigns); err != nil {
		t.Fatalf("list campaigns: %v", err)
	}

	if len(campaigns.Items) != 0 {
		t.Fatalf("campaigns = %d, want 0", len(campaigns.Items))
	}
}

func TestCampaignEnrollmentPinsStopOnFailure(t *testing.T) {
	t.Parallel()

	node := readyCampaignNode()
	policy := campaignPolicy()
	policy.Spec.StopOnFailure = true
	kube := fake.NewClientBuilder().
		WithScheme(campaignTestScheme(t)).
		WithObjects(node, campaignProfile(), campaignSuite(), policy).
		Build()
	reconciler := &CampaignEnrollmentReconciler{
		Client:    kube,
		Namespace: "project-signal",
	}

	if _, err := reconciler.Reconcile(
		context.Background(),
		ctrl.Request{NamespacedName: client.ObjectKey{Name: node.Name}},
	); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	var campaigns signalv1.CertificationCampaignList
	if err := kube.List(context.Background(), &campaigns); err != nil {
		t.Fatalf("list campaigns: %v", err)
	}

	if len(campaigns.Items) != 1 || !campaigns.Items[0].Spec.StopOnFailure {
		t.Fatalf("campaigns = %#v, want one fail-fast campaign", campaigns.Items)
	}
}

func TestCampaignEnrollmentKeepsExistingCampaignAfterProfileRemoval(t *testing.T) {
	t.Parallel()

	node := readyCampaignNode()
	policy := campaignPolicy()
	campaign := campaignExecution(time.Date(2026, time.November, 17, 22, 0, 0, 0, time.UTC))
	campaign.Name = campaignName(policy.UID, node.UID)
	kube := fake.NewClientBuilder().
		WithScheme(campaignTestScheme(t)).
		WithObjects(node, policy, campaign).
		Build()
	reconciler := &CampaignEnrollmentReconciler{
		Client:    kube,
		Namespace: campaign.Namespace,
	}

	if _, err := reconciler.Reconcile(
		context.Background(),
		ctrl.Request{NamespacedName: client.ObjectKey{Name: node.Name}},
	); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
}

func TestCampaignContinuesAfterFailedReleasedRun(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.November, 17, 22, 0, 0, 0, time.UTC)
	node := readyCampaignNode()
	campaign := campaignExecution(now)
	kube := campaignFakeClient(t, node, campaign, campaignProfile(), campaignSuite())
	reconciler := &CampaignReconciler{
		Client:    kube,
		Namespace: campaign.Namespace,
		Now:       func() time.Time { return now },
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(campaign)}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("request first Run: %v", err)
	}

	var requested corev1.Node
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(node), &requested); err != nil {
		t.Fatalf("get requested Node: %v", err)
	}

	if requested.Labels[RequestLabel] != campaign.Spec.Profile.Name ||
		requested.Annotations[CampaignRequestUID] != string(campaign.UID) ||
		requested.Annotations[CampaignRequestSequence] != "1" {
		t.Fatalf("campaign request metadata = labels %#v annotations %#v", requested.Labels, requested.Annotations)
	}

	run := campaignRun(campaign, signalv1.RunPhaseRunning)
	if err := kube.Create(context.Background(), run); err != nil {
		t.Fatalf("create campaign Run: %v", err)
	}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("observe active Run: %v", err)
	}

	var current signalv1.CertificationCampaign
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(campaign), &current); err != nil {
		t.Fatalf("get current Campaign: %v", err)
	}

	if current.Status.StartedRuns != 1 ||
		current.Status.CurrentRun == nil ||
		current.Status.CurrentRun.UID != run.UID {
		t.Fatalf("active campaign status = %#v", current.Status)
	}

	var failed signalv1.CertificationRun
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), &failed); err != nil {
		t.Fatalf("get Run: %v", err)
	}

	failed.Status.Phase = signalv1.RunPhaseFailed
	if err := kube.Status().Update(context.Background(), &failed); err != nil {
		t.Fatalf("mark Run failed: %v", err)
	}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("request failed Run release: %v", err)
	}

	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), &failed); err != nil {
		t.Fatalf("get releasing Run: %v", err)
	}

	if !failed.Spec.Release {
		t.Fatal("failed campaign Run was not released")
	}

	failed.Status.Conditions = []metav1.Condition{{
		Type:   signalv1.RunConditionReleased,
		Status: metav1.ConditionTrue,
		Reason: "Released",
	}}
	if err := kube.Status().Update(context.Background(), &failed); err != nil {
		t.Fatalf("mark Run released: %v", err)
	}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("record failed Run: %v", err)
	}

	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(campaign), &current); err != nil {
		t.Fatalf("get completed Campaign status: %v", err)
	}

	if current.Status.FailedRuns != 1 ||
		current.Status.LastRun == nil ||
		current.Status.LastRun.Sequence != 1 ||
		current.Status.CurrentRun != nil {
		t.Fatalf("completed campaign status = %#v", current.Status)
	}

	requested.Labels = map[string]string{"campaign": "enabled"}

	requested.Annotations = nil
	if err := kube.Update(context.Background(), &requested); err != nil {
		t.Fatalf("clear admitted request: %v", err)
	}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("request second Run: %v", err)
	}

	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(node), &requested); err != nil {
		t.Fatalf("get second requested Node: %v", err)
	}

	if requested.Annotations[CampaignRequestSequence] != "2" {
		t.Fatalf("second sequence = %q, want 2", requested.Annotations[CampaignRequestSequence])
	}
}

func TestCampaignDeadlineStopsNewRuns(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.November, 18, 22, 0, 0, 0, time.UTC)
	node := readyCampaignNode()
	campaign := campaignExecution(now.Add(-24 * time.Hour))
	campaign.Spec.Deadline = metav1.NewTime(now)
	node.Labels[RequestLabel] = campaign.Spec.Profile.Name
	node.Annotations = map[string]string{
		CampaignRequestName:     campaign.Name,
		CampaignRequestUID:      string(campaign.UID),
		CampaignRequestSequence: "1",
	}
	kube := campaignFakeClient(t, node, campaign, campaignProfile(), campaignSuite())
	reconciler := &CampaignReconciler{
		Client:    kube,
		Namespace: campaign.Namespace,
		Now:       func() time.Time { return now },
	}

	if _, err := reconciler.Reconcile(
		context.Background(),
		ctrl.Request{NamespacedName: client.ObjectKeyFromObject(campaign)},
	); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	var completed signalv1.CertificationCampaign
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(campaign), &completed); err != nil {
		t.Fatalf("get Campaign: %v", err)
	}

	if completed.Status.Phase != signalv1.CampaignPhaseCompleted {
		t.Fatalf("phase = %q, want Completed", completed.Status.Phase)
	}

	var unchanged corev1.Node
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(node), &unchanged); err != nil {
		t.Fatalf("get Node: %v", err)
	}

	if unchanged.Labels[RequestLabel] != "" {
		t.Fatalf("deadline campaign requested Run with label %q", unchanged.Labels[RequestLabel])
	}

	if unchanged.Annotations[CampaignRequestUID] != "" {
		t.Fatalf("deadline campaign retained request annotations: %#v", unchanged.Annotations)
	}
}

func TestCampaignStopsAfterFailedReleasedRunWhenConfigured(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.November, 17, 22, 0, 0, 0, time.UTC)
	node := readyCampaignNode()
	campaign := campaignExecution(now)
	campaign.Spec.StopOnFailure = true
	run := campaignRun(campaign, signalv1.RunPhaseFailed)
	run.Status.Conditions = []metav1.Condition{{
		Type:   signalv1.RunConditionReleased,
		Status: metav1.ConditionTrue,
		Reason: "Released",
	}}
	kube := campaignFakeClient(t, node, campaign, campaignProfile(), campaignSuite(), run)
	reconciler := &CampaignReconciler{
		Client:    kube,
		Namespace: campaign.Namespace,
		Now:       func() time.Time { return now },
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(campaign)}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("record failed Run: %v", err)
	}

	var completed signalv1.CertificationCampaign
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(campaign), &completed); err != nil {
		t.Fatalf("get Campaign: %v", err)
	}

	if completed.Status.Phase != signalv1.CampaignPhaseCompleted ||
		completed.Status.FailedRuns != 1 ||
		completed.Status.LastRun == nil ||
		completed.Status.CurrentRun != nil {
		t.Fatalf("completed campaign status = %#v", completed.Status)
	}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("reconcile completed Campaign: %v", err)
	}

	var unchanged corev1.Node
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(node), &unchanged); err != nil {
		t.Fatalf("get Node: %v", err)
	}

	if unchanged.Labels[RequestLabel] != "" {
		t.Fatalf("completed campaign requested another Run with label %q", unchanged.Labels[RequestLabel])
	}
}

func TestCampaignBlocksAndClearsRequestOnSuiteDrift(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.November, 17, 22, 0, 0, 0, time.UTC)
	node := readyCampaignNode()
	campaign := campaignExecution(now)
	node.Labels[RequestLabel] = campaign.Spec.Profile.Name
	node.Annotations = map[string]string{
		CampaignRequestName:     campaign.Name,
		CampaignRequestUID:      string(campaign.UID),
		CampaignRequestSequence: "1",
	}
	suite := campaignSuite()
	suite.Generation++
	kube := campaignFakeClient(t, node, campaign, campaignProfile(), suite)
	reconciler := &CampaignReconciler{
		Client:    kube,
		Namespace: campaign.Namespace,
		Now:       func() time.Time { return now },
	}

	if _, err := reconciler.Reconcile(
		context.Background(),
		ctrl.Request{NamespacedName: client.ObjectKeyFromObject(campaign)},
	); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	var blocked signalv1.CertificationCampaign
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(campaign), &blocked); err != nil {
		t.Fatalf("get Campaign: %v", err)
	}

	if blocked.Status.Phase != signalv1.CampaignPhaseBlocked {
		t.Fatalf("phase = %q, want Blocked", blocked.Status.Phase)
	}

	var cleared corev1.Node
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(node), &cleared); err != nil {
		t.Fatalf("get Node: %v", err)
	}

	if cleared.Labels[RequestLabel] != "" || cleared.Annotations[CampaignRequestUID] != "" {
		t.Fatalf("drifted request was not cleared: labels %#v annotations %#v", cleared.Labels, cleared.Annotations)
	}
}

func TestCampaignIdentityRejectsUnexpectedSequence(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.November, 17, 22, 0, 0, 0, time.UTC)
	node := readyCampaignNode()
	campaign := campaignExecution(now)
	campaign.Status.StartedRuns = 1
	node.Labels[RequestLabel] = campaign.Spec.Profile.Name
	node.Annotations = map[string]string{
		CampaignRequestName:     campaign.Name,
		CampaignRequestUID:      string(campaign.UID),
		CampaignRequestSequence: "1",
	}
	kube := campaignFakeClient(t, node, campaign)

	if _, err := campaignIdentityFromNode(
		context.Background(),
		kube,
		campaign.Namespace,
		node,
		campaignProfile(),
		campaignSuite(),
		now,
	); err == nil {
		t.Fatal("campaignIdentityFromNode() accepted repeated sequence")
	}
}

func TestCampaignIdentityRejectsProfileDrift(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.November, 17, 22, 0, 0, 0, time.UTC)
	node := readyCampaignNode()
	campaign := campaignExecution(now)
	node.Labels[RequestLabel] = campaign.Spec.Profile.Name
	node.Annotations = map[string]string{
		CampaignRequestName:     campaign.Name,
		CampaignRequestUID:      string(campaign.UID),
		CampaignRequestSequence: "1",
	}
	kube := campaignFakeClient(t, node, campaign)
	profile := campaignProfile()
	profile.Generation++

	if _, err := campaignIdentityFromNode(
		context.Background(),
		kube,
		campaign.Namespace,
		node,
		profile,
		campaignSuite(),
		now,
	); err == nil {
		t.Fatal("campaignIdentityFromNode() accepted Profile generation drift")
	}
}

func campaignTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	if err := signalv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add Signal scheme: %v", err)
	}

	return scheme
}

func campaignFakeClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()

	return fake.NewClientBuilder().
		WithScheme(campaignTestScheme(t)).
		WithStatusSubresource(&signalv1.CertificationCampaign{}, &signalv1.CertificationRun{}).
		WithIndex(
			&signalv1.CertificationRun{},
			campaignRunUIDField,
			func(object client.Object) []string {
				run := object.(*signalv1.CertificationRun)
				if run.Spec.Campaign == nil {
					return nil
				}

				return []string{string(run.Spec.Campaign.UID)}
			},
		).
		WithObjects(objects...).
		Build()
}

func readyCampaignNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "participant",
			UID:    types.UID("node-uid"),
			Labels: map[string]string{"campaign": "enabled"},
		},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type:   corev1.NodeReady,
			Status: corev1.ConditionTrue,
		}}},
	}
}

func campaignProfile() *signalv1.CertificationProfile {
	return &signalv1.CertificationProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "complete-profile",
			UID:        types.UID("profile-uid"),
			Generation: 3,
		},
		Spec: signalv1.CertificationProfileSpec{
			Suite: signalv1.CertificationSuiteReference{Name: "complete-suite"},
		},
	}
}

func campaignSuite() *signalv1.CertificationSuite {
	return &signalv1.CertificationSuite{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "complete-suite",
			UID:        types.UID("suite-uid"),
			Generation: 5,
		},
	}
}

func campaignPolicy() *signalv1.CertificationCampaignPolicy {
	return &signalv1.CertificationCampaignPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "initial-validation",
			UID:        types.UID("policy-uid"),
			Generation: 2,
		},
		Spec: signalv1.CertificationCampaignPolicySpec{
			Profile: signalv1.PolicyProfileReference{Name: "complete-profile"},
			NodeSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"campaign": "enabled"},
			},
			Trigger:  signalv1.CampaignTriggerOnNodeReady,
			Duration: metav1.Duration{Duration: 24 * time.Hour},
		},
	}
}

func campaignExecution(start time.Time) *signalv1.CertificationCampaign {
	return &signalv1.CertificationCampaign{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "campaign-execution",
			Namespace:  "project-signal",
			UID:        types.UID("campaign-uid"),
			Generation: 1,
		},
		Spec: signalv1.CertificationCampaignSpec{
			Policy: signalv1.PinnedCampaignPolicy{
				Name:       "initial-validation",
				UID:        types.UID("policy-uid"),
				Generation: 2,
			},
			Profile: signalv1.PinnedProfile{
				Name:       "complete-profile",
				UID:        types.UID("profile-uid"),
				Generation: 3,
			},
			Suite: signalv1.PinnedSuite{
				Name:       "complete-suite",
				UID:        types.UID("suite-uid"),
				Generation: 5,
			},
			NodeName:  "participant",
			NodeUID:   types.UID("node-uid"),
			StartedAt: metav1.NewTime(start),
			Deadline:  metav1.NewTime(start.Add(24 * time.Hour)),
		},
	}
}

func campaignRun(
	campaign *signalv1.CertificationCampaign,
	phase signalv1.RunPhase,
) *signalv1.CertificationRun {
	return &signalv1.CertificationRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "campaign-run-1",
			Namespace: campaign.Namespace,
			UID:       types.UID("run-uid"),
		},
		Spec: signalv1.CertificationRunSpec{
			Campaign: &signalv1.CampaignRunIdentity{
				Name:     campaign.Name,
				UID:      campaign.UID,
				Sequence: 1,
			},
		},
		Status: signalv1.CertificationRunStatus{Phase: phase},
	}
}
