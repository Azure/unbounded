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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	machinav1 "github.com/Azure/unbounded/api/machina/v1alpha3"
	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

const defaultPolicyEvaluationInterval = time.Minute

type RecurringReconciler struct {
	client.Client
	Reader    client.Reader
	Namespace string
	Admission *AdmissionReconciler
	Now       func() time.Time
}

func (r *RecurringReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&signalv1.CertificationProfile{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

func (r *RecurringReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var profile signalv1.CertificationProfile
	if err := r.Get(ctx, req.NamespacedName, &profile); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	policy := profile.Spec.Recurring
	if policy == nil || !policy.Enabled {
		return ctrl.Result{}, nil
	}

	now := r.now()

	interval := policy.EvaluationInterval.Duration
	if interval <= 0 {
		interval = defaultPolicyEvaluationInterval
	}

	before := profile.DeepCopy()
	profile.Status.ObservedGeneration = profile.Generation

	if err := validateRecurringPolicy(policy); err != nil {
		setProfileCondition(&profile, now, metav1.ConditionFalse, "InvalidPolicy", err.Error())

		return ctrl.Result{RequeueAfter: interval}, r.Status().Patch(ctx, &profile, client.MergeFrom(before))
	}

	if policyExpired(policy, now) {
		for index := range profile.Status.Nodes {
			profile.Status.Nodes[index].Suspended = true
			profile.Status.Nodes[index].Reason = "PolicyExpired"
			profile.Status.Nodes[index].NextDueAt = nil
		}

		setProfileCondition(&profile, now, metav1.ConditionFalse, "Expired",
			fmt.Sprintf("recurring admission expired at %s", policy.ExpiresAt.UTC().Format(time.RFC3339)))

		if err := r.Status().Patch(ctx, &profile, client.MergeFrom(before)); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, r.projectStatuses(ctx, &profile)
	}

	var runs signalv1.CertificationRunList
	if err := r.List(ctx, &runs, client.InNamespace(r.Namespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("list CertificationRuns: %w", err)
	}

	activeByProfile, activeByDomain := activeBudgets(runs.Items, &profile)
	if policy.FailureDomainLabel != "" {
		for index := range runs.Items {
			run := &runs.Items[index]
			if run.Spec.Profile.Name != profile.Name || run.Status.Phase.Terminal() {
				continue
			}

			for _, participant := range run.Spec.Participants {
				var node corev1.Node
				if err := r.Reader.Get(ctx, client.ObjectKey{Name: participant.NodeName}, &node); err != nil {
					continue
				}

				activeByDomain[node.Labels[policy.FailureDomainLabel]]++
			}
		}
	}

	statuses := make([]signalv1.RecurringNodeStatus, 0, len(policy.NodeNames))

	for _, nodeName := range policy.NodeNames {
		old := recurringNodeStatus(profile.Status.Nodes, nodeName)

		status, admitted, err := r.evaluateNode(ctx, &profile, nodeName, old, runs.Items, activeByProfile, activeByDomain, now)
		if err != nil {
			status.Reason = "EvaluationError: " + err.Error()
			statuses = append(statuses, status)

			continue
		}

		statuses = append(statuses, status)

		if admitted {
			activeByProfile++

			var node corev1.Node
			if getErr := r.Reader.Get(ctx, client.ObjectKey{Name: nodeName}, &node); getErr == nil {
				activeByDomain[node.Labels[policy.FailureDomainLabel]]++
			}
		}
	}

	profile.Status.Nodes = statuses
	setProfileCondition(&profile, now, metav1.ConditionTrue, "Evaluated",
		fmt.Sprintf("evaluated %d explicitly opted-in nodes", len(statuses)))

	if err := r.Status().Patch(ctx, &profile, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.projectStatuses(ctx, &profile); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: interval}, nil
}

func (r *RecurringReconciler) projectStatuses(ctx context.Context, profile *signalv1.CertificationProfile) error {
	for _, status := range profile.Status.Nodes {
		if err := projectRecurring(ctx, r.Client, profile, status); err != nil {
			return err
		}
	}

	return nil
}

func (r *RecurringReconciler) evaluateNode(
	ctx context.Context,
	profile *signalv1.CertificationProfile,
	nodeName string,
	old signalv1.RecurringNodeStatus,
	runs []signalv1.CertificationRun,
	activeByProfile int32,
	activeByDomain map[string]int32,
	now time.Time,
) (signalv1.RecurringNodeStatus, bool, error) {
	status := old
	status.NodeName = nodeName
	evaluated := metav1.NewTime(now)
	status.LastEvaluatedAt = &evaluated
	status.Suspended = false

	var node corev1.Node
	if err := r.Reader.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		status.IdleSince = nil

		return status, false, fmt.Errorf("get Node: %w", err)
	}

	selector, err := metav1.LabelSelectorAsSelector(&profile.Spec.Participants.Selector)
	if err != nil {
		return status, false, fmt.Errorf("participant selector: %w", err)
	}

	if !selector.Matches(labels.Set(node.Labels)) || !nodeEligible(&node, profile.Spec.Participants.RequiredResources) {
		status.IdleSince = nil
		status.Reason = "NodeIneligible"

		return status, false, nil
	}

	busy, reason, err := r.nodeBusy(ctx, &node)
	if err != nil {
		status.IdleSince = nil

		return status, false, err
	}

	if busy {
		status.IdleSince = nil
		status.Reason = reason

		return status, false, nil
	}

	if status.IdleSince == nil {
		idleSince := metav1.NewTime(now)
		status.IdleSince = &idleSince
	}

	nodeRuns := runsForNodeProfile(runs, nodeName, profile.Name)
	lastSuccess, consecutiveFailures, latestFailure := runHistory(nodeRuns)
	status.ConsecutiveFailures = consecutiveFailures

	if lastSuccess != nil {
		status.LastSuccessfulRun = lastSuccess.Name
		status.LastSuccessfulAt = lastSuccess.Status.FinishedAt
	}

	due := dueAt(profile.Spec.Recurring, status.IdleSince, lastSuccess)
	status.NextDueAt = ptrMetaTime(due)

	if suspended, reason := failureGate(profile.Spec.Recurring, consecutiveFailures, latestFailure, now); reason != "" {
		status.Suspended = suspended
		status.Reason = reason

		return status, false, nil
	}

	if now.Before(due) {
		status.Reason = "NotDue"

		return status, false, nil
	}

	if activeRunForNode(runs, nodeName) || node.Annotations[ReservationLabel] != "" {
		status.Reason = "RunActive"

		return status, false, nil
	}

	if excludedRunActive(runs, nodeName, profile.Spec.Recurring.ExclusiveProfiles) {
		status.Reason = "ExclusiveProfileActive"

		return status, false, nil
	}

	if activeByProfile >= profile.Spec.Recurring.MaxConcurrent {
		status.Reason = "ConcurrencyBudget"

		return status, false, nil
	}

	domain := node.Labels[profile.Spec.Recurring.FailureDomainLabel]
	if profile.Spec.Recurring.FailureDomainLabel != "" &&
		profile.Spec.Recurring.MaxConcurrentPerFailureDomain > 0 &&
		activeByDomain[domain] >= profile.Spec.Recurring.MaxConcurrentPerFailureDomain {
		status.Reason = "FailureDomainBudget"

		return status, false, nil
	}

	if err := r.Admission.admitNodeWithTrigger(ctx, &node, profile.Name, "recurring"); err != nil {
		if apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err) {
			status.Reason = "AdmissionRaced"

			return status, false, nil
		}

		return status, false, fmt.Errorf("automatic admission: %w", err)
	}

	status.Reason = "Admitted"
	status.NextDueAt = nil

	return status, true, nil
}

func (r *RecurringReconciler) nodeBusy(ctx context.Context, node *corev1.Node) (bool, string, error) {
	var pods corev1.PodList
	if err := r.Reader.List(ctx, &pods, client.MatchingFields{"spec.nodeName": node.Name}); err != nil {
		return true, "WorkloadStateUnavailable", fmt.Errorf("list Node pods: %w", err)
	}

	for index := range pods.Items {
		pod := &pods.Items[index]
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}

		if controlledByDaemonSet(pod) {
			continue
		}

		return true, "WorkloadActive", nil
	}

	active, err := r.activeMachineOperation(ctx, node.Name)
	if err != nil {
		return true, "MaintenanceStateUnavailable", err
	}

	if active {
		return true, "MaintenanceOperationActive", nil
	}

	return false, "", nil
}

