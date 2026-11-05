// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

const (
	qualificationPolicyUIDLabel  = "signal.unbounded-cloud.io/policy-uid"
	qualificationNodeUIDLabel    = "signal.unbounded-cloud.io/node-uid"
	qualificationPolicyFinalizer = "signal.unbounded-cloud.io/qualification-ledgers"
)

type QualificationReconciler struct {
	client.Client
	APIReader     client.Reader
	Evidence      EvidenceStore
	Namespace     string
	GateProtected bool
	Now           func() time.Time
}

// +kubebuilder:rbac:groups=signal.unbounded-cloud.io,resources=certificationpolicies,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=signal.unbounded-cloud.io,resources=certificationpolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups=signal.unbounded-cloud.io,resources=nodecertifications,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=signal.unbounded-cloud.io,resources=nodecertifications/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=signal.unbounded-cloud.io,resources=certificationruns;certificationprofiles;certificationsuites,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch

type qualificationRecord struct {
	Reference  signalv1.ContributingRunReference
	Profile    signalv1.PinnedProfile
	Suite      signalv1.PinnedSuite
	Identity   signalv1.QualificationEvidenceIdentity
	Phase      signalv1.RunPhase
	StartedAt  time.Time
	FinishedAt time.Time
}

type qualificationRunIndex map[types.UID][]qualificationRecord

type qualificationRunIndexContextKey struct{}

func (r *QualificationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil || r.APIReader == nil || r.Evidence == nil {
		return errors.New("qualification reconciler requires client, API reader, and evidence store")
	}

	if r.Namespace == "" {
		return errors.New("qualification namespace is required")
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&signalv1.CertificationPolicy{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.requestsForAllPolicies)).
		Watches(&signalv1.CertificationRun{}, handler.EnqueueRequestsFromMapFunc(r.requestsForAllPolicies)).
		Watches(&signalv1.NodeCertification{}, handler.EnqueueRequestsFromMapFunc(r.requestsForAllPolicies)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

func (r *QualificationReconciler) requestsForAllPolicies(
	ctx context.Context,
	_ client.Object,
) []ctrl.Request {
	var policies signalv1.CertificationPolicyList
	if err := r.List(ctx, &policies); err != nil {
		return nil
	}

	requests := make([]ctrl.Request, len(policies.Items))
	for index := range policies.Items {
		requests[index] = ctrl.Request{NamespacedName: types.NamespacedName{Name: policies.Items[index].Name}}
	}

	return requests
}

func (r *QualificationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var policy signalv1.CertificationPolicy
	if err := r.Get(ctx, client.ObjectKey{Name: req.Name}, &policy); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.reconcileAllQualificationLabels(ctx)
		}

		return ctrl.Result{}, err
	}

	if !policy.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalizePolicy(ctx, &policy)
	}

	if !slices.Contains(policy.Finalizers, qualificationPolicyFinalizer) {
		before := policy.DeepCopy()

		policy.Finalizers = append(policy.Finalizers, qualificationPolicyFinalizer)
		if err := r.Patch(ctx, &policy, client.MergeFrom(before)); err != nil {
			return ctrl.Result{}, err
		}
	}

	var nodes corev1.NodeList
	if err := r.APIReader.List(ctx, &nodes); err != nil {
		return ctrl.Result{}, err
	}

	runIndex, err := r.buildQualificationRunIndex(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	ctx = context.WithValue(ctx, qualificationRunIndexContextKey{}, runIndex)

	var reconcileErrors []error
	for index := range nodes.Items {
		if err := r.reconcileNodeLedger(ctx, &policy, &nodes.Items[index]); err != nil {
			reconcileErrors = append(reconcileErrors, err)
		}
	}

	if err := r.markGoneLedgers(ctx, &policy, nodes.Items); err != nil {
		reconcileErrors = append(reconcileErrors, err)
	}

	for index := range nodes.Items {
		if err := r.reconcileQualificationLabel(ctx, &nodes.Items[index]); err != nil {
			reconcileErrors = append(reconcileErrors, err)
		}
	}

	return ctrl.Result{RequeueAfter: time.Minute}, errors.Join(reconcileErrors...)
}

func (r *QualificationReconciler) finalizePolicy(
	ctx context.Context,
	policy *signalv1.CertificationPolicy,
) error {
	var ledgers signalv1.NodeCertificationList
	if err := r.List(
		ctx,
		&ledgers,
		client.InNamespace(r.Namespace),
		client.MatchingLabels{qualificationPolicyUIDLabel: string(policy.UID)},
	); err != nil {
		return err
	}

	for index := range ledgers.Items {
		if err := r.Delete(ctx, &ledgers.Items[index]); client.IgnoreNotFound(err) != nil {
			return err
		}
	}

	if err := r.reconcileAllQualificationLabels(ctx); err != nil {
		return err
	}

	if slices.Contains(policy.Finalizers, qualificationPolicyFinalizer) {
		before := policy.DeepCopy()

		policy.Finalizers = slices.DeleteFunc(policy.Finalizers, func(value string) bool {
			return value == qualificationPolicyFinalizer
		})
		if err := r.Patch(ctx, policy, client.MergeFrom(before)); err != nil {
			return err
		}
	}

	return nil
}

