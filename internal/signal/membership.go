// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func admissionReader(reader client.Reader, fallback client.Reader) client.Reader {
	if reader != nil {
		return reader
	}

	return fallback
}

func explicitParticipants(policy signalv1.ParticipantPolicy) bool {
	return policy.Count > 1 || policy.NodeNames != nil || policy.TopologyKey != ""
}

func validateParticipantPolicy(policy signalv1.ParticipantPolicy) error {
	if policy.Count < 1 || policy.Count > 128 {
		return fmt.Errorf("count must be between 1 and 128")
	}

	if policy.Count > 1 && (len(policy.NodeNames) == 0 || policy.TopologyKey == "") {
		return fmt.Errorf("grouped participants require explicit nodeNames and topologyKey")
	}

	if policy.NodeNames != nil && len(policy.NodeNames) != int(policy.Count) {
		return fmt.Errorf("nodeNames must contain exactly count members")
	}

	seen := make(map[string]bool, len(policy.NodeNames))
	for _, name := range policy.NodeNames {
		if len(validation.IsDNS1123Subdomain(name)) != 0 {
			return fmt.Errorf("invalid participant nodeName %q", name)
		}

		if seen[name] {
			return fmt.Errorf("duplicate participant nodeName %q", name)
		}

		seen[name] = true
	}

	if policy.TopologyKey != "" && len(validation.IsQualifiedName(policy.TopologyKey)) != 0 {
		return fmt.Errorf("invalid participant topologyKey %q", policy.TopologyKey)
	}

	return nil
}

func (r *AdmissionReconciler) selectExplicitParticipants(
	ctx context.Context,
	requestNode *corev1.Node,
	profile *signalv1.CertificationProfile,
) ([]signalv1.Participant, error) {
	policy := profile.Spec.Participants

	names := append([]string(nil), policy.NodeNames...)
	if len(names) == 0 {
		names = []string{requestNode.Name}
	}

	if !slices.Contains(names, requestNode.Name) {
		return nil, fmt.Errorf("request node %s is not a declared member of profile %s", requestNode.Name, profile.Name)
	}

	if policy.Count > 1 && profile.Spec.Workflow.Name != RackCertificationWorkflow {
		return nil, fmt.Errorf("grouped participants require workflow %s", RackCertificationWorkflow)
	}

	selector, err := metav1.LabelSelectorAsSelector(&policy.Selector)
	if err != nil {
		return nil, fmt.Errorf("participant selector: %w", err)
	}

	sort.Strings(names)
	participants := make([]signalv1.Participant, 0, len(names))
	topologyValue := ""

	for _, name := range names {
		var node corev1.Node
		if err := admissionReader(r.Reader, r.Client).Get(ctx, client.ObjectKey{Name: name}, &node); err != nil {
			return nil, fmt.Errorf("get declared participant %s: %w", name, err)
		}

		if node.UID == "" || name == requestNode.Name && node.UID != requestNode.UID {
			return nil, fmt.Errorf("participant %s identity changed or is empty", name)
		}

		if !selector.Matches(labels.Set(node.Labels)) || !nodeEligible(&node, policy.RequiredResources) {
			return nil, fmt.Errorf("participant %s does not satisfy profile %s", name, profile.Name)
		}

		if policy.TopologyKey != "" {
			value := node.Labels[policy.TopologyKey]
			if value == "" || topologyValue != "" && value != topologyValue {
				return nil, fmt.Errorf("participant %s must have the same nonempty %s topology label", name, policy.TopologyKey)
			}

			topologyValue = value
		}

		participants = append(participants, signalv1.Participant{NodeName: name, NodeUID: node.UID})
	}

	return participants, nil
}

func (r *AdmissionReconciler) existingExplicitRun(
	ctx context.Context,
	participants []signalv1.Participant,
	profile string,
) (bool, error) {
	var runs signalv1.CertificationRunList
	if err := admissionReader(r.Reader, r.Client).List(ctx, &runs, client.InNamespace(r.Namespace)); err != nil {
		return false, fmt.Errorf("list existing participant runs: %w", err)
	}

	for index := range runs.Items {
		run := &runs.Items[index]
		if run.Status.Phase.Terminal() && !containsString(run.Finalizers, RunFinalizer) {
			continue
		}

		overlap := false

		for _, participant := range participants {
			if participantNodeUID(run, participant.NodeName) != "" {
				overlap = true
			}
		}

		if !overlap {
			continue
		}

		if run.DeletionTimestamp.IsZero() && !run.Spec.Cancel && !run.Status.Phase.Terminal() &&
			run.Spec.Profile.Name == profile && sameParticipants(run.Spec.Participants, participants) {
			return true, nil
		}

		return false, fmt.Errorf("declared participants overlap run %s which has not completed cleanup", run.Name)
	}

	return false, nil
}

