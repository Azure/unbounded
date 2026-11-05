// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/provision"
	publicmachineops "github.com/Azure/unbounded/pkg/machineops"
)

// Reasons the allocation path fails or holds an allocation operation.
const (
	allocationReasonUnavailable       = "AllocationUnavailable"
	allocationReasonNotRecorded       = "OperationNotRecorded"
	allocationReasonFenced            = "AllocationFenced"
	allocationReasonAuthorityReplaced = "AuthorityReplaced"
	reasonRequestBuildFailed          = "RequestBuildFailed"
	reasonMachineFenced               = "MachineFenced"
	reasonUnsupportedOperation        = "UnsupportedOperation"
)

// allocationInputError is an allocation operation input that cannot be
// built. Begin is never called without it.
type allocationInputError struct {
	err error
}

func (e *allocationInputError) Error() string {
	return e.err.Error()
}

func (e *allocationInputError) Unwrap() error {
	return e.err
}

func allocationInputErrorf(format string, args ...any) error {
	return &allocationInputError{err: fmt.Errorf(format, args...)}
}

// allocationOperation is one reconcile of a HostAllocate or HostRelease.
type allocationOperation struct {
	op         *unboundedv1alpha3.MachineOperation
	machine    *unboundedv1alpha3.Machine
	match      providerMatch
	allocation *unboundedv1alpha3.MachineAllocation
	names      allocationNames
	// providerID is the Machine's host providerID.
	providerID string
	// handle reports a persisted provider handle, so Begin returned.
	handle bool
	// begun reports that Begin may have run: the target is InProgress, has
	// attempts, or has a handle.
	begun bool
}

// ReconcileAllocationOperation runs the HostAllocate and HostRelease
// operations of Machines bound to a MachineAllocation. It is the only code
// that calls a provider's HostAllocate or HostRelease callbacks.
func (r *MachineOperationReconciler) ReconcileAllocationOperation(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var op unboundedv1alpha3.MachineOperation
	if err := r.Get(ctx, client.ObjectKey{Name: req.Name}, &op); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !shouldReconcileAllocationOperation(&op) || op.Spec.MachineRef == "" || op.Spec.AllocationRef == nil {
		return ctrl.Result{}, nil
	}

	var machine unboundedv1alpha3.Machine
	if err := r.Get(ctx, client.ObjectKey{Name: op.Spec.MachineRef}, &machine); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	host, err := resolveMachineHost(&machine)
	if err != nil || !r.ownsMachine(&machine, host.provider) {
		return ctrl.Result{}, nil
	}

	match := r.providerFor(host.provider, op.Spec.OperationKind)
	if !match.supported() {
		return ctrl.Result{}, nil
	}

	if _, ok := match.provider.Capacity(); !ok {
		return ctrl.Result{}, nil
	}

	if !sameUIDReference(machine.Spec.AllocationRef, op.Spec.AllocationRef) {
		return ctrl.Result{}, nil
	}

	a := &allocationOperation{
		op:         &op,
		machine:    &machine,
		match:      match,
		names:      allocationNamesFor(op.Spec.AllocationRef.UID),
		providerID: host.providerID,
	}

	if target, ok := operationTarget(&op, machine.Name); ok {
		// The target finished, and the operation did not record it.
		switch target.Phase {
		case unboundedv1alpha3.OperationPhaseComplete:
			return r.completeOperation(ctx, &op, target.ObservedGeneration, target.Message)
		case unboundedv1alpha3.OperationPhaseFailed:
			return r.failOperation(ctx, &op, "TargetFailed", target.Message)
		}

		a.handle = target.ProviderOperation != nil
		a.begun = target.Phase == unboundedv1alpha3.OperationPhaseInProgress || target.Attempts > 0 || a.handle

		if a.handle && target.ProviderOperation.Provider != match.provider.Name() {
			message := fmt.Sprintf("persisted provider operation belongs to %q, not %q", target.ProviderOperation.Provider, match.provider.Name())
			return r.failOperationTarget(ctx, &op, machine.Name, "ProviderOperationMismatch", message)
		}
	}

	a.allocation, err = r.liveAllocation(ctx, &op)
	if err != nil {
		return ctrl.Result{}, err
	}

	if a.allocation == nil {
		return r.failOrHoldAllocationOperation(ctx, a, allocationReasonUnavailable, allocationReasonUnavailable,
			fmt.Sprintf("MachineAllocation %s (%s) is missing, replaced or has no snapshot", op.Spec.AllocationRef.Name, op.Spec.AllocationRef.UID))
	}

	if !a.handle {
		if result, done, err := r.checkRecordedOperation(ctx, a); done {
			return result, err
		}
	}

	if op.Spec.OperationKind == unboundedv1alpha3.OperationHostRelease {
		return r.reconcileHostRelease(ctx, a)
	}

	return r.reconcileHostAllocate(ctx, a)
}

