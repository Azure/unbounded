// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/recorder"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

const (
	campaignNodeNameField = "spec.nodeName"
	campaignRunUIDField   = "spec.campaign.uid"

	campaignConditionReady = "Ready"
)

// +kubebuilder:rbac:groups=signal.unbounded-cloud.io,resources=certificationcampaignpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=signal.unbounded-cloud.io,resources=certificationcampaigns,verbs=get;list;watch;create;patch;update
// +kubebuilder:rbac:groups=signal.unbounded-cloud.io,resources=certificationcampaigns/status,verbs=get;patch;update

type CampaignEnrollmentReconciler struct {
	client.Client
	Namespace string
	Now       func() time.Time
	Recorder  recorder.EventRecorder
}

func (r *CampaignEnrollmentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("campaign-enrollment").
		For(&corev1.Node{}).
		Watches(
			&signalv1.CertificationCampaignPolicy{},
			handler.EnqueueRequestsFromMapFunc(r.requestsForPolicy),
		).
		Watches(
			&signalv1.CertificationProfile{},
			handler.EnqueueRequestsFromMapFunc(r.requestsForProfile),
		).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

func (r *CampaignEnrollmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: req.Name}, &node); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !nodeReady(&node) || !node.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	var policies signalv1.CertificationCampaignPolicyList
	if err := r.List(ctx, &policies); err != nil {
		return ctrl.Result{}, fmt.Errorf("list CertificationCampaignPolicies: %w", err)
	}

	for index := range policies.Items {
		policy := &policies.Items[index]
		if policy.Spec.Trigger != signalv1.CampaignTriggerOnNodeReady {
			continue
		}

		selector, err := metav1.LabelSelectorAsSelector(&policy.Spec.NodeSelector)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("compile campaign policy %s selector: %w", policy.Name, err)
		}

		if !selector.Matches(labels.Set(node.Labels)) {
			continue
		}

		if err := r.ensureCampaign(ctx, &node, policy); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *CampaignEnrollmentReconciler) ensureCampaign(
	ctx context.Context,
	node *corev1.Node,
	policy *signalv1.CertificationCampaignPolicy,
) error {
	name := campaignName(policy.UID, node.UID)

	var existing signalv1.CertificationCampaign

	err := r.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: name}, &existing)
	if err == nil {
		return nil
	}

	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get CertificationCampaign %s: %w", name, err)
	}

	var profile signalv1.CertificationProfile
	if err := r.Get(ctx, client.ObjectKey{Name: policy.Spec.Profile.Name}, &profile); err != nil {
		return fmt.Errorf("get campaign Profile %s: %w", policy.Spec.Profile.Name, err)
	}

	if policy.Spec.Profile.UID != "" && policy.Spec.Profile.UID != profile.UID {
		return fmt.Errorf(
			"campaign policy %s requires Profile %s UID %s, got %s",
			policy.Name,
			profile.Name,
			policy.Spec.Profile.UID,
			profile.UID,
		)
	}

	var suite signalv1.CertificationSuite
	if err := r.Get(ctx, client.ObjectKey{Name: profile.Spec.Suite.Name}, &suite); err != nil {
		return fmt.Errorf("get campaign Suite %s: %w", profile.Spec.Suite.Name, err)
	}

	now := r.now()
	campaign := &signalv1.CertificationCampaign{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: r.Namespace,
			Labels: map[string]string{
				RunNodeLabel: node.Name,
			},
		},
		Spec: signalv1.CertificationCampaignSpec{
			Policy: signalv1.PinnedCampaignPolicy{
				Name:       policy.Name,
				UID:        policy.UID,
				Generation: policy.Generation,
			},
			Profile: signalv1.PinnedProfile{
				Name:       profile.Name,
				UID:        profile.UID,
				Generation: profile.Generation,
			},
			Suite: signalv1.PinnedSuite{
				Name:       suite.Name,
				UID:        suite.UID,
				Generation: suite.Generation,
			},
			NodeName:      node.Name,
			NodeUID:       node.UID,
			StartedAt:     metav1.NewTime(now),
			Deadline:      metav1.NewTime(now.Add(policy.Spec.Duration.Duration)),
			StopOnFailure: policy.Spec.StopOnFailure,
		},
	}

	if err := r.Create(ctx, campaign); err != nil {
		return fmt.Errorf("create CertificationCampaign %s: %w", name, err)
	}

	if r.Recorder != nil {
		r.Recorder.Eventf(
			campaign,
			nil,
			corev1.EventTypeNormal,
			"CampaignEnrolled",
			"EnrollCampaign",
			"Enrolled node %s for Profile %s until %s",
			node.Name,
			profile.Name,
			campaign.Spec.Deadline.Time.UTC().Format(time.RFC3339),
		)
	}

	return nil
}