func sameParticipants(first, second []signalv1.Participant) bool {
	if len(first) != len(second) {
		return false
	}

	counts := make(map[signalv1.Participant]int, len(first))
	for _, participant := range first {
		counts[participant]++
	}

	for _, participant := range second {
		if counts[participant] == 0 {
			return false
		}

		counts[participant]--
	}

	return true
}

func (r *AdmissionReconciler) membershipSnapshot(
	ctx context.Context,
	participants []signalv1.Participant,
	profile *signalv1.CertificationProfile,
	trigger string,
) (*signalv1.AdmissionSnapshot, error) {
	policy := profile.Spec.Participants.DeepCopy()

	policy.NodeNames = make([]string, 0, len(participants))
	for _, participant := range participants {
		policy.NodeNames = append(policy.NodeNames, participant.NodeName)
	}

	admission := &signalv1.AdmissionSnapshot{Participants: *policy, Trigger: trigger}
	if policy.TopologyKey != "" {
		var node corev1.Node
		if err := admissionReader(r.Reader, r.Client).Get(ctx, client.ObjectKey{Name: participants[0].NodeName}, &node); err != nil {
			return nil, err
		}

		admission.TopologyValue = node.Labels[policy.TopologyKey]
	}

	if trigger == "recurring" {
		admission.Recurring = profile.Spec.Recurring.DeepCopy()
		for _, participant := range participants {
			status := recurringNodeStatus(profile.Status.Nodes, participant.NodeName)
			if status.IdleSince == nil || status.NodeUID != participant.NodeUID {
				return nil, fmt.Errorf("participant %s has no identity-bound continuous idle observation", participant.NodeName)
			}

			if admission.IdleSince == nil || admission.IdleSince.Before(status.IdleSince) {
				admission.IdleSince = status.IdleSince.DeepCopy()
			}
		}
	}

	return admission, nil
}

func validateAdmissionContract(run *signalv1.CertificationRun) error {
	admission := run.Spec.Admission
	if admission == nil {
		if len(run.Spec.Participants) > 1 ||
			run.Spec.Workflow.Name == RackCertificationWorkflow && len(run.Spec.ParticipantPlan) > 1 {
			return fmt.Errorf("grouped runs require an immutable admission snapshot")
		}

		return nil
	}

	policy := admission.Participants
	if err := validateParticipantPolicy(policy); err != nil {
		return err
	}

	if len(run.Spec.Participants) != int(policy.Count) || len(policy.NodeNames) != int(policy.Count) {
		return fmt.Errorf("run participants do not match the declared member count")
	}

	seenNames := map[string]bool{}

	seenUIDs := map[string]bool{}
	for _, participant := range run.Spec.Participants {
		if participant.NodeUID == "" || !slices.Contains(policy.NodeNames, participant.NodeName) ||
			seenNames[participant.NodeName] || seenUIDs[string(participant.NodeUID)] {
			return fmt.Errorf("run participant %s has an invalid, duplicate, or undeclared identity", participant.NodeName)
		}

		seenNames[participant.NodeName] = true
		seenUIDs[string(participant.NodeUID)] = true
	}

	if !seenNames[run.Spec.RequestNodeName] {
		return fmt.Errorf("request node %s is not a declared run member", run.Spec.RequestNodeName)
	}

	if policy.TopologyKey != "" && admission.TopologyValue == "" {
		return fmt.Errorf("admission snapshot requires a nonempty topology value")
	}

	if len(run.Spec.Participants) > 1 {
		if run.Spec.Workflow.Name != RackCertificationWorkflow || len(run.Spec.TestPlan) == 0 {
			return fmt.Errorf("grouped runs require the rack workflow and an immutable per-node test plan")
		}
	}

	if len(run.Spec.ParticipantPlan) != len(run.Spec.Participants) {
		return fmt.Errorf("admission requires an inventory snapshot for every declared member")
	}

	if _, _, err := effectiveTestPlan(run); err != nil {
		return err
	}

	if admission.Trigger != "manual" && admission.Trigger != "recurring" {
		return fmt.Errorf("invalid admission trigger %q", admission.Trigger)
	}

	if admission.Trigger == "recurring" {
		if admission.Recurring == nil || !admission.Recurring.Enabled || admission.IdleSince == nil {
			return fmt.Errorf("recurring admission requires an enabled pinned policy and continuous idle observation")
		}

		if err := validateRecurringMembership(policy, admission.Recurring); err != nil {
			return err
		}
	}

	return nil
}