func (r *QualificationReconciler) reconcileNodeLedger(
	ctx context.Context,
	policy *signalv1.CertificationPolicy,
	node *corev1.Node,
) error {
	name := nodeCertificationName(policy.UID, node.UID)
	key := client.ObjectKey{Namespace: r.Namespace, Name: name}

	var ledger signalv1.NodeCertification
	if err := r.Get(ctx, key, &ledger); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}

		ledger = signalv1.NodeCertification{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: r.Namespace,
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
		if err := r.Create(ctx, &ledger); err != nil {
			return err
		}
	}

	if policy.Generation > ledger.Spec.Policy.Generation {
		before := ledger.DeepCopy()

		ledger.Spec.Policy.Generation = policy.Generation
		if err := r.Patch(ctx, &ledger, client.MergeFrom(before)); err != nil {
			return err
		}
	}

	status, err := r.evaluateLedger(ctx, policy, node, &ledger)

	before := ledger.DeepCopy()
	ledger.Status = status
	if reflect.DeepEqual(before.Status, ledger.Status) {
		return err
	}

	if patchErr := r.Status().Patch(ctx, &ledger, client.MergeFrom(before)); patchErr != nil {
		return errors.Join(err, patchErr)
	}

	return err
}

func (r *QualificationReconciler) evaluateLedger(
	ctx context.Context,
	policy *signalv1.CertificationPolicy,
	node *corev1.Node,
	ledger *signalv1.NodeCertification,
) (signalv1.NodeCertificationStatus, error) {
	status := ledger.Status
	status.ObservedGeneration = policy.Generation

	digest, err := policyDigest(policy.Spec)
	if err != nil {
		return status, err
	}

	status.PolicyDigest = digest

	if node.UID != ledger.Spec.NodeUID {
		status.Phase = signalv1.NodeCertificationPhaseNodeGone
		status.Reason = "Node UID no longer matches the ledger"

		return status, nil
	}

	records, err := r.qualificationRecords(ctx, ledger, node.UID)
	if err != nil {
		apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type:               "EvidenceHealthy",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: policy.Generation,
			Reason:             "EvidenceUnavailable",
			Message:            boundedStatusText(err.Error()),
		})
	} else {
		apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type:               "EvidenceHealthy",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: policy.Generation,
			Reason:             "Verified",
			Message:            "All retained evidence references were verified",
		})
	}
	evidenceErr := err

	var liveNonPass *signalv1.LatestNonPassWatermark

	var watermarkErr error
	if policy.Spec.NonPassBehavior != signalv1.NonPassBehaviorIgnore {
		liveNonPass, watermarkErr = latestNonPassWatermark(policy, records)
	}
	evidenceErr = errors.Join(evidenceErr, watermarkErr)
	if watermarkErr != nil {
		clearQualificationStatus(&status)
		status.Phase = signalv1.NodeCertificationPhasePending
		status.Reason = "Latest non-pass evidence could not be recorded"

		return status, evidenceErr
	}

	if watermarkAfter(liveNonPass, status.LatestNonPass) {
		status.LatestNonPass = liveNonPass
		clearQualificationStatus(&status)
	}

	if status.LatestNonPass != nil &&
		policy.Spec.NonPassBehavior != signalv1.NonPassBehaviorIgnore {
		records = slices.DeleteFunc(records, func(record qualificationRecord) bool {
			return !record.FinishedAt.After(status.LatestNonPass.FinishedAt.Time)
		})
	}

	contributions := make([]qualificationRecord, 0)

	for requirementIndex := range policy.Spec.Requirements {
		requirement := &policy.Spec.Requirements[requirementIndex]

		accepted, err := r.acceptedRecords(
			ctx,
			requirement,
			node,
			policy.Spec.InvalidationFingerprintFields,
			records,
		)
		if err != nil {
			return status, errors.Join(evidenceErr, err)
		}

		if !requirementSatisfied(requirement, accepted) {
			switch {
			case policy.Spec.NonPassBehavior == signalv1.NonPassBehaviorRevoke &&
				status.LatestNonPass != nil:
				status.Phase = signalv1.NodeCertificationPhaseRevoked
			case len(contributions) == 0:
				status.Phase = signalv1.NodeCertificationPhasePending
			default:
				status.Phase = signalv1.NodeCertificationPhaseQualifying
			}

			status.Reason = fmt.Sprintf("Requirement %s is not yet satisfied", requirement.Name)
			if status.Phase == signalv1.NodeCertificationPhaseRevoked {
				status.Reason = fmt.Sprintf(
					"Requirement %s is not yet satisfied after the latest non-pass Run",
					requirement.Name,
				)
			}
			status.ContributingRuns = recordReferences(contributions)
			status.PlanDigests = recordPlanDigests(contributions)
			status.QualifiedAt = nil
			status.ExpiresAt = nil

			return status, evidenceErr
		}

		contributions = append(contributions, accepted...)
	}

	contributions = uniqueRecords(contributions)

	fingerprint, ok := qualificationFingerprint(policy.Spec.InvalidationFingerprintFields, node, contributions)
	if !ok {
		status.Phase = signalv1.NodeCertificationPhaseQualifying
		status.Reason = "Required fingerprint evidence is missing or changed"
		status.QualifiedAt = nil
		status.ExpiresAt = nil

		return status, evidenceErr
	}

	qualifiedAt := latestRecordTime(contributions)
	expiresAt := qualifiedAt.Add(policy.Spec.Validity.Duration)
	now := r.now()
	status.ContributingRuns = recordReferences(contributions)
	status.PlanDigests = recordPlanDigests(contributions)
	status.Fingerprint = fingerprint
	status.QualifiedAt = &metav1.Time{Time: qualifiedAt}
	status.ExpiresAt = &metav1.Time{Time: expiresAt}

	if !now.Before(expiresAt) {
		status.Phase = signalv1.NodeCertificationPhaseExpired
		status.Reason = "Qualification evidence expired"
		apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type:               "RenewalDue",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: policy.Generation,
			Reason:             "Expired",
			Message:            "Qualification evidence has expired",
		})

		return status, evidenceErr
	}

	status.Phase = signalv1.NodeCertificationPhaseQualified
	status.Reason = "All policy requirements are satisfied"
	renewalDue := now.Add(policy.Spec.RenewBefore.Duration).After(expiresAt) ||
		now.Add(policy.Spec.RenewBefore.Duration).Equal(expiresAt)
	conditionStatus := metav1.ConditionFalse
	conditionReason := "Current"
	conditionMessage := "Qualification is outside its renewal window"

	if renewalDue {
		conditionStatus = metav1.ConditionTrue
		conditionReason = "RenewalWindow"
		conditionMessage = "Qualification is inside its renewal window"
	}

	apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               "RenewalDue",
		Status:             conditionStatus,
		ObservedGeneration: policy.Generation,
		Reason:             conditionReason,
		Message:            conditionMessage,
	})

	return status, evidenceErr
}

