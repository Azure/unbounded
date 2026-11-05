// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/kube"
	publicmachineops "github.com/Azure/unbounded/pkg/machineops"
)

// allocationWaitInterval is how long the MachineAllocation controller waits
// before it looks again at a class that is not ready, at class inputs that
// changed, and at an operation it waits for.
const allocationWaitInterval = 15 * time.Second

// MachineAllocation condition reasons.
const (
	allocationReasonClassNotReady             = "ClassNotReady"
	allocationReasonClassInputsChanged        = "ClassInputsChanged"
	allocationReasonLaunching                 = "Launching"
	allocationReasonAllocated                 = "Allocated"
	allocationReasonSnapshotMissing           = "SnapshotMissing"
	allocationReasonNodeRegistered            = "NodeRegistered"
	allocationReasonNodeNotRegistered         = "NodeNotRegistered"
	allocationReasonNodeConflict              = "NodeConflict"
	allocationReasonFencing                   = "Fencing"
	allocationReasonLifecycleReleased         = "LifecycleReleased"
	allocationReasonAllocationDeleting        = "AllocationDeleting"
	allocationReasonConsumerTerminating       = "ConsumerTerminating"
	allocationReasonConsumerGone              = "ConsumerGone"
	allocationReasonMachineDeleting           = "MachineDeleting"
	allocationReasonAwaitingRelease           = "AwaitingRelease"
	allocationReasonAwaitingAllocateOperation = "AwaitingAllocateOperation"
	allocationReasonReleasing                 = "Releasing"
	allocationReasonReleaseFailed             = "ReleaseFailed"
	allocationReasonReleased                  = "Released"
	allocationReasonAllocateOperationMissing  = "AllocateOperationMissing"
	allocationReasonReleaseOperationMissing   = "ReleaseOperationMissing"
	allocationReasonMachineMissing            = "MachineMissing"
	allocationReasonBeginOutcomeUnknown       = "BeginOutcomeUnknown"
)

const (
	// allocationInputFinalizerPrefix prefixes the allocation UID in the
	// finalizer that keeps a class input ConfigMap until the allocation's
	// HostAllocate no longer needs it.
	allocationInputFinalizerPrefix = "machineallocations.unbounded-cloud.io/"

	allocationMachineNamePrefix = "ub-"
	allocationTokenAlphabet     = "abcdefghijklmnopqrstuvwxyz0123456789"
	allocationTokenIDLength     = 6
)

// MachineAllocationReconciler launches one host for each MachineAllocation of
// a provider that declares capacity, through a Machine and a HostAllocate
// MachineOperation, fences it when the allocation retires, and releases it
// through a HostRelease MachineOperation once the consumer sets lifecycle
// Released.
type MachineAllocationReconciler struct {
	// Client reads MachineAllocations, MachineClasses, Sites,
	// MachineConfigurationVersions, Machines, MachineOperations, Nodes and
	// consumer metadata from the manager cache, and makes every write.
	client.Client
	// ConfigMapReader reads provider-config and bootstrap files ConfigMaps.
	ConfigMapReader client.Reader
	// APIReader reads bootstrap token Secrets and confirms what the cache
	// reports missing or replaced.
	APIReader client.Reader
	Providers []*Provider
	SiteName  string
	// ConsumerKinds are the cluster-scoped consumer kinds whose termination
	// and loss retire an allocation.
	ConsumerKinds []schema.GroupVersionKind
	Now           func() time.Time
}

// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machineallocations,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machineallocations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machineclasses;sites;machineconfigurationversions,verbs=get;list;watch
// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machines,verbs=get;list;watch;create;patch;allocate
// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machineoperations,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;delete

// allocationNames are the deterministic names of the objects an allocation
// creates. Each derives from the allocation UID alone.
type allocationNames struct {
	machine           string
	allocateOperation string
	releaseOperation  string
	tokenID           string
	tokenSecret       string
	inputFinalizer    string
}