func (r *CampaignEnrollmentReconciler) requestsForPolicy(
	ctx context.Context,
	object client.Object,
) []ctrl.Request {
	policy, ok := object.(*signalv1.CertificationCampaignPolicy)
	if !ok {
		return nil
	}

	selector, err := metav1.LabelSelectorAsSelector(&policy.Spec.NodeSelector)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to compile campaign policy selector", "policy", policy.Name)

		return nil
	}

	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes, client.MatchingLabelsSelector{Selector: selector}); err != nil {
		log.FromContext(ctx).Error(err, "failed to list Nodes for campaign policy", "policy", policy.Name)

		return nil
	}

	requests := make([]ctrl.Request, 0, len(nodes.Items))
	for index := range nodes.Items {
		requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKey{Name: nodes.Items[index].Name}})
	}

	return requests
}

func (r *CampaignEnrollmentReconciler) requestsForProfile(
	ctx context.Context,
	object client.Object,
) []ctrl.Request {
	profile, ok := object.(*signalv1.CertificationProfile)
	if !ok {
		return nil
	}

	var policies signalv1.CertificationCampaignPolicyList
	if err := r.List(ctx, &policies); err != nil {
		log.FromContext(ctx).Error(err, "failed to list campaign policies for Profile", "profile", profile.Name)

		return nil
	}

	requests := make([]ctrl.Request, 0)

	for index := range policies.Items {
		if policies.Items[index].Spec.Profile.Name != profile.Name {
			continue
		}

		requests = append(requests, r.requestsForPolicy(ctx, &policies.Items[index])...)
	}

	return uniqueRequests(requests)
}

func (r *CampaignEnrollmentReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}

	return time.Now().UTC()
}

type CampaignReconciler struct {
	client.Client
	Namespace string
	Now       func() time.Time
	Recorder  recorder.EventRecorder
}