// liveAllocation reads the operation's MachineAllocation. It returns nil when
// the allocation is missing, has another UID or has no snapshot.
func (r *MachineOperationReconciler) liveAllocation(
	ctx context.Context,
	op *unboundedv1alpha3.MachineOperation,
) (*unboundedv1alpha3.MachineAllocation, error) {
	reference := op.Spec.AllocationRef

	var allocation unboundedv1alpha3.MachineAllocation
	if err := r.Get(ctx, client.ObjectKey{Name: reference.Name}, &allocation); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("get MachineAllocation %s: %w", reference.Name, err)
	}

	if allocation.UID != reference.UID || allocation.Status.Snapshot == nil {
		return nil, nil
	}

	return &allocation, nil
}

// checkRecordedOperation lets Begin run only for the operation the
// allocation recorded for its kind. An unrecorded operation under the
// deterministic name waits while the allocation controller may still record
// it; any other one fails.
func (r *MachineOperationReconciler) checkRecordedOperation(ctx context.Context, a *allocationOperation) (ctrl.Result, bool, error) {
	kind := a.op.Spec.OperationKind
	operations := a.allocation.Status.Operations

	recorded := operations.Allocate
	if kind == unboundedv1alpha3.OperationHostRelease {
		recorded = operations.Release
	}

	if recorded != nil && recorded.Name == a.op.Name && recorded.UID == a.op.UID {
		return ctrl.Result{}, false, nil
	}

	if recorded == nil && a.op.Name == a.names.operationName(kind) && mayStillRecord(a) {
		return ctrl.Result{RequeueAfter: r.providerPollInterval()}, true, nil
	}

	message := fmt.Sprintf("MachineAllocation %s did not record %s MachineOperation %s (%s)", a.allocation.Name, kind, a.op.Name, a.op.UID)
	if recorded != nil {
		message = fmt.Sprintf("MachineAllocation %s recorded %s MachineOperation %s (%s), not %s (%s)",
			a.allocation.Name, kind, recorded.Name, recorded.UID, a.op.Name, a.op.UID)
	}

	result, err := r.failOrHoldAllocationOperation(ctx, a, allocationReasonNotRecorded, allocationReasonNotRecorded, message)

	return result, true, err
}

// mayStillRecord reports whether the allocation controller may still record
// an unrecorded operation. It records a HostAllocate only while the
// allocation is active and launching, and a HostRelease once it is released.
func mayStillRecord(a *allocationOperation) bool {
	if a.op.Spec.OperationKind == unboundedv1alpha3.OperationHostRelease {
		return true
	}

	allocation := a.allocation

	return allocation.Spec.Lifecycle != unboundedv1alpha3.MachineAllocationReleased &&
		allocation.DeletionTimestamp.IsZero() &&
		allocation.Status.FailureReason == "" &&
		!a.machine.Spec.Fenced &&
		a.machine.DeletionTimestamp.IsZero()
}