func allocationNamesFor(uid types.UID) allocationNames {
	machine := allocationMachineNamePrefix + strings.ReplaceAll(string(uid), "-", "")

	sum := sha256.Sum256([]byte(uid))

	id := make([]byte, allocationTokenIDLength)
	for i := range id {
		id[i] = allocationTokenAlphabet[int(sum[i])%len(allocationTokenAlphabet)]
	}

	return allocationNames{
		machine:           machine,
		allocateOperation: machine + "-allocate",
		releaseOperation:  machine + "-release",
		tokenID:           string(id),
		tokenSecret:       kube.BootstrapTokenSecretName(string(id)),
		inputFinalizer:    allocationInputFinalizerPrefix + string(uid),
	}
}

// operationName is the deterministic name of the allocation's operation of
// kind.
func (n allocationNames) operationName(kind unboundedv1alpha3.OperationKind) string {
	if kind == unboundedv1alpha3.OperationHostRelease {
		return n.releaseOperation
	}

	return n.allocateOperation
}

// allocationState is one reconcile of an allocation: the object as read and
// the status computed for it.
type allocationState struct {
	allocation *unboundedv1alpha3.MachineAllocation
	status     *unboundedv1alpha3.MachineAllocationStatus
	names      allocationNames
	requeue    time.Duration
}

func (s *allocationState) wait(after time.Duration) {
	if s.requeue == 0 || after < s.requeue {
		s.requeue = after
	}
}

func (s *allocationState) result() ctrl.Result {
	return ctrl.Result{RequeueAfter: s.requeue}
}

func (s *allocationState) failed() bool {
	return s.status.FailureReason != ""
}

func (s *allocationState) hasFinalizer() bool {
	return controllerutil.ContainsFinalizer(s.allocation, unboundedv1alpha3.MachineAllocationFinalizer)
}

func (s *allocationState) reference() *unboundedv1alpha3.UIDReference {
	return &unboundedv1alpha3.UIDReference{Name: s.allocation.Name, UID: s.allocation.UID}
}

func (r *MachineAllocationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var allocation unboundedv1alpha3.MachineAllocation
	if err := r.Get(ctx, client.ObjectKey{Name: req.Name}, &allocation); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	capacity, inScope, err := r.allocationScope(ctx, &allocation)
	if err != nil || !inScope {
		return ctrl.Result{}, err
	}

	s := &allocationState{
		allocation: &allocation,
		status:     allocation.Status.DeepCopy(),
		names:      allocationNamesFor(allocation.UID),
	}

	// A finished allocation is left alone.
	if !s.hasFinalizer() && apimeta.IsStatusConditionTrue(allocation.Status.Conditions, unboundedv1alpha3.MachineAllocationConditionReleased) {
		return ctrl.Result{}, nil
	}

	signal, err := r.retirementSignal(ctx, s)
	if err != nil {
		return ctrl.Result{}, err
	}

	if signal != "" {
		return r.reconcileRetired(ctx, s, signal)
	}

	return r.reconcileActive(ctx, s, capacity)
}

// allocationScope reports whether the allocation is in this controller's
// scope, and the capacity of its provider. The snapshot decides, or without
// one the class the allocation names. Without either the allocation is in
// every controller's scope, and it can only fail.
func (r *MachineAllocationReconciler) allocationScope(
	ctx context.Context,
	allocation *unboundedv1alpha3.MachineAllocation,
) (*publicmachineops.Capacity, bool, error) {
	var provider, site string

	if snapshot := allocation.Status.Snapshot; snapshot != nil {
		provider, site = snapshot.Provider, snapshot.SiteName
	} else {
		var class unboundedv1alpha3.MachineClass
		if err := r.Get(ctx, client.ObjectKey{Name: allocation.Spec.ClassRef.Name}, &class); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, true, nil
			}

			return nil, false, fmt.Errorf("get MachineClass %s: %w", allocation.Spec.ClassRef.Name, err)
		}

		provider, site = class.Spec.Provider, class.Spec.SiteRef.Name
	}

	if r.SiteName != "" && site != r.SiteName {
		return nil, false, nil
	}

	capacity, ok := r.capacityFor(provider)

	return capacity, ok, nil
}