func (r *CampaignReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&signalv1.CertificationCampaign{},
		campaignNodeNameField,
		func(object client.Object) []string {
			campaign, ok := object.(*signalv1.CertificationCampaign)
			if !ok || campaign.Spec.NodeName == "" {
				return nil
			}

			return []string{campaign.Spec.NodeName}
		},
	); err != nil {
		return fmt.Errorf("index CertificationCampaigns by Node: %w", err)
	}

	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&signalv1.CertificationRun{},
		campaignRunUIDField,
		func(object client.Object) []string {
			run, ok := object.(*signalv1.CertificationRun)
			if !ok || run.Spec.Campaign == nil || run.Spec.Campaign.UID == "" {
				return nil
			}

			return []string{string(run.Spec.Campaign.UID)}
		},
	); err != nil {
		return fmt.Errorf("index CertificationRuns by campaign UID: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&signalv1.CertificationCampaign{}).
		Watches(
			&signalv1.CertificationRun{},
			handler.EnqueueRequestsFromMapFunc(r.requestForRun),
		).
		Watches(
			&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(r.requestsForNode),
		).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

func (r *CampaignReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var campaign signalv1.CertificationCampaign
	if err := r.Get(ctx, req.NamespacedName, &campaign); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !campaign.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	if campaign.Status.Phase == signalv1.CampaignPhaseCompleted {
		return ctrl.Result{}, nil
	}

	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: campaign.Spec.NodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return r.blockCampaign(ctx, &campaign, "NodeMissing", "The campaign node no longer exists")
		}

		return ctrl.Result{}, err
	}

	if node.UID != campaign.Spec.NodeUID {
		return r.blockCampaign(ctx, &campaign, "NodeReplaced", "The campaign node identity was replaced")
	}

	run, err := r.currentRun(ctx, &campaign)
	if err != nil {
		return ctrl.Result{}, err
	}

	if run != nil {
		return r.reconcileRun(ctx, &campaign, run)
	}

	now := r.now()
	if campaign.Spec.Stop || !now.Before(campaign.Spec.Deadline.Time) {
		if err := r.clearPendingRequest(ctx, &node, &campaign); err != nil {
			return ctrl.Result{}, err
		}

		return r.completeCampaign(ctx, &campaign)
	}

	if err := r.requirePinnedProfileAndSuite(ctx, &campaign); err != nil {
		if clearErr := r.clearPendingRequest(ctx, &node, &campaign); clearErr != nil {
			return ctrl.Result{}, clearErr
		}

		return r.blockCampaign(ctx, &campaign, "PlanDrift", err.Error())
	}

	if !nodeReady(&node) {
		return r.blockCampaign(ctx, &campaign, "NodeNotReady", "The campaign node is not Ready")
	}

	if node.Annotations[ReservationLabel] != "" {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if node.Labels[RequestLabel] != "" {
		if node.Annotations[CampaignRequestUID] == string(campaign.UID) {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	before := node.DeepCopy()
	if node.Labels == nil {
		node.Labels = make(map[string]string)
	}

	if node.Annotations == nil {
		node.Annotations = make(map[string]string)
	}

	sequence := campaign.Status.StartedRuns + 1
	node.Labels[RequestLabel] = campaign.Spec.Profile.Name
	node.Annotations[CampaignRequestName] = campaign.Name
	node.Annotations[CampaignRequestUID] = string(campaign.UID)
	node.Annotations[CampaignRequestSequence] = strconv.FormatInt(int64(sequence), 10)

	if err := r.Patch(ctx, &node, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return ctrl.Result{}, fmt.Errorf("request campaign Run %d: %w", sequence, err)
	}

	if err := r.patchCampaignStatus(
		ctx,
		&campaign,
		signalv1.CampaignPhaseRunning,
		"RunRequested",
		fmt.Sprintf("Requested Run %d", sequence),
	); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r *CampaignReconciler) requirePinnedProfileAndSuite(
	ctx context.Context,
	campaign *signalv1.CertificationCampaign,
) error {
	var profile signalv1.CertificationProfile
	if err := r.Get(ctx, client.ObjectKey{Name: campaign.Spec.Profile.Name}, &profile); err != nil {
		return fmt.Errorf("get pinned Profile: %w", err)
	}

	if profile.UID != campaign.Spec.Profile.UID ||
		profile.Generation != campaign.Spec.Profile.Generation ||
		profile.Spec.Suite.Name != campaign.Spec.Suite.Name {
		return errors.New("the campaign Profile no longer matches its pinned identity")
	}

	var suite signalv1.CertificationSuite
	if err := r.Get(ctx, client.ObjectKey{Name: campaign.Spec.Suite.Name}, &suite); err != nil {
		return fmt.Errorf("get pinned Suite: %w", err)
	}

	if suite.UID != campaign.Spec.Suite.UID ||
		suite.Generation != campaign.Spec.Suite.Generation {
		return errors.New("the campaign Suite no longer matches its pinned identity")
	}

	return nil
}

func (r *CampaignReconciler) currentRun(
	ctx context.Context,
	campaign *signalv1.CertificationCampaign,
) (*signalv1.CertificationRun, error) {
	var runs signalv1.CertificationRunList
	if err := r.List(
		ctx,
		&runs,
		client.InNamespace(campaign.Namespace),
		client.MatchingFields{campaignRunUIDField: string(campaign.UID)},
	); err != nil {
		return nil, fmt.Errorf("list campaign Runs: %w", err)
	}

	lastSequence := int32(0)
	if campaign.Status.LastRun != nil {
		lastSequence = campaign.Status.LastRun.Sequence
	}

	var current *signalv1.CertificationRun

	for index := range runs.Items {
		run := &runs.Items[index]
		if run.Spec.Campaign == nil || run.Spec.Campaign.Sequence <= lastSequence {
			continue
		}

		if current == nil || run.Spec.Campaign.Sequence < current.Spec.Campaign.Sequence {
			current = run
		}
	}

	return current, nil
}

func (r *CampaignReconciler) reconcileRun(
	ctx context.Context,
	campaign *signalv1.CertificationCampaign,
	run *signalv1.CertificationRun,
) (ctrl.Result, error) {
	reference := signalv1.CampaignRunReference{
		Name:     run.Name,
		UID:      run.UID,
		Sequence: run.Spec.Campaign.Sequence,
	}

	if !run.Status.Phase.Terminal() {
		before := campaign.DeepCopy()
		campaign.Status.ObservedGeneration = campaign.Generation
		campaign.Status.Phase = signalv1.CampaignPhaseRunning

		campaign.Status.CurrentRun = &reference
		if reference.Sequence > campaign.Status.StartedRuns {
			campaign.Status.StartedRuns = reference.Sequence
		}

		setCampaignCondition(campaign, metav1.ConditionFalse, "RunActive", "A campaign Run is active")

		if err := r.Status().Patch(ctx, campaign, client.MergeFrom(before)); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	if !apimeta.IsStatusConditionTrue(run.Status.Conditions, signalv1.RunConditionReleased) {
		if run.Status.Phase != signalv1.RunPhaseSucceeded && !run.Spec.Release {
			before := run.DeepCopy()

			run.Spec.Release = true
			if err := r.Patch(ctx, run, client.MergeFrom(before)); err != nil {
				return ctrl.Result{}, fmt.Errorf("release terminal campaign Run %s: %w", run.Name, err)
			}
		}

		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	before := campaign.DeepCopy()
	campaign.Status.ObservedGeneration = campaign.Generation
	campaign.Status.Phase = signalv1.CampaignPhaseRunning
	campaign.Status.CurrentRun = nil

	campaign.Status.LastRun = &reference
	if reference.Sequence > campaign.Status.StartedRuns {
		campaign.Status.StartedRuns = reference.Sequence
	}

	switch run.Status.Phase {
	case signalv1.RunPhaseSucceeded:
		campaign.Status.SucceededRuns++
	case signalv1.RunPhaseFailed:
		campaign.Status.FailedRuns++
	case signalv1.RunPhaseInconclusive:
		campaign.Status.InconclusiveRuns++
	case signalv1.RunPhaseCancelled:
		campaign.Status.CanceledRuns++
	}

	if run.Status.Phase == signalv1.RunPhaseFailed && campaign.Spec.StopOnFailure {
		campaign.Status.Phase = signalv1.CampaignPhaseCompleted
		setCampaignCondition(
			campaign,
			metav1.ConditionTrue,
			"RunFailed",
			"The campaign stopped after a failed Run",
		)

		if err := r.Status().Patch(ctx, campaign, client.MergeFrom(before)); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	setCampaignCondition(campaign, metav1.ConditionFalse, "RunComplete", "The latest campaign Run completed and released")

	if err := r.Status().Patch(ctx, campaign, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: time.Millisecond}, nil
}

func (r *CampaignReconciler) completeCampaign(
	ctx context.Context,
	campaign *signalv1.CertificationCampaign,
) (ctrl.Result, error) {
	reason := "DeadlineReached"
	message := "The campaign admission window ended"

	if campaign.Spec.Stop {
		reason = "Stopped"
		message = "The campaign was stopped"
	}

	if campaign.Status.Phase == signalv1.CampaignPhaseCompleted &&
		apimeta.IsStatusConditionTrue(campaign.Status.Conditions, campaignConditionReady) {
		return ctrl.Result{}, nil
	}

	before := campaign.DeepCopy()
	campaign.Status.ObservedGeneration = campaign.Generation
	campaign.Status.Phase = signalv1.CampaignPhaseCompleted
	campaign.Status.CurrentRun = nil
	setCampaignCondition(campaign, metav1.ConditionTrue, reason, message)

	if err := r.Status().Patch(ctx, campaign, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *CampaignReconciler) clearPendingRequest(
	ctx context.Context,
	node *corev1.Node,
	campaign *signalv1.CertificationCampaign,
) error {
	if node.Annotations[ReservationLabel] != "" ||
		node.Annotations[CampaignRequestUID] != string(campaign.UID) {
		return nil
	}

	before := node.DeepCopy()
	if node.Labels[RequestLabel] == campaign.Spec.Profile.Name {
		delete(node.Labels, RequestLabel)
	}

	delete(node.Annotations, CampaignRequestName)
	delete(node.Annotations, CampaignRequestUID)
	delete(node.Annotations, CampaignRequestSequence)

	if err := r.Patch(ctx, node, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("clear expired campaign request: %w", err)
	}

	return nil
}

func (r *CampaignReconciler) blockCampaign(
	ctx context.Context,
	campaign *signalv1.CertificationCampaign,
	reason string,
	message string,
) (ctrl.Result, error) {
	before := campaign.DeepCopy()
	campaign.Status.ObservedGeneration = campaign.Generation
	campaign.Status.Phase = signalv1.CampaignPhaseBlocked
	setCampaignCondition(campaign, metav1.ConditionFalse, reason, message)

	if err := r.Status().Patch(ctx, campaign, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *CampaignReconciler) patchCampaignStatus(
	ctx context.Context,
	campaign *signalv1.CertificationCampaign,
	phase signalv1.CampaignPhase,
	reason string,
	message string,
) error {
	before := campaign.DeepCopy()
	campaign.Status.ObservedGeneration = campaign.Generation
	campaign.Status.Phase = phase
	setCampaignCondition(campaign, metav1.ConditionFalse, reason, message)

	return r.Status().Patch(ctx, campaign, client.MergeFrom(before))
}

func (r *CampaignReconciler) requestForRun(
	_ context.Context,
	object client.Object,
) []ctrl.Request {
	run, ok := object.(*signalv1.CertificationRun)
	if !ok || run.Spec.Campaign == nil {
		return nil
	}

	return []ctrl.Request{{
		NamespacedName: client.ObjectKey{
			Namespace: run.Namespace,
			Name:      run.Spec.Campaign.Name,
		},
	}}
}

func (r *CampaignReconciler) requestsForNode(
	ctx context.Context,
	object client.Object,
) []ctrl.Request {
	node, ok := object.(*corev1.Node)
	if !ok {
		return nil
	}

	var campaigns signalv1.CertificationCampaignList
	if err := r.List(
		ctx,
		&campaigns,
		client.InNamespace(r.Namespace),
		client.MatchingFields{campaignNodeNameField: node.Name},
	); err != nil {
		log.FromContext(ctx).Error(err, "failed to list CertificationCampaigns for Node", "node", node.Name)

		return nil
	}

	requests := make([]ctrl.Request, 0, len(campaigns.Items))
	for index := range campaigns.Items {
		requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKey{
			Namespace: campaigns.Items[index].Namespace,
			Name:      campaigns.Items[index].Name,
		}})
	}

	return requests
}

func (r *CampaignReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}

	return time.Now().UTC()
}

func setCampaignCondition(
	campaign *signalv1.CertificationCampaign,
	status metav1.ConditionStatus,
	reason string,
	message string,
) {
	apimeta.SetStatusCondition(&campaign.Status.Conditions, metav1.Condition{
		Type:               campaignConditionReady,
		Status:             status,
		ObservedGeneration: campaign.Generation,
		Reason:             reason,
		Message:            message,
	})
}

func campaignName(policyUID, nodeUID types.UID) string {
	sum := sha256.Sum256([]byte(string(policyUID) + "\x00" + string(nodeUID)))

	return "campaign-" + hex.EncodeToString(sum[:10])
}

func campaignIdentityFromNode(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	node *corev1.Node,
	profile *signalv1.CertificationProfile,
	suite *signalv1.CertificationSuite,
	now time.Time,
) (*signalv1.CampaignRunIdentity, error) {
	name := node.Annotations[CampaignRequestName]
	uid := types.UID(node.Annotations[CampaignRequestUID])

	sequenceText := node.Annotations[CampaignRequestSequence]
	if name == "" && uid == "" && sequenceText == "" {
		return nil, nil
	}

	if name == "" || uid == "" || sequenceText == "" {
		return nil, errors.New("campaign request metadata is incomplete")
	}

	sequence, err := strconv.ParseInt(sequenceText, 10, 32)
	if err != nil || sequence < 1 {
		return nil, errors.New("campaign request sequence is invalid")
	}

	var campaign signalv1.CertificationCampaign
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &campaign); err != nil {
		return nil, fmt.Errorf("get requesting CertificationCampaign: %w", err)
	}

	if campaign.UID != uid ||
		campaign.Spec.NodeName != node.Name ||
		campaign.Spec.NodeUID != node.UID ||
		campaign.Spec.Profile.Name != node.Labels[RequestLabel] ||
		profile.Name != campaign.Spec.Profile.Name ||
		profile.UID != campaign.Spec.Profile.UID ||
		profile.Generation != campaign.Spec.Profile.Generation ||
		suite.Name != campaign.Spec.Suite.Name ||
		suite.UID != campaign.Spec.Suite.UID ||
		suite.Generation != campaign.Spec.Suite.Generation {
		return nil, errors.New("campaign request metadata does not match the live Campaign")
	}

	if campaign.Spec.Stop || !now.Before(campaign.Spec.Deadline.Time) {
		return nil, errors.New("campaign admission window has ended")
	}

	if int32(sequence) != campaign.Status.StartedRuns+1 {
		return nil, fmt.Errorf(
			"campaign request sequence %d does not follow started Run count %d",
			sequence,
			campaign.Status.StartedRuns,
		)
	}

	return &signalv1.CampaignRunIdentity{
		Name:     campaign.Name,
		UID:      campaign.UID,
		Sequence: int32(sequence),
	}, nil
}