func (r *MachineOperationReconciler) reconcileHostAllocate(ctx context.Context, a *allocationOperation) (ctrl.Result, error) {
	if !a.handle && allocationFenced(a) {
		return r.failOrHoldAllocationOperation(ctx, a, allocationReasonFenced, allocationReasonBeginOutcomeUnknown,
			fmt.Sprintf("MachineAllocation %s is released or being deleted, or Machine %s is fenced or being deleted", a.allocation.Name, a.machine.Name))
	}

	auth, failure, err := r.allocationAuthority(ctx, a)
	if err != nil {
		return ctrl.Result{}, err
	}

	if failure != nil {
		return r.failOrHoldAllocationOperation(ctx, a, failure.Reason, failure.Reason, failure.Message)
	}

	if a.handle {
		return r.pollAllocationOperation(ctx, a, auth, r.allocationRequest(a, nil))
	}

	inputs, err := r.allocateInputs(ctx, a)
	if err != nil {
		var inputErr *allocationInputError
		if !errors.As(err, &inputErr) {
			return ctrl.Result{}, err
		}

		return r.failOrHoldAllocationOperation(ctx, a, reasonRequestBuildFailed, allocationReasonBeginOutcomeUnknown, err.Error())
	}

	return r.beginAllocationOperation(ctx, a, auth, r.allocationRequest(a, inputs))
}

func allocationFenced(a *allocationOperation) bool {
	return a.machine.Spec.Fenced || !a.machine.DeletionTimestamp.IsZero() ||
		a.allocation.Spec.Lifecycle == unboundedv1alpha3.MachineAllocationReleased || !a.allocation.DeletionTimestamp.IsZero()
}

func (r *MachineOperationReconciler) reconcileHostRelease(ctx context.Context, a *allocationOperation) (ctrl.Result, error) {
	allocate, err := r.recordedAllocate(ctx, a.allocation)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !a.handle {
		if waiting := releaseBlocked(a, allocate); waiting != "" {
			log.FromContext(ctx).V(1).Info("waiting to begin HostRelease", "operation", a.op.Name, "reason", waiting)
			return ctrl.Result{RequeueAfter: r.providerPollInterval()}, nil
		}
	}

	auth, failure, err := r.allocationAuthority(ctx, a)
	if err != nil {
		return ctrl.Result{}, err
	}

	if failure != nil {
		if failure.Reason == allocationReasonAuthorityReplaced || a.begun {
			return r.holdAllocationOperation(ctx, a, failure.Reason, failure.Message)
		}

		return r.failAllocationOperation(ctx, a, failure.Reason, failure.Message)
	}

	request := r.allocationRequest(a, nil)
	request.OwnedResources = mergeOwnedResources(a.allocation.Status.OwnedResources, allocate)

	if a.handle {
		return r.pollAllocationOperation(ctx, a, auth, request)
	}

	return r.beginAllocationOperation(ctx, a, auth, request)
}

// recordedAllocate returns the allocation's recorded HostAllocate, or nil
// when none is recorded or it is missing or replaced.
func (r *MachineOperationReconciler) recordedAllocate(
	ctx context.Context,
	allocation *unboundedv1alpha3.MachineAllocation,
) (*unboundedv1alpha3.MachineOperation, error) {
	recorded := allocation.Status.Operations.Allocate
	if recorded == nil {
		return nil, nil
	}

	var allocate unboundedv1alpha3.MachineOperation
	if err := r.Get(ctx, client.ObjectKey{Name: recorded.Name}, &allocate); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("get HostAllocate MachineOperation %s: %w", recorded.Name, err)
	}

	if allocate.UID != recorded.UID {
		return nil, nil
	}

	return &allocate, nil
}

// releaseBlocked returns why HostRelease may not begin yet, or "".
func releaseBlocked(a *allocationOperation, allocate *unboundedv1alpha3.MachineOperation) string {
	switch {
	case a.allocation.Spec.Lifecycle != unboundedv1alpha3.MachineAllocationReleased:
		return "lifecycle is not Released"
	case !a.machine.Spec.Fenced && a.machine.DeletionTimestamp.IsZero():
		return "the Machine is not fenced"
	case a.allocation.Status.Operations.Allocate != nil && (allocate == nil || !allocate.Status.IsTerminal()):
		return "the recorded HostAllocate is missing or not finished"
	}

	return ""
}