func (r *RecurringReconciler) activeMachineOperation(ctx context.Context, nodeName string) (bool, error) {
	var machines machinav1.MachineList
	if err := r.Reader.List(ctx, &machines); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}

		return false, fmt.Errorf("list Machines: %w", err)
	}

	machineNames := map[string]struct{}{}

	for index := range machines.Items {
		machine := &machines.Items[index]
		if machine.Spec.Kubernetes != nil &&
			machine.Spec.Kubernetes.NodeRef != nil &&
			machine.Spec.Kubernetes.NodeRef.Name == nodeName {
			machineNames[machine.Name] = struct{}{}
		}
	}

	if len(machineNames) == 0 {
		return false, nil
	}

	var operations machinav1.MachineOperationList
	if err := r.Reader.List(ctx, &operations); err != nil {
		return false, fmt.Errorf("list MachineOperations: %w", err)
	}

	for index := range operations.Items {
		operation := &operations.Items[index]
		if operation.Status.Phase != machinav1.OperationPhasePending &&
			operation.Status.Phase != machinav1.OperationPhaseInProgress {
			continue
		}

		if _, ok := machineNames[operation.Spec.MachineRef]; ok {
			return true, nil
		}

		if operation.Spec.MachineSelector != nil {
			selector, err := metav1.LabelSelectorAsSelector(operation.Spec.MachineSelector)
			if err != nil {
				return false, fmt.Errorf("MachineOperation %s selector: %w", operation.Name, err)
			}

			for machineIndex := range machines.Items {
				machine := &machines.Items[machineIndex]
				if _, ok := machineNames[machine.Name]; ok && selector.Matches(labels.Set(machine.Labels)) {
					return true, nil
				}
			}
		}
	}

	return false, nil
}