func (r *MachineAllocationReconciler) capacityFor(provider string) (*publicmachineops.Capacity, bool) {
	for _, registration := range r.Providers {
		if registration != nil && registration.Name() == provider {
			return registration.Capacity()
		}
	}

	return nil, false
}

// retirementSignal returns the reason the allocation is retired, or "" when
// it is not. Every signal is monotonic.
func (r *MachineAllocationReconciler) retirementSignal(ctx context.Context, s *allocationState) (string, error) {
	allocation := s.allocation

	if allocation.Spec.Lifecycle == unboundedv1alpha3.MachineAllocationReleased {
		return allocationReasonLifecycleReleased, nil
	}

	if !allocation.DeletionTimestamp.IsZero() {
		return allocationReasonAllocationDeleting, nil
	}

	signal, err := r.consumerSignal(ctx, allocation)
	if err != nil || signal != "" {
		return signal, err
	}

	signal, err = r.machineSignal(ctx, s)
	if err != nil || signal != "" {
		return signal, err
	}

	// A completed fence is never undone, even if the signal that caused it can
	// no longer be observed.
	if fenced := apimeta.FindStatusCondition(s.status.Conditions, unboundedv1alpha3.MachineAllocationConditionFenced); fenced != nil && fenced.Status == metav1.ConditionTrue {
		return fenced.Reason, nil
	}

	return "", nil
}

// consumerSignal reports a terminating or lost consumer of a kind listed in
// ConsumerKinds. A consumer the cache does not show, or shows with another
// UID, is lost only when a live read agrees.
func (r *MachineAllocationReconciler) consumerSignal(ctx context.Context, allocation *unboundedv1alpha3.MachineAllocation) (string, error) {
	reference := allocation.Spec.ConsumerRef

	gvk, ok := r.consumerKind(reference.APIGroup, reference.Kind)
	if !ok {
		return "", nil
	}

	key := client.ObjectKey{Name: reference.Name}

	consumer := &metav1.PartialObjectMetadata{}
	consumer.SetGroupVersionKind(gvk)

	err := r.Get(ctx, key, consumer)
	if err != nil && !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("get consumer %s %s: %w", reference.Kind, reference.Name, err)
	}

	if err != nil || consumer.UID != reference.UID {
		consumer = &metav1.PartialObjectMetadata{}
		consumer.SetGroupVersionKind(gvk)

		if err := r.APIReader.Get(ctx, key, consumer); err != nil {
			if apierrors.IsNotFound(err) {
				return allocationReasonConsumerGone, nil
			}

			return "", fmt.Errorf("get consumer %s %s: %w", reference.Kind, reference.Name, err)
		}

		if consumer.UID != reference.UID {
			return allocationReasonConsumerGone, nil
		}
	}

	if !consumer.DeletionTimestamp.IsZero() {
		return allocationReasonConsumerTerminating, nil
	}

	return "", nil
}

func (r *MachineAllocationReconciler) consumerKind(group, kind string) (schema.GroupVersionKind, bool) {
	for _, gvk := range r.ConsumerKinds {
		if gvk.Group == group && gvk.Kind == kind {
			return gvk, true
		}
	}

	return schema.GroupVersionKind{}, false
}

// machineSignal reports a bound Machine that is being deleted, or that was
// recorded and is confirmed gone.
func (r *MachineAllocationReconciler) machineSignal(ctx context.Context, s *allocationState) (string, error) {
	if s.status.MachineRef == nil {
		var machine unboundedv1alpha3.Machine
		if err := r.Get(ctx, client.ObjectKey{Name: s.names.machine}, &machine); err != nil {
			return "", client.IgnoreNotFound(err)
		}

		if boundTo(&machine, s.allocation) && !machine.DeletionTimestamp.IsZero() {
			return allocationReasonMachineDeleting, nil
		}

		return "", nil
	}

	machine, err := r.boundMachine(ctx, s)
	if err != nil {
		return "", err
	}

	if machine == nil || !machine.DeletionTimestamp.IsZero() {
		return allocationReasonMachineDeleting, nil
	}

	return "", nil
}