// mergeOwnedResources returns the allocation's owned resources and those the
// recorded HostAllocate reported, without duplicates, sorted by id.
func mergeOwnedResources(
	owned []unboundedv1alpha3.ProviderOwnedResource,
	allocate *unboundedv1alpha3.MachineOperation,
) []unboundedv1alpha3.ProviderOwnedResource {
	merged := slices.Clone(owned)

	if allocate != nil && allocate.Status.Allocation != nil {
		for _, resource := range allocate.Status.Allocation.OwnedResources {
			if !slices.Contains(merged, resource) {
				merged = append(merged, resource)
			}
		}
	}

	slices.SortFunc(merged, func(a, b unboundedv1alpha3.ProviderOwnedResource) int {
		return cmp.Or(cmp.Compare(a.ID, b.ID), cmp.Compare(a.NativeID, b.NativeID))
	})

	return merged
}

// allocationAuthority resolves the operation's credential and checks that it
// and the live Site are the instances the allocation froze.
func (r *MachineOperationReconciler) allocationAuthority(
	ctx context.Context,
	a *allocationOperation,
) (*OperationAuth, *authResolutionFailure, error) {
	target, failure := operationAuthTargetFor(a.machine)
	if failure != nil {
		return nil, failure, nil
	}

	credential, auth, failure, err := resolveTargetAuth(ctx, r.Client, r.Client, r.CredentialSecretNamespace, target)
	if failure != nil || err != nil {
		return nil, failure, err
	}

	snapshot := a.allocation.Status.Snapshot
	if credential.UID != snapshot.Credential.UID {
		return nil, &authResolutionFailure{
			Reason: allocationReasonAuthorityReplaced,
			Message: fmt.Sprintf("MachineOperationCredential %s (%s) is not the credential %s (%s) MachineAllocation %s froze",
				credential.Name, credential.UID, snapshot.Credential.Name, snapshot.Credential.UID, a.allocation.Name),
		}, nil
	}

	var site unboundedv1alpha3.Site
	if err := r.Get(ctx, client.ObjectKey{Name: snapshot.SiteName}, &site); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, nil, fmt.Errorf("get Site %s: %w", snapshot.SiteName, err)
		}
	}

	if site.UID != snapshot.SiteUID {
		return nil, &authResolutionFailure{
			Reason:  allocationReasonAuthorityReplaced,
			Message: fmt.Sprintf("Site %s is missing or is not the instance %s MachineAllocation %s froze", snapshot.SiteName, snapshot.SiteUID, a.allocation.Name),
		}, nil
	}

	return auth, nil, nil
}

// allocateInputs are the inputs HostAllocate Begin needs and that are read
// again for every Begin.
type allocateInputs struct {
	providerConfig map[string]string
	userData       string
}

// allocateInputs rebuilds the HostAllocate Begin inputs from the snapshot.
// A missing or changed input is an allocationInputError.
func (r *MachineOperationReconciler) allocateInputs(ctx context.Context, a *allocationOperation) (*allocateInputs, error) {
	allocation := a.allocation
	snapshot := allocation.Status.Snapshot

	providerConfig, err := r.frozenConfigMap(ctx, &snapshot.ProviderConfig)
	if err != nil {
		return nil, err
	}

	var bootstrapFiles map[string]string

	if snapshot.BootstrapFiles != nil {
		configMap, err := r.frozenConfigMap(ctx, snapshot.BootstrapFiles)
		if err != nil {
			return nil, err
		}

		bootstrapFiles = configMap.Data
	}

	expectedProviderID := unboundedv1alpha3.AllocationProviderIDPrefix + string(allocation.UID)
	if allocation.Status.ProviderID != expectedProviderID {
		return nil, allocationInputErrorf("MachineAllocation %s has providerID %q, not %q", allocation.Name, allocation.Status.ProviderID, expectedProviderID)
	}

	token, err := r.allocationToken(ctx, a)
	if err != nil {
		return nil, err
	}

	clusterInfo, err := r.clusterInfo(ctx)
	if err != nil {
		return nil, err
	}

	agentConfig := buildAgentConfig(a.machine, clusterInfo, token)
	agentConfig.Kubelet.Configuration = map[string]any{"providerID": allocation.Status.ProviderID}

	installEnv := provision.AgentInstallEnv(a.machine.Spec.Agent)
	if snapshot.AgentArtifact != nil {
		installEnv = provision.AgentArtifactInstallEnv(snapshot.AgentArtifact)
	}

	userData, err := provision.AllocationCloudInit(agentConfig, installEnv, bootstrapFiles)
	if err != nil {
		return nil, allocationInputErrorf("render user data: %w", err)
	}

	return &allocateInputs{providerConfig: maps.Clone(providerConfig.Data), userData: userData}, nil
}