func controlledByDaemonSet(pod *corev1.Pod) bool {
	for _, owner := range pod.OwnerReferences {
		if owner.Controller != nil && *owner.Controller && owner.Kind == "DaemonSet" {
			return true
		}
	}

	return false
}

func validateRecurringPolicy(policy *signalv1.RecurringPolicy) error {
	switch {
	case len(policy.NodeNames) == 0:
		return fmt.Errorf("nodeNames must explicitly opt in at least one Node")
	case policy.MinIdleDuration.Duration < 15*time.Minute:
		return fmt.Errorf("minIdleDuration must be at least 15 minutes")
	case policy.ValidFor.Duration <= 0:
		return fmt.Errorf("validFor must be positive")
	case policy.ExpiresAt.IsZero():
		return fmt.Errorf("expiresAt is required")
	case policy.MaxConcurrent < 1:
		return fmt.Errorf("maxConcurrent must be positive")
	case policy.MaxConsecutiveFailures < 1:
		return fmt.Errorf("maxConsecutiveFailures must be positive")
	}

	return nil
}

func policyExpired(policy *signalv1.RecurringPolicy, now time.Time) bool {
	return !now.Before(policy.ExpiresAt.Time)
}

func failureGate(
	policy *signalv1.RecurringPolicy,
	consecutiveFailures int32,
	latestFailure *time.Time,
	now time.Time,
) (bool, string) {
	if policy.MaxConsecutiveFailures > 0 && consecutiveFailures >= policy.MaxConsecutiveFailures {
		return true, "CircuitOpen"
	}

	if latestFailure != nil && now.Before(latestFailure.Add(policy.FailureBackoff.Duration)) {
		return false, "FailureBackoff"
	}

	return false, ""
}