func validateRunAdmission(
	ctx context.Context,
	reader client.Reader,
	run *signalv1.CertificationRun,
	reservationsRequired bool,
	checkPolicy bool,
	now time.Time,
) error {
	if err := validateAdmissionContract(run); err != nil || run.Spec.Admission == nil {
		return err
	}

	admission := run.Spec.Admission
	policy := admission.Participants

	selector, err := metav1.LabelSelectorAsSelector(&policy.Selector)
	if err != nil {
		return fmt.Errorf("pinned participant selector: %w", err)
	}

	nodes := make([]corev1.Node, 0, len(run.Spec.Participants))

	inventory := map[string]signalv1.ParticipantSnapshot{}
	for _, snapshot := range run.Spec.ParticipantPlan {
		if _, duplicate := inventory[snapshot.NodeName]; duplicate {
			return fmt.Errorf("duplicate participant inventory snapshot %s", snapshot.NodeName)
		}

		inventory[snapshot.NodeName] = snapshot
	}

	for _, participant := range run.Spec.Participants {
		var node corev1.Node
		if err := reader.Get(ctx, client.ObjectKey{Name: participant.NodeName}, &node); err != nil {
			return fmt.Errorf("revalidate participant %s: %w", participant.NodeName, err)
		}

		if node.UID != participant.NodeUID {
			return fmt.Errorf("participant %s UID changed", node.Name)
		}

		owner := node.Annotations[ReservationLabel]
		if owner != run.Name && (reservationsRequired || owner != "") {
			return fmt.Errorf("participant %s reservation is %q, expected %q", node.Name, owner, run.Name)
		}

		if policy.TopologyKey != "" && node.Labels[policy.TopologyKey] != admission.TopologyValue {
			return fmt.Errorf("participant %s topology label %s changed or is missing", node.Name, policy.TopologyKey)
		}

		if !selector.Matches(labels.Set(node.Labels)) || !nodeEligible(&node, policy.RequiredResources) {
			return fmt.Errorf("participant %s no longer satisfies the pinned eligibility policy", node.Name)
		}

		if inventory[node.Name] != participantSnapshot(&node) {
			return fmt.Errorf("participant %s inventory or identity changed after snapshot", node.Name)
		}

		if checkPolicy {
			busy, reason, err := (&RecurringReconciler{Reader: reader}).nodeBusy(ctx, &node)
			if err != nil {
				return fmt.Errorf("participant %s: %s: %w", node.Name, reason, err)
			}

			if busy {
				return fmt.Errorf("participant %s: %s", node.Name, reason)
			}
		}

		nodes = append(nodes, node)
	}

	if !checkPolicy {
		return nil
	}

	if run.Spec.Deadline.Duration <= 0 {
		return fmt.Errorf("admission requires a positive deadline")
	}

	if !run.CreationTimestamp.IsZero() && !now.Before(run.CreationTimestamp.Add(run.Spec.Deadline.Duration)) {
		return fmt.Errorf("run deadline expired before workload admission")
	}

	var runs signalv1.CertificationRunList
	if err := reader.List(ctx, &runs, client.InNamespace(run.Namespace)); err != nil {
		return fmt.Errorf("list admission runs: %w", err)
	}

	others := make([]signalv1.CertificationRun, 0, len(runs.Items))
	for _, other := range runs.Items {
		if other.Name == run.Name {
			continue
		}

		for _, participant := range run.Spec.Participants {
			if activeRunForNode([]signalv1.CertificationRun{other}, participant.NodeName) {
				return fmt.Errorf("participant %s has active run %s", participant.NodeName, other.Name)
			}
		}

		others = append(others, other)
	}

	if admission.Trigger == "recurring" {
		return validateRecurringRun(ctx, reader, run, nodes, others, now)
	}

	return nil
}