// frozenConfigMap reads an input ConfigMap and checks it against the
// snapshot.
func (r *MachineOperationReconciler) frozenConfigMap(
	ctx context.Context,
	frozen *unboundedv1alpha3.ObjectContentSnapshot,
) (*corev1.ConfigMap, error) {
	var configMap corev1.ConfigMap
	if err := r.Get(ctx, client.ObjectKey{Namespace: frozen.Namespace, Name: frozen.Name}, &configMap); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, allocationInputErrorf("input ConfigMap %s/%s was not found", frozen.Namespace, frozen.Name)
		}

		return nil, fmt.Errorf("get ConfigMap %s/%s: %w", frozen.Namespace, frozen.Name, err)
	}

	hash, err := configMapDataHash(configMap.Data)
	if err != nil {
		return nil, fmt.Errorf("hash ConfigMap %s/%s: %w", frozen.Namespace, frozen.Name, err)
	}

	if configMap.UID != frozen.UID || hash != frozen.Hash {
		return nil, allocationInputErrorf("input ConfigMap %s/%s is not the content the allocation froze", frozen.Namespace, frozen.Name)
	}

	return &configMap, nil
}

// allocationToken reads the allocation's bootstrap token Secret.
func (r *MachineOperationReconciler) allocationToken(ctx context.Context, a *allocationOperation) (string, error) {
	key := client.ObjectKey{Namespace: metav1.NamespaceSystem, Name: a.names.tokenSecret}

	var secret corev1.Secret
	if err := r.Get(ctx, key, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", allocationInputErrorf("bootstrap token Secret %s/%s was not found", key.Namespace, key.Name)
		}

		return "", fmt.Errorf("get bootstrap token Secret %s/%s: %w", key.Namespace, key.Name, err)
	}

	if secret.Labels[unboundedv1alpha3.LabelAllocationUID] != string(a.allocation.UID) {
		return "", allocationInputErrorf("bootstrap token Secret %s/%s is not MachineAllocation %s's", key.Namespace, key.Name, a.allocation.Name)
	}

	id, tokenSecret := string(secret.Data["token-id"]), string(secret.Data["token-secret"])
	if strings.TrimSpace(id) == "" || strings.TrimSpace(tokenSecret) == "" {
		return "", allocationInputErrorf("bootstrap token Secret %s/%s has no token", key.Namespace, key.Name)
	}

	return id + "." + tokenSecret, nil
}

// allocationRequest returns the allocation input for the operation. Without
// inputs, ProviderConfig and UserData are empty, as for Poll and HostRelease.
func (r *MachineOperationReconciler) allocationRequest(a *allocationOperation, inputs *allocateInputs) *publicmachineops.AllocationRequest {
	allocation := a.allocation

	request := &publicmachineops.AllocationRequest{
		Name:           allocation.Name,
		UID:            allocation.UID,
		ClassName:      allocation.Spec.ClassRef.Name,
		ClassUID:       allocation.Spec.ClassRef.UID,
		HostName:       a.machine.Name,
		Offering:       *allocation.Status.Snapshot.Offering.DeepCopy(),
		OwnedResources: slices.Clone(allocation.Status.OwnedResources),
	}

	if inputs != nil {
		request.ProviderConfig = publicmachineops.ProviderConfig{Data: inputs.providerConfig}
		request.UserData = inputs.userData
	}

	return request
}