func latestNonPassWatermark(
	policy *signalv1.CertificationPolicy,
	records []qualificationRecord,
) (*signalv1.LatestNonPassWatermark, error) {
	var latest *signalv1.LatestNonPassWatermark

	for index := range records {
		record := records[index]
		if !recordMatchesPolicy(policy, record) || record.Phase == signalv1.RunPhaseSucceeded {
			continue
		}

		classification, ok := nonPassClassification(record.Phase)
		if !ok {
			continue
		}
		if !signalv1.IsSHA256Digest(record.Reference.EvidenceDigest) {
			return nil, fmt.Errorf(
				"non-pass Run %s has no valid evidence digest",
				record.Reference.Name,
			)
		}

		candidate := &signalv1.LatestNonPassWatermark{
			Name:           record.Reference.Name,
			UID:            record.Reference.UID,
			FinishedAt:     metav1.NewTime(record.FinishedAt),
			Classification: classification,
			EvidenceDigest: record.Reference.EvidenceDigest,
		}
		if watermarkAfter(candidate, latest) {
			latest = candidate
		}
	}

	return latest, nil
}

func nonPassClassification(phase signalv1.RunPhase) (signalv1.NonPassClassification, bool) {
	switch phase {
	case signalv1.RunPhaseFailed:
		return signalv1.NonPassClassificationFailed, true
	case signalv1.RunPhaseInconclusive:
		return signalv1.NonPassClassificationInconclusive, true
	case signalv1.RunPhaseCancelled:
		return signalv1.NonPassClassificationCanceled, true
	default:
		return "", false
	}
}

func watermarkAfter(
	candidate *signalv1.LatestNonPassWatermark,
	current *signalv1.LatestNonPassWatermark,
) bool {
	if candidate == nil {
		return false
	}
	if current == nil {
		return true
	}
	if candidate.FinishedAt.After(current.FinishedAt.Time) {
		return true
	}
	if !candidate.FinishedAt.Equal(&current.FinishedAt) {
		return false
	}

	return string(candidate.UID) > string(current.UID)
}