// boundMachine returns the allocation's Machine, confirming a cache miss with
// a live read. It returns nil when the Machine is gone or is bound to another
// allocation.
func (r *MachineAllocationReconciler) boundMachine(ctx context.Context, s *allocationState) (*unboundedv1alpha3.Machine, error) {
	var machine unboundedv1alpha3.Machine

	found, err := r.getConfirmed(ctx, client.ObjectKey{Name: s.names.machine}, &machine)
	if err != nil || !found || !boundTo(&machine, s.allocation) {
		return nil, err
	}

	return &machine, nil
}

// getConfirmed reads object from the cache and, when the cache does not have
// it, from the API server. It reports false when the object does not exist.
func (r *MachineAllocationReconciler) getConfirmed(ctx context.Context, key client.ObjectKey, object client.Object) (bool, error) {
	err := r.Get(ctx, key, object)
	if apierrors.IsNotFound(err) {
		err = r.APIReader.Get(ctx, key, object)
	}

	if apierrors.IsNotFound(err) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("get %T %s: %w", object, key.Name, err)
	}

	return true, nil
}

func boundTo(machine *unboundedv1alpha3.Machine, allocation *unboundedv1alpha3.MachineAllocation) bool {
	reference := machine.Spec.AllocationRef

	return reference != nil && reference.Name == allocation.Name && reference.UID == allocation.UID
}

// setCondition sets a condition on the computed status. The transition time
// changes only when the status does.
func (r *MachineAllocationReconciler) setCondition(
	s *allocationState,
	conditionType string,
	status metav1.ConditionStatus,
	reason string,
	message string,
) {
	apimeta.SetStatusCondition(&s.status.Conditions, normalizeCondition(metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: s.allocation.Generation,
		LastTransitionTime: metav1.NewTime(r.now()),
	}))
}

// fail records a terminal failure. The first failure is kept.
func (r *MachineAllocationReconciler) fail(
	s *allocationState,
	reason unboundedv1alpha3.MachineAllocationFailureReason,
	message string,
) {
	if s.failed() {
		return
	}

	s.status.FailureReason = reason
	s.status.FailureMessage = TruncateUTF8Bytes(message, MaxConditionMessageBytes)
	r.setCondition(s, unboundedv1alpha3.MachineAllocationConditionAllocated, metav1.ConditionFalse, string(reason), message)
}

// writeStatus updates status on the allocation read at the start of the
// reconcile and returns the updated allocation. A conflict is returned, not
// retried, and nothing is written when the status did not change.
func (r *MachineAllocationReconciler) writeStatus(ctx context.Context, s *allocationState) (*unboundedv1alpha3.MachineAllocation, error) {
	s.status.ObservedGeneration = s.allocation.Generation

	if equality.Semantic.DeepEqual(s.allocation.Status, *s.status) {
		return s.allocation, nil
	}

	updated := s.allocation.DeepCopy()
	updated.Status = *s.status

	if err := r.Status().Update(ctx, updated); err != nil {
		return nil, fmt.Errorf("update MachineAllocation %s status: %w", s.allocation.Name, err)
	}

	return updated, nil
}

// finishWrite writes the status and returns the reconcile result.
func (r *MachineAllocationReconciler) finishWrite(ctx context.Context, s *allocationState) (ctrl.Result, error) {
	if _, err := r.writeStatus(ctx, s); err != nil {
		return ctrl.Result{}, err
	}

	return s.result(), nil
}

// patchMetadata applies mutate to a deep copy of object and sends the
// difference as a merge patch with an optimistic lock. Only metadata may
// change.
func (r *MachineAllocationReconciler) patchMetadata(ctx context.Context, object client.Object, mutate func(client.Object) bool) error {
	base, ok := object.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("copy %T %s", object, object.GetName())
	}

	updated, ok := object.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("copy %T %s", object, object.GetName())
	}

	if !mutate(updated) {
		return nil
	}

	if err := r.Patch(ctx, updated, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return fmt.Errorf("patch %T %s metadata: %w", object, object.GetName(), err)
	}

	return nil
}