// allocationOperationRequest returns the provider request for the
// operation's target.
func (r *MachineOperationReconciler) allocationOperationRequest(
	a *allocationOperation,
	target *unboundedv1alpha3.MachineOperationTargetStatus,
	auth *OperationAuth,
	allocation *publicmachineops.AllocationRequest,
) OperationRequest {
	return OperationRequest{
		MachineName:       a.machine.Name,
		MachineUID:        a.machine.UID,
		MachineGeneration: target.ObservedGeneration,
		OperationName:     a.op.Name,
		OperationUID:      a.op.UID,
		ProviderID:        a.providerID,
		HostImage:         a.allocation.Status.Snapshot.HostImage,
		Operation:         a.op.Spec.OperationKind,
		Auth:              auth,
		Allocation:        allocation,
	}
}

// beginAllocationOperation initializes the target, marks the operation
// InProgress and calls Begin. Every check runs before it.
func (r *MachineOperationReconciler) beginAllocationOperation(
	ctx context.Context,
	a *allocationOperation,
	auth *OperationAuth,
	allocation *publicmachineops.AllocationRequest,
) (ctrl.Result, error) {
	key := client.ObjectKey{Name: a.op.Name}

	if _, ok := operationTarget(a.op, a.machine.Name); !ok {
		if err := r.initializeOperationTarget(ctx, a.op, a.machine, a.match); err != nil {
			var permanentErr *targetInputError
			if errors.As(err, &permanentErr) {
				return r.failOperation(ctx, a.op, reasonRequestBuildFailed, err.Error())
			}

			return ctrl.Result{}, fmt.Errorf("initialize MachineOperation target input: %w", err)
		}

		if err := r.Get(ctx, key, a.op); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}

	message := fmt.Sprintf("executing %s via %s", a.op.Spec.OperationKind, a.match.provider.Name())
	if err := r.ensureOperationInProgress(ctx, key, a.op, message); err != nil {
		return ctrl.Result{}, err
	}

	target, ok := operationTarget(a.op, a.machine.Name)
	if !ok {
		return r.failOperation(ctx, a.op, "TargetStateMissing", fmt.Sprintf("MachineOperation target %s is missing", a.machine.Name))
	}

	begin, beginErr := a.match.operation.Begin(ctx, r.allocationOperationRequest(a, target, auth, allocation))
	if beginErr != nil {
		return r.handleBeginError(ctx, a.op, a.machine.Name, beginErr)
	}

	if strings.TrimSpace(begin.Operation.OperationID) == "" {
		return r.handleBeginError(ctx, a.op, a.machine.Name, errors.New("provider returned an empty operation ID"))
	}

	return r.applyBeginResult(ctx, a.op, a.machine, a.match, begin)
}