func (r *RunReconciler) completeExplicitAdmission(ctx context.Context, run *signalv1.CertificationRun) (ctrl.Result, error) {
	if !containsString(run.Finalizers, RunFinalizer) {
		before := run.DeepCopy()

		run.Finalizers = append(run.Finalizers, RunFinalizer)
		if err := r.Patch(ctx, run, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: time.Millisecond}, nil
	}

	reader := admissionReader(r.Reader, r.Client)
	if err := validateRunAdmission(ctx, reader, run, false, true, r.now()); err != nil {
		return r.rejectAdmission(ctx, run, err)
	}

	if err := r.prepareExplicitSnapshot(ctx, run); err != nil {
		return r.rejectAdmission(ctx, run, err)
	}

	// The Run is the durable cleanup anchor before the first reservation write.
	// These writes are not atomic; only a fully revalidated set may start work.
	admission := &AdmissionReconciler{Client: r.Client, Reader: reader}
	if err := admission.reserveParticipants(ctx, run.Spec.Participants, run.Name); err != nil {
		return r.rejectAdmission(ctx, run, err)
	}

	if err := validateRunAdmission(ctx, reader, run, true, true, r.now()); err != nil {
		return r.rejectAdmission(ctx, run, err)
	}

	var latest signalv1.CertificationRun
	if err := reader.Get(ctx, client.ObjectKeyFromObject(run), &latest); err != nil {
		return ctrl.Result{}, err
	}

	if latest.Spec.Cancel || !latest.DeletionTimestamp.IsZero() {
		return ctrl.Result{RequeueAfter: time.Millisecond}, nil
	}

	before := latest.DeepCopy()

	latest.Status.AdmissionReady = true
	if err := r.Status().Patch(ctx, &latest, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return ctrl.Result{}, err
	}

	*run = latest

	return r.startAttempt(ctx, run, 1)
}

func (r *RunReconciler) rejectAdmission(
	ctx context.Context,
	run *signalv1.CertificationRun,
	cause error,
) (ctrl.Result, error) {
	reason := "AdmissionRejected: " + cause.Error()

	if run.Status.Workflow.Name != "" {
		workflow := &unstructured.Unstructured{}
		workflow.SetGroupVersionKind(workflowGVK)

		if err := admissionReader(r.Reader, r.Client).Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: run.Status.Workflow.Name}, workflow); err != nil {
			if !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}

			workflow = nil
		}

		if err := r.preserveAttempt(ctx, run, workflow, "Error", "Inconclusive", reason); err != nil {
			return ctrl.Result{}, err
		}

		outcome := run.Status.Attempts[attemptIndex(run)]

		outcome.ExecutionState, outcome.Verdict, outcome.Reason = "Error", "Inconclusive", reason
		if err := r.preserveFinalEvidence(ctx, run, workflow, &outcome); err != nil {
			return ctrl.Result{}, err
		}
	}

	before := run.DeepCopy()
	run.Status.AdmissionReady = false
	run.Status.Phase = signalv1.RunPhaseInconclusive
	run.Status.ExecutionState = "Error"
	run.Status.Verdict = "Inconclusive"
	run.Status.Reason = reason
	finished := metav1.NewTime(r.now())

	run.Status.FinishedAt = &finished
	if err := r.Status().Patch(ctx, run, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, err
	}

	stopped, err := r.ensureWorkflowStopped(ctx, run)
	if err != nil || !stopped {
		return ctrl.Result{RequeueAfter: time.Second}, err
	}

	_, releaseErr := r.releaseRunReservations(context.WithoutCancel(ctx), run)

	return ctrl.Result{RequeueAfter: time.Millisecond}, releaseErr
}

func (r *RunReconciler) prepareExplicitSnapshot(ctx context.Context, run *signalv1.CertificationRun) error {
	var snapshot corev1.ConfigMap
	if err := admissionReader(r.Reader, r.Client).Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: run.Spec.Script.ConfigMapName}, &snapshot); err != nil {
		return fmt.Errorf("read pinned script before admission: %w", err)
	}

	content, found := snapshot.Data[run.Spec.Script.Key]
	if !found || snapshot.Immutable == nil || !*snapshot.Immutable || digestString(content) != run.Spec.Script.Digest ||
		snapshot.Labels[SnapshotRunLabel] != run.Name {
		return fmt.Errorf("script snapshot does not match the immutable admission contract")
	}

	if controller := metav1.GetControllerOf(&snapshot); controller != nil {
		if !metav1.IsControlledBy(&snapshot, run) {
			return fmt.Errorf("script snapshot is controlled by another run")
		}

		return nil
	}

	before := snapshot.DeepCopy()
	snapshot.OwnerReferences = append(snapshot.OwnerReferences,
		*metav1.NewControllerRef(run, signalv1.GroupVersion.WithKind("CertificationRun")))

	return r.Patch(ctx, &snapshot, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}