func clearQualificationStatus(status *signalv1.NodeCertificationStatus) {
	status.PlanDigests = nil
	status.Fingerprint = nil
	status.QualifiedAt = nil
	status.ExpiresAt = nil
	status.ContributingRuns = nil
}

func (r *QualificationReconciler) qualificationRecords(
	ctx context.Context,
	ledger *signalv1.NodeCertification,
	nodeUID types.UID,
) ([]qualificationRecord, error) {
	records := make([]qualificationRecord, 0, len(ledger.Status.ContributingRuns))
	seen := map[types.UID]struct{}{}
	var evidenceErrors []error

	for index := range ledger.Status.ContributingRuns {
		reference := ledger.Status.ContributingRuns[index]
		seen[reference.UID] = struct{}{}

		record, found, err := r.recordFromEvidence(ctx, reference)
		if err != nil {
			evidenceErrors = append(evidenceErrors, fmt.Errorf(
				"verify retained Run %s evidence: %w",
				reference.Name,
				err,
			))

			continue
		}

		if !found || !recordIncludesNode(record, nodeUID) {
			continue
		}
		records = append(records, record)
		records = append(records, record)
	}

	runIndex, ok := ctx.Value(qualificationRunIndexContextKey{}).(qualificationRunIndex)
	if !ok {
		var err error
		runIndex, err = r.buildQualificationRunIndex(ctx)
		if err != nil {
			evidenceErrors = append(evidenceErrors, err)
		}
	}

	for index := range runIndex[nodeUID] {
		record := runIndex[nodeUID][index]
		if _, exists := seen[record.Reference.UID]; exists {
			continue
		}

		records = append(records, record)
	}

	sort.Slice(records, func(i, j int) bool {
		return records[i].FinishedAt.Before(records[j].FinishedAt)
	})

	return records, errors.Join(evidenceErrors...)
}

func (r *QualificationReconciler) buildQualificationRunIndex(
	ctx context.Context,
) (qualificationRunIndex, error) {
	var runs signalv1.CertificationRunList
	if err := r.List(ctx, &runs, client.InNamespace(r.Namespace)); err != nil {
		return nil, err
	}

	index := make(qualificationRunIndex)
	for runIndex := range runs.Items {
		run := &runs.Items[runIndex]
		if !run.Status.Phase.Terminal() || run.Status.Evidence.Identity == nil ||
			run.Spec.PlanDigest == "" || run.Spec.PlanProvenance == nil {
			continue
		}

		canonical, err := signalv1.DeriveExecutionPlanDigest(
			int32(len(run.Spec.Participants)),
			run.Spec.ScriptBundle.Digest,
			run.Spec.Checks,
			*run.Spec.PlanProvenance,
		)
		if err != nil || canonical != run.Spec.PlanDigest ||
			run.Status.Evidence.Identity.PlanDigest != run.Spec.PlanDigest ||
			!qualificationIdentityProvenanceValid(run.Status.Evidence.Identity) {
			continue
		}

		record := qualificationRecord{
			Reference: signalv1.ContributingRunReference{
				Name:           run.Name,
				UID:            run.UID,
				PlanDigest:     run.Spec.PlanDigest,
				EvidenceURI:    run.Status.Evidence.URI,
				EvidenceDigest: run.Status.Evidence.Digest,
			},
			Profile:    run.Spec.Profile,
			Suite:      run.Spec.Suite,
			Identity:   *run.Status.Evidence.Identity.DeepCopy(),
			Phase:      run.Status.Phase,
			StartedAt:  identityStartedAt(run.Status.Evidence.Identity),
			FinishedAt: identityFinishedAt(run.Status.Evidence.Identity),
		}
		for participantIndex := range run.Status.Evidence.Identity.Participants {
			nodeUID := run.Status.Evidence.Identity.Participants[participantIndex].End.NodeUID
			if nodeUID == "" || !recordIncludesNode(record, nodeUID) {
				continue
			}

			index[nodeUID] = append(index[nodeUID], record)
		}
	}

	for nodeUID := range index {
		sort.Slice(index[nodeUID], func(i, j int) bool {
			return index[nodeUID][i].FinishedAt.Before(index[nodeUID][j].FinishedAt)
		})
	}

	return index, nil
}