// pollAllocationOperation polls the persisted handle. A HostAllocate result
// is written with the terminal target phase, before the operation phase. The
// Machine is never changed.
func (r *MachineOperationReconciler) pollAllocationOperation(
	ctx context.Context,
	a *allocationOperation,
	auth *OperationAuth,
	allocation *publicmachineops.AllocationRequest,
) (ctrl.Result, error) {
	target, _ := operationTarget(a.op, a.machine.Name)
	providerOperation := publicmachineops.ProviderOperation{
		OperationID: target.ProviderOperation.OperationID,
		ResumeToken: target.ProviderOperation.ResumeToken,
	}

	result, err := a.match.operation.Poll(ctx, r.allocationOperationRequest(a, target, auth, allocation), providerOperation)
	if err != nil {
		message := fmt.Sprintf("poll provider operation %s: %v", providerOperation.OperationID, err)

		var permanentErr *publicmachineops.PermanentError
		if errors.As(err, &permanentErr) {
			return r.finishAllocationOperation(ctx, a, unboundedv1alpha3.OperationPhaseFailed, valueOrDefault(permanentErr.Reason, "PermanentPollFailure"), message, nil)
		}

		delay := r.providerPollInterval()
		if r.providerOperationStalled(target) {
			delay = r.providerStalledPollInterval()
		}

		return r.waitForResumableOperation(ctx, a.op.Name, a.machine.Name, "PollFailed", message, delay)
	}

	var allocationResult *publicmachineops.AllocationResult
	if a.op.Spec.OperationKind == unboundedv1alpha3.OperationHostAllocate {
		allocationResult = result.Result.Allocation
	}

	switch result.State {
	case publicmachineops.ProviderOperationStateInProgress:
		delay := durationOrDefault(result.RequeueAfter, r.providerPollInterval())
		if r.providerOperationStalled(target) {
			delay = r.providerStalledPollInterval()
		}

		return r.waitForResumableOperation(ctx, a.op.Name, a.machine.Name, "InProgress",
			valueOrDefault(result.Message, fmt.Sprintf("provider operation %s is in progress", providerOperation.OperationID)), delay)
	case publicmachineops.ProviderOperationStateSucceeded:
		message := valueOrDefault(result.Message, fmt.Sprintf("%s completed via %s", a.op.Spec.OperationKind, a.match.provider.Name()))

		return r.finishAllocationOperation(ctx, a, unboundedv1alpha3.OperationPhaseComplete, "Succeeded", message, allocationResult)
	case publicmachineops.ProviderOperationStateFailed, publicmachineops.ProviderOperationStateCanceled:
		reason := valueOrDefault(result.Reason, string(result.State))
		message := valueOrDefault(result.Message, fmt.Sprintf("provider operation %s %s", providerOperation.OperationID, strings.ToLower(string(result.State))))

		return r.finishAllocationOperation(ctx, a, unboundedv1alpha3.OperationPhaseFailed, reason, message, allocationResult)
	default:
		message := fmt.Sprintf("provider operation %s returned unknown state %q", providerOperation.OperationID, result.State)

		return r.waitForResumableOperation(ctx, a.op.Name, a.machine.Name, "UnknownProviderState", message, r.providerPollInterval())
	}
}

// finishAllocationOperation makes the target terminal, with the HostAllocate
// result in the same status update, and then the operation.
func (r *MachineOperationReconciler) finishAllocationOperation(
	ctx context.Context,
	a *allocationOperation,
	phase unboundedv1alpha3.OperationPhase,
	reason string,
	message string,
	result *publicmachineops.AllocationResult,
) (ctrl.Result, error) {
	targetMessage := message
	if phase == unboundedv1alpha3.OperationPhaseFailed {
		targetMessage = fmt.Sprintf("%s: %s", reason, message)
	}

	if err := r.updateOperationStatus(ctx, a.op.Name, func(latest *unboundedv1alpha3.MachineOperation) {
		target, ok := operationTarget(latest, a.machine.Name)
		if !ok {
			return
		}

		r.markOperationTargetFinished(target, phase, targetMessage)
		target.Message = truncateUTF8Bytes(target.Message, maxConditionMessageBytes)
		latest.Status.Message = target.Message

		if result != nil && latest.Status.Allocation == nil {
			latest.Status.Allocation = &unboundedv1alpha3.MachineOperationAllocationResult{
				ProviderResourceID: result.ProviderResourceID,
				InstanceType:       result.InstanceType,
				Zone:               result.Zone,
				Image:              result.Image,
				OwnedResources:     slices.Clone(result.OwnedResources),
			}
		}
	}); err != nil {
		return ctrl.Result{}, err
	}

	if phase == unboundedv1alpha3.OperationPhaseComplete {
		return r.completeOperation(ctx, a.op, a.machine.Generation, message)
	}

	return r.failOperation(ctx, a.op, reason, message)
}

// failOrHoldAllocationOperation fails the operation with failReason when
// Begin cannot have run, and otherwise holds it with holdReason.
func (r *MachineOperationReconciler) failOrHoldAllocationOperation(
	ctx context.Context,
	a *allocationOperation,
	failReason string,
	holdReason string,
	message string,
) (ctrl.Result, error) {
	if a.begun {
		return r.holdAllocationOperation(ctx, a, holdReason, message)
	}

	return r.failAllocationOperation(ctx, a, failReason, message)
}