func dueAt(
	policy *signalv1.RecurringPolicy,
	idleSince *metav1.Time,
	lastSuccess *signalv1.CertificationRun,
) time.Time {
	due := idleSince.Add(policy.MinIdleDuration.Duration)
	if policy.NotBefore != nil && due.Before(policy.NotBefore.Time) {
		due = policy.NotBefore.Time
	}

	if lastSuccess != nil && lastSuccess.Status.FinishedAt != nil {
		freshUntil := lastSuccess.Status.FinishedAt.Add(policy.ValidFor.Duration)
		if due.Before(freshUntil) {
			due = freshUntil
		}
	}

	return due
}

func activeBudgets(runs []signalv1.CertificationRun, profile *signalv1.CertificationProfile) (int32, map[string]int32) {
	var active int32

	domains := map[string]int32{}

	for index := range runs {
		run := &runs[index]
		if run.Spec.Profile.Name != profile.Name || run.Status.Phase.Terminal() {
			continue
		}

		active++
	}

	return active, domains
}

func activeRunForNode(runs []signalv1.CertificationRun, nodeName string) bool {
	for index := range runs {
		run := &runs[index]
		if run.Status.Phase.Terminal() {
			continue
		}

		for _, participant := range run.Spec.Participants {
			if participant.NodeName == nodeName {
				return true
			}
		}
	}

	return false
}

func excludedRunActive(runs []signalv1.CertificationRun, nodeName string, profiles []string) bool {
	for index := range runs {
		run := &runs[index]
		if run.Status.Phase.Terminal() || !slices.Contains(profiles, run.Spec.Profile.Name) {
			continue
		}

		for _, participant := range run.Spec.Participants {
			if participant.NodeName == nodeName {
				return true
			}
		}
	}

	return false
}

func runsForNodeProfile(
	runs []signalv1.CertificationRun,
	nodeName string,
	profileName string,
) []signalv1.CertificationRun {
	result := make([]signalv1.CertificationRun, 0)

	for index := range runs {
		run := runs[index]
		if run.Spec.Profile.Name != profileName || !run.Status.Phase.Terminal() {
			continue
		}

		for _, participant := range run.Spec.Participants {
			if participant.NodeName == nodeName {
				result = append(result, run)

				break
			}
		}
	}

	sort.Slice(result, func(i, j int) bool {
		return completionTime(&result[i]).After(completionTime(&result[j]))
	})

	return result
}

func runHistory(
	runs []signalv1.CertificationRun,
) (*signalv1.CertificationRun, int32, *time.Time) {
	var (
		lastSuccess        *signalv1.CertificationRun
		latestFailure      *time.Time
		consecutiveFailure int32
	)

	for index := range runs {
		run := &runs[index]
		if run.Status.Phase == signalv1.RunPhaseSucceeded {
			lastSuccess = run

			break
		}

		if run.Status.Phase != signalv1.RunPhaseFailed && run.Status.Phase != signalv1.RunPhaseInconclusive {
			continue
		}

		consecutiveFailure++

		if latestFailure == nil {
			value := completionTime(run)
			latestFailure = &value
		}
	}

	return lastSuccess, consecutiveFailure, latestFailure
}

func completionTime(run *signalv1.CertificationRun) time.Time {
	if run.Status.FinishedAt != nil {
		return run.Status.FinishedAt.Time
	}

	return run.CreationTimestamp.Time
}

func recurringNodeStatus(statuses []signalv1.RecurringNodeStatus, nodeName string) signalv1.RecurringNodeStatus {
	for _, status := range statuses {
		if status.NodeName == nodeName {
			return status
		}
	}

	return signalv1.RecurringNodeStatus{NodeName: nodeName}
}

func ptrMetaTime(value time.Time) *metav1.Time {
	result := metav1.NewTime(value)

	return &result
}

func setProfileCondition(
	profile *signalv1.CertificationProfile,
	now time.Time,
	status metav1.ConditionStatus,
	reason string,
	message string,
) {
	apimeta.SetStatusCondition(&profile.Status.Conditions, metav1.Condition{
		Type:               "RecurringAdmissionReady",
		Status:             status,
		ObservedGeneration: profile.Generation,
		LastTransitionTime: metav1.NewTime(now),
		Reason:             reason,
		Message:            message,
	})
}

func (r *RecurringReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}

	return time.Now()
}