// removeFinalizer removes finalizer from object, if it is there. A missing
// object counts as done.
func (r *MachineAllocationReconciler) removeFinalizer(ctx context.Context, object client.Object, finalizer string) error {
	return r.patchMetadata(ctx, object, func(updated client.Object) bool {
		return controllerutil.RemoveFinalizer(updated, finalizer)
	})
}

func (r *MachineAllocationReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}

	return time.Now()
}

// SetupWithManager registers the controller with mgr.
func (r *MachineAllocationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	builder := ctrl.NewControllerManagedBy(mgr).
		Named("machineallocation").
		For(&unboundedv1alpha3.MachineAllocation{}).
		Watches(&unboundedv1alpha3.MachineClass{}, handler.EnqueueRequestsFromMapFunc(r.allocationsForClass)).
		Watches(&unboundedv1alpha3.Machine{}, handler.EnqueueRequestsFromMapFunc(allocationForMachine)).
		Watches(&unboundedv1alpha3.MachineOperation{}, handler.EnqueueRequestsFromMapFunc(allocationForOperation)).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.allocationsForNode))

	for _, gvk := range r.ConsumerKinds {
		consumer := &metav1.PartialObjectMetadata{}
		consumer.SetGroupVersionKind(gvk)

		builder = builder.WatchesMetadata(consumer, handler.EnqueueRequestsFromMapFunc(r.allocationsForConsumer(gvk)))
	}

	return builder.Complete(r)
}

func (r *MachineAllocationReconciler) allocationsForClass(ctx context.Context, class client.Object) []reconcile.Request {
	return r.allocationsMatching(ctx, func(allocation *unboundedv1alpha3.MachineAllocation) bool {
		return allocation.Spec.ClassRef.Name == class.GetName()
	})
}

func (r *MachineAllocationReconciler) allocationsForNode(ctx context.Context, node client.Object) []reconcile.Request {
	return r.allocationsMatching(ctx, func(allocation *unboundedv1alpha3.MachineAllocation) bool {
		return allocationNamesFor(allocation.UID).machine == node.GetName()
	})
}

func (r *MachineAllocationReconciler) allocationsForConsumer(gvk schema.GroupVersionKind) handler.MapFunc {
	return func(ctx context.Context, consumer client.Object) []reconcile.Request {
		return r.allocationsMatching(ctx, func(allocation *unboundedv1alpha3.MachineAllocation) bool {
			reference := allocation.Spec.ConsumerRef

			return reference.APIGroup == gvk.Group && reference.Kind == gvk.Kind && reference.Name == consumer.GetName()
		})
	}
}

func allocationForMachine(_ context.Context, object client.Object) []reconcile.Request {
	machine, ok := object.(*unboundedv1alpha3.Machine)
	if !ok || machine.Spec.AllocationRef == nil {
		return nil
	}

	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: machine.Spec.AllocationRef.Name}}}
}

func allocationForOperation(_ context.Context, object client.Object) []reconcile.Request {
	operation, ok := object.(*unboundedv1alpha3.MachineOperation)
	if !ok || operation.Spec.AllocationRef == nil {
		return nil
	}

	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: operation.Spec.AllocationRef.Name}}}
}

// allocationsMatching lists MachineAllocations from the manager cache and
// returns those for which matches reports true.
func (r *MachineAllocationReconciler) allocationsMatching(
	ctx context.Context,
	matches func(*unboundedv1alpha3.MachineAllocation) bool,
) []reconcile.Request {
	var allocations unboundedv1alpha3.MachineAllocationList
	if err := r.List(ctx, &allocations); err != nil {
		log.FromContext(ctx).Error(err, "list MachineAllocations for a watch event")
		return nil
	}

	var requests []reconcile.Request

	for i := range allocations.Items {
		if matches(&allocations.Items[i]) {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: allocations.Items[i].Name}})
		}
	}

	return requests
}