func (r *MachineOperationReconciler) failAllocationOperation(
	ctx context.Context,
	a *allocationOperation,
	reason string,
	message string,
) (ctrl.Result, error) {
	if _, ok := operationTarget(a.op, a.machine.Name); ok {
		return r.failOperationTarget(ctx, a.op, a.machine.Name, reason, message)
	}

	return r.failOperation(ctx, a.op, reason, message)
}

// holdAllocationOperation keeps an operation on which Begin may have run
// InProgress and reports why it cannot continue: in the target message and
// as the reason of the Completed condition. Nothing terminal is written.
func (r *MachineOperationReconciler) holdAllocationOperation(
	ctx context.Context,
	a *allocationOperation,
	reason string,
	message string,
) (ctrl.Result, error) {
	reason = normalizeConditionReason(reason)
	message = truncateUTF8Bytes(fmt.Sprintf("%s: %s", reason, message), maxConditionMessageBytes)

	if err := r.updateOperationStatus(ctx, a.op.Name, func(latest *unboundedv1alpha3.MachineOperation) {
		if latest.Status.IsTerminal() {
			return
		}

		latest.Status.Phase = unboundedv1alpha3.OperationPhaseInProgress
		latest.Status.Message = message

		if target, ok := operationTarget(latest, a.machine.Name); ok {
			target.Phase = unboundedv1alpha3.OperationPhaseInProgress
			target.Message = message
		}

		apimeta.SetStatusCondition(&latest.Status.Conditions, normalizeCondition(metav1.Condition{
			Type:               OperationConditionCompleted,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: latest.Generation,
		}))
	}); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: r.providerStalledPollInterval()}, nil
}

// allocationMachineRejection returns why a host operation must not run on a
// Machine bound to a MachineAllocation, or "" when it may. Machines without
// an allocation are unaffected.
func allocationMachineRejection(kind unboundedv1alpha3.OperationKind, machine *unboundedv1alpha3.Machine) (string, string) {
	if machine.Spec.AllocationRef == nil || !isHostOperation(kind) {
		return "", ""
	}

	switch kind {
	case unboundedv1alpha3.OperationHostReplace, unboundedv1alpha3.OperationHostPowerOn, unboundedv1alpha3.OperationHostPowerOff:
		return reasonUnsupportedOperation, fmt.Sprintf("%s is not supported for Machine %s, which is bound to MachineAllocation %s",
			kind, machine.Name, machine.Spec.AllocationRef.Name)
	}

	if machine.Spec.Fenced || !machine.DeletionTimestamp.IsZero() {
		return reasonMachineFenced, fmt.Sprintf("Machine %s is fenced or being deleted", machine.Name)
	}

	return "", ""
}

func shouldReconcileAllocationOperation(op *unboundedv1alpha3.MachineOperation) bool {
	return op.Spec.OperationKind.IsAllocationOperation() && !op.Status.IsTerminal()
}

// SetupAllocationWithManager registers the controller that runs HostAllocate
// and HostRelease operations with mgr.
func (r *MachineOperationReconciler) SetupAllocationWithManager(mgr ctrl.Manager) error {
	maxConcurrent := r.MaxConcurrentReconciles
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("machineoperation-allocation").
		For(&unboundedv1alpha3.MachineOperation{}).
		WithEventFilter(allocationOperationPredicate()).
		WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrent}).
		Complete(reconcile.Func(r.ReconcileAllocationOperation))
}

// allocationOperationPredicate passes non-terminal allocation operations on
// create and generic events, and on updates only when the generation
// changed.
func allocationOperationPredicate() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			op, ok := e.Object.(*unboundedv1alpha3.MachineOperation)
			return ok && shouldReconcileAllocationOperation(op)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			op, ok := e.ObjectNew.(*unboundedv1alpha3.MachineOperation)
			return ok && e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() && shouldReconcileAllocationOperation(op)
		},
		DeleteFunc: func(event.DeleteEvent) bool { return false },
		GenericFunc: func(e event.GenericEvent) bool {
			op, ok := e.Object.(*unboundedv1alpha3.MachineOperation)
			return ok && shouldReconcileAllocationOperation(op)
		},
	}
}