func (r *QualificationReconciler) recordFromEvidence(
	ctx context.Context,
	reference signalv1.ContributingRunReference,
) (qualificationRecord, bool, error) {
	data, _, digest, found, err := r.Evidence.Get(ctx, reference.Name+".json")
	if err != nil {
		return qualificationRecord{}, false, err
	}
	if !found {
		return qualificationRecord{}, false, errors.New("evidence document is missing")
	}

	if digest != reference.EvidenceDigest {
		return qualificationRecord{}, false, errors.New("evidence digest does not match the retained reference")
	}

	var document struct {
		RunID                 string                                  `json:"runID"`
		Profile               signalv1.PinnedProfile                  `json:"profile"`
		Suite                 signalv1.PinnedSuite                    `json:"suite"`
		Participants          []signalv1.Participant                  `json:"participants"`
		ScriptBundle          signalv1.ScriptBundleReference          `json:"scriptBundle"`
		Checks                []signalv1.CertificationCheckSnapshot   `json:"checks"`
		Phase                 signalv1.RunPhase                       `json:"phase"`
		FinishedAt            string                                  `json:"finishedAt"`
		StartedAt             string                                  `json:"startedAt"`
		QualificationIdentity *signalv1.QualificationEvidenceIdentity `json:"qualificationIdentity"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return qualificationRecord{}, false, err
	}

	if document.RunID != string(reference.UID) || document.QualificationIdentity == nil ||
		document.QualificationIdentity.PlanDigest != reference.PlanDigest {
		return qualificationRecord{}, false, errors.New("evidence identity does not match the retained reference")
	}

	provenance := signalv1.ExecutionPlanProvenance{
		AdapterImage:           document.QualificationIdentity.Provenance.AdapterImage,
		WorkflowTemplateDigest: document.QualificationIdentity.Provenance.WorkflowTemplateDigest,
	}

	canonical, err := signalv1.DeriveExecutionPlanDigest(
		int32(len(document.Participants)),
		document.ScriptBundle.Digest,
		document.Checks,
		provenance,
	)
	if err != nil {
		return qualificationRecord{}, false, fmt.Errorf("derive evidence plan digest: %w", err)
	}
	if canonical != reference.PlanDigest {
		return qualificationRecord{}, false, errors.New("evidence plan digest is not canonical")
	}
	if !qualificationIdentityProvenanceValid(document.QualificationIdentity) {
		return qualificationRecord{}, false, errors.New("evidence provenance is incomplete")
	}

	finishedAt, err := time.Parse(time.RFC3339Nano, document.FinishedAt)
	if err != nil {
		return qualificationRecord{}, false, err
	}

	startedAt, err := time.Parse(time.RFC3339Nano, document.StartedAt)
	if err != nil {
		return qualificationRecord{}, false, err
	}

	return qualificationRecord{
		Reference:  reference,
		Profile:    document.Profile,
		Suite:      document.Suite,
		Identity:   *document.QualificationIdentity,
		Phase:      document.Phase,
		StartedAt:  startedAt,
		FinishedAt: finishedAt,
	}, true, nil
}

func qualificationIdentityProvenanceValid(identity *signalv1.QualificationEvidenceIdentity) bool {
	if identity == nil ||
		!signalv1.IsDigestPinnedImage(identity.Provenance.ControllerImage) ||
		!signalv1.IsDigestPinnedImage(identity.Provenance.AdapterImage) ||
		!signalv1.IsSHA256Digest(identity.Provenance.WorkflowTemplateDigest) ||
		len(identity.Provenance.RunnerImages) == 0 {
		return false
	}

	for _, image := range identity.Provenance.RunnerImages {
		if !signalv1.IsDigestPinnedImage(image) {
			return false
		}
	}

	return true
}

func (r *QualificationReconciler) acceptedRecords(
	ctx context.Context,
	requirement *signalv1.CertificationRequirement,
	node *corev1.Node,
	fingerprintFields []signalv1.FingerprintField,
	records []qualificationRecord,
) ([]qualificationRecord, error) {
	var currentProfile signalv1.CertificationProfile

	var currentSuite signalv1.CertificationSuite

	if requirement.AcceptedPlans.Mode == signalv1.AcceptedPlanModeCurrent {
		if err := r.APIReader.Get(ctx, client.ObjectKey{Name: requirement.Profile.Name}, &currentProfile); err != nil {
			return nil, client.IgnoreNotFound(err)
		}

		if err := r.APIReader.Get(ctx, client.ObjectKey{Name: currentProfile.Spec.Suite.Name}, &currentSuite); err != nil {
			return nil, client.IgnoreNotFound(err)
		}
	}

	accepted := make([]qualificationRecord, 0)

	for index := range records {
		record := records[index]
		if record.Profile.Name != requirement.Profile.Name ||
			(requirement.Profile.UID != "" && record.Profile.UID != requirement.Profile.UID) {
			continue
		}

		if requirement.AcceptedPlans.Mode == signalv1.AcceptedPlanModeListed &&
			!slices.Contains(requirement.AcceptedPlans.Digests, record.Identity.PlanDigest) {
			continue
		}

		if requirement.AcceptedPlans.Mode == signalv1.AcceptedPlanModeCurrent &&
			(record.Profile.UID != currentProfile.UID ||
				record.Profile.Generation != currentProfile.Generation ||
				record.Suite.UID != currentSuite.UID ||
				record.Suite.Generation != currentSuite.Generation) {
			continue
		}

		participant, found := recordParticipant(record, node.UID)
		if !found || participant.Verdict != signalv1.QualificationVerdictPass {
			continue
		}
		if !participantFingerprintMatches(
			fingerprintFields,
			participant,
			nodeFingerprint(node),
		) {
			continue
		}

		switch requirement.CreditRule {
		case signalv1.QualificationCreditRequester:
			if participant.Role != signalv1.ParticipantRoleRequester {
				continue
			}
		case signalv1.QualificationCreditAllParticipants:
			if slices.ContainsFunc(record.Identity.Participants, func(value signalv1.ParticipantEvidenceIdentity) bool {
				return value.Verdict != signalv1.QualificationVerdictPass
			}) {
				continue
			}
		case signalv1.QualificationCreditPerParticipant:
		default:
			continue
		}

		accepted = append(accepted, record)
	}

	return accepted, nil
}

func participantFingerprintMatches(
	fields []signalv1.FingerprintField,
	participant signalv1.ParticipantEvidenceIdentity,
	current signalv1.NodeFingerprint,
) bool {
	for _, field := range fields {
		if !fingerprintFieldMatches(field, participant.Start, participant.End) ||
			!fingerprintFieldMatches(field, participant.End, current) {
			return false
		}
	}

	return true
}

func requirementSatisfied(
	requirement *signalv1.CertificationRequirement,
	records []qualificationRecord,
) bool {
	if len(records) < int(requirement.RequiredPassCount) {
		return false
	}

	if requirement.Variation == nil {
		return true
	}

	if requirement.Variation.MinDistinctPlanDigests > 0 &&
		len(recordPlanDigests(records)) < int(requirement.Variation.MinDistinctPlanDigests) {
		return false
	}

	if requirement.Variation.MinRunSeparation != nil {
		separated := 0

		var previous time.Time
		for index := range records {
			if !previous.IsZero() &&
				records[index].StartedAt.Sub(previous) < requirement.Variation.MinRunSeparation.Duration {
				continue
			}

			previous = records[index].StartedAt
			separated++
		}

		if separated < int(requirement.RequiredPassCount) {
			return false
		}
	}

	return true
}

func recordMatchesPolicy(policy *signalv1.CertificationPolicy, record qualificationRecord) bool {
	for index := range policy.Spec.Requirements {
		requirement := &policy.Spec.Requirements[index]
		if record.Profile.Name == requirement.Profile.Name &&
			(requirement.Profile.UID == "" || record.Profile.UID == requirement.Profile.UID) {
			return true
		}
	}

	return false
}

func qualificationFingerprint(
	fields []signalv1.FingerprintField,
	node *corev1.Node,
	records []qualificationRecord,
) (*signalv1.NodeFingerprint, bool) {
	if len(records) == 0 {
		return nil, false
	}

	latest := records[len(records)-1]

	participant, found := recordParticipant(latest, node.UID)
	if !found {
		return nil, false
	}

	current := nodeFingerprint(node)
	for _, field := range fields {
		if !fingerprintFieldMatches(field, participant.Start, participant.End) ||
			!fingerprintFieldMatches(field, participant.End, current) {
			return nil, false
		}

		for index := range records {
			contributor, found := recordParticipant(records[index], node.UID)
			if !found ||
				!fingerprintFieldMatches(field, contributor.Start, contributor.End) ||
				!fingerprintFieldMatches(field, contributor.End, participant.End) {
				return nil, false
			}
		}
	}

	value := participant.End

	return &value, true
}

func fingerprintFieldMatches(
	field signalv1.FingerprintField,
	evidence signalv1.NodeFingerprint,
	current signalv1.NodeFingerprint,
) bool {
	switch field {
	case signalv1.FingerprintFieldNodeUID:
		return evidence.NodeUID != "" && evidence.NodeUID == current.NodeUID
	case signalv1.FingerprintFieldBootID:
		return evidence.BootID != "" && evidence.BootID == current.BootID
	case signalv1.FingerprintFieldKernel:
		return evidence.KernelVersion != "" && evidence.KernelVersion == current.KernelVersion
	case signalv1.FingerprintFieldOSImage:
		return evidence.OSImage != "" && evidence.OSImage == current.OSImage
	case signalv1.FingerprintFieldKubelet:
		return evidence.KubeletVersion != "" && evidence.KubeletVersion == current.KubeletVersion
	case signalv1.FingerprintFieldSystemUUID:
		return evidence.SystemUUID != "" && evidence.SystemUUID == current.SystemUUID
	case signalv1.FingerprintFieldDriver:
		return evidence.DriverVersion != "" && evidence.DriverVersion == current.DriverVersion
	case signalv1.FingerprintFieldGPUInventory:
		return evidence.GPUInventoryDigest != "" &&
			evidence.GPUInventoryDigest == current.GPUInventoryDigest
	default:
		return false
	}
}

func (r *QualificationReconciler) markGoneLedgers(
	ctx context.Context,
	policy *signalv1.CertificationPolicy,
	nodes []corev1.Node,
) error {
	live := make(map[types.UID]struct{}, len(nodes))
	for index := range nodes {
		live[nodes[index].UID] = struct{}{}
	}

	var ledgers signalv1.NodeCertificationList
	if err := r.List(
		ctx,
		&ledgers,
		client.InNamespace(r.Namespace),
		client.MatchingLabels{qualificationPolicyUIDLabel: string(policy.UID)},
	); err != nil {
		return err
	}

	for index := range ledgers.Items {
		ledger := &ledgers.Items[index]
		if _, exists := live[ledger.Spec.NodeUID]; exists {
			continue
		}

		if ledger.Status.Phase == signalv1.NodeCertificationPhaseNodeGone {
			if err := r.Delete(ctx, ledger); client.IgnoreNotFound(err) != nil {
				return err
			}

			continue
		}

		before := ledger.DeepCopy()
		ledger.Status.Phase = signalv1.NodeCertificationPhaseNodeGone

		ledger.Status.Reason = "The pinned Node UID no longer exists"
		if err := r.Status().Patch(ctx, ledger, client.MergeFrom(before)); err != nil {
			return err
		}
	}

	return nil
}

func (r *QualificationReconciler) reconcileAllQualificationLabels(ctx context.Context) error {
	var nodes corev1.NodeList
	if err := r.APIReader.List(ctx, &nodes); err != nil {
		return err
	}

	for index := range nodes.Items {
		if err := r.reconcileQualificationLabel(ctx, &nodes.Items[index]); err != nil {
			return err
		}
	}

	return nil
}

func (r *QualificationReconciler) reconcileQualificationLabel(
	ctx context.Context,
	node *corev1.Node,
) error {
	var policies signalv1.CertificationPolicyList
	if err := r.APIReader.List(ctx, &policies); err != nil {
		return err
	}

	var ledgers signalv1.NodeCertificationList
	if err := r.List(
		ctx,
		&ledgers,
		client.InNamespace(r.Namespace),
		client.MatchingLabels{qualificationNodeUIDLabel: string(node.UID)},
	); err != nil {
		return err
	}

	protected := make([]signalv1.CertificationPolicy, 0)

	for index := range policies.Items {
		policy := &policies.Items[index]
		if policy.DeletionTimestamp.IsZero() &&
			policy.Spec.GateProjection.Mode == signalv1.GateProjectionProtectedLabel {
			protected = append(protected, *policy)
		}
	}

	shouldProject := r.GateProtected && len(protected) > 0
	for index := range protected {
		if !r.policyHasQualifiedLedger(node, &protected[index], ledgers.Items) {
			shouldProject = false
			break
		}
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current corev1.Node
		if err := r.Get(ctx, client.ObjectKey{Name: node.Name}, &current); err != nil {
			return client.IgnoreNotFound(err)
		}

		if current.UID != node.UID {
			return nil
		}

		before := current.DeepCopy()
		if current.Labels == nil {
			current.Labels = map[string]string{}
		}

		if current.Labels[EligibilityLabel] == "production" {
			delete(current.Labels, EligibilityLabel)
		}

		if shouldProject {
			current.Labels[signalv1.NodeQualificationLabel] = signalv1.NodeQualificationValue
		} else {
			delete(current.Labels, signalv1.NodeQualificationLabel)
		}

		if reflect.DeepEqual(before.Labels, current.Labels) {
			return nil
		}

		return r.Patch(ctx, &current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	}); err != nil {
		return err
	}

	return r.projectGateConditions(ctx, protected, ledgers.Items, shouldProject)
}

func (r *QualificationReconciler) policyHasQualifiedLedger(
	node *corev1.Node,
	policy *signalv1.CertificationPolicy,
	ledgers []signalv1.NodeCertification,
) bool {
	digest, err := policyDigest(policy.Spec)
	if err != nil {
		return false
	}

	for index := range ledgers {
		ledger := &ledgers[index]
		if ledger.DeletionTimestamp.IsZero() &&
			ledger.Spec.NodeUID == node.UID &&
			ledger.Spec.Policy.UID == policy.UID &&
			ledger.Spec.Policy.Generation == policy.Generation &&
			ledger.Status.ObservedGeneration == policy.Generation &&
			ledger.Status.PolicyDigest == digest &&
			ledger.Status.Phase == signalv1.NodeCertificationPhaseQualified &&
			ledger.Status.ExpiresAt != nil &&
			r.now().Before(ledger.Status.ExpiresAt.Time) &&
			ledger.Status.Fingerprint != nil &&
			ledger.Status.Fingerprint.NodeUID == node.UID {
			return true
		}
	}

	return false
}

func (r *QualificationReconciler) projectGateConditions(
	ctx context.Context,
	policies []signalv1.CertificationPolicy,
	ledgers []signalv1.NodeCertification,
	projected bool,
) error {
	policyUIDs := make(map[types.UID]struct{}, len(policies))
	for index := range policies {
		policyUIDs[policies[index].UID] = struct{}{}
	}

	for index := range ledgers {
		ledger := &ledgers[index]
		if _, exists := policyUIDs[ledger.Spec.Policy.UID]; !exists {
			continue
		}

		status := metav1.ConditionFalse
		reason := "PoliciesNotQualified"
		message := "The shared protected label is not backed by every ProtectedLabel Policy"

		if !r.GateProtected {
			reason = signalv1.NodeCertificationReasonProtectionUnverified
			message = "Node authorizer and NodeRestriction protection are not explicitly verified"
		} else if projected {
			status = metav1.ConditionTrue
			reason = "Projected"
			message = "The shared protected label is projected"
		}

		before := ledger.DeepCopy()
		apimeta.SetStatusCondition(&ledger.Status.Conditions, metav1.Condition{
			Type:               signalv1.NodeCertificationConditionGateProjected,
			Status:             status,
			ObservedGeneration: ledger.Status.ObservedGeneration,
			Reason:             reason,
			Message:            message,
		})

		if err := r.Status().Patch(ctx, ledger, client.MergeFrom(before)); err != nil {
			return err
		}
	}

	return nil
}

func (r *QualificationReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}

	return time.Now().UTC()
}

func nodeCertificationName(policyUID, nodeUID types.UID) string {
	sum := sha256.Sum256([]byte(string(policyUID) + "\x00" + string(nodeUID)))

	return "nodecert-" + hex.EncodeToString(sum[:16])
}

func policyDigest(spec signalv1.CertificationPolicySpec) (string, error) {
	data, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("marshal CertificationPolicy spec: %w", err)
	}

	return digestBytes(data), nil
}

func recordIncludesNode(record qualificationRecord, nodeUID types.UID) bool {
	_, found := recordParticipant(record, nodeUID)

	return found
}

func recordParticipant(
	record qualificationRecord,
	nodeUID types.UID,
) (signalv1.ParticipantEvidenceIdentity, bool) {
	for index := range record.Identity.Participants {
		participant := record.Identity.Participants[index]
		if participant.End.NodeUID == nodeUID && participant.Start.NodeUID == nodeUID {
			return participant, true
		}
	}

	return signalv1.ParticipantEvidenceIdentity{}, false
}

func identityFinishedAt(identity *signalv1.QualificationEvidenceIdentity) time.Time {
	var finished time.Time
	for index := range identity.Checks {
		if identity.Checks[index].FinishedAt.After(finished) {
			finished = identity.Checks[index].FinishedAt.Time
		}
	}

	return finished
}

func identityStartedAt(identity *signalv1.QualificationEvidenceIdentity) time.Time {
	var started time.Time

	for index := range identity.Checks {
		value := identity.Checks[index].StartedAt.Time
		if started.IsZero() || value.Before(started) {
			started = value
		}
	}

	return started
}

func uniqueRecords(records []qualificationRecord) []qualificationRecord {
	seen := map[types.UID]struct{}{}

	result := make([]qualificationRecord, 0, len(records))
	for index := range records {
		if _, exists := seen[records[index].Reference.UID]; exists {
			continue
		}

		seen[records[index].Reference.UID] = struct{}{}
		result = append(result, records[index])
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].FinishedAt.Before(result[j].FinishedAt)
	})

	return result
}

func recordReferences(records []qualificationRecord) []signalv1.ContributingRunReference {
	records = uniqueRecords(records)
	if len(records) > signalv1.MaxContributingRuns {
		records = records[len(records)-signalv1.MaxContributingRuns:]
	}

	result := make([]signalv1.ContributingRunReference, len(records))
	for index := range records {
		result[index] = records[index].Reference
	}

	return result
}

func recordPlanDigests(records []qualificationRecord) []string {
	values := make([]string, 0)
	for index := range records {
		if !slices.Contains(values, records[index].Identity.PlanDigest) {
			values = append(values, records[index].Identity.PlanDigest)
		}
	}

	sort.Strings(values)

	if len(values) > signalv1.MaxContributingRuns {
		values = values[len(values)-signalv1.MaxContributingRuns:]
	}

	return values
}

func latestRecordTime(records []qualificationRecord) time.Time {
	var latest time.Time
	for index := range records {
		if records[index].FinishedAt.After(latest) {
			latest = records[index].FinishedAt
		}
	}

	return latest
}
