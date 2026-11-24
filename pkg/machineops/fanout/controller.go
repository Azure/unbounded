// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package fanout resolves a selector MachineOperation into one child
// MachineOperation per matching Machine and paces their execution. The
// children are the per-machine record; the parent's status keeps only counts.
package fanout

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

const (
	controllerName = "machineoperation-fanout"

	defaultPickupTimeout         = 5 * time.Minute
	defaultMaxConcurrent         = 1
	defaultClientQPS     float32 = 100
	defaultBatchSize             = 500
	// runningChildRecheck is how long a finished parent waits before it
	// looks again at a child that still appears to be running.
	runningChildRecheck         = 30 * time.Second
	defaultMinReconcileInterval = time.Second

	conflictRequeueDelay = time.Second
	creatingRequeueDelay = 100 * time.Millisecond
	releaseRequeueDelay  = 100 * time.Millisecond
	createConcurrency    = 16
	maxFailedMachines    = 10
)

// Options tunes the selector fan-out controller.
type Options struct {
	// PickupTimeout: how long a released child may have a completely empty
	// status before it is failed as not picked up. Default 5 minutes.
	PickupTimeout time.Duration
	// MaxConcurrentReconciles defaults to 1.
	MaxConcurrentReconciles int
	// ClientQPS is the rate limit of the client this controller writes
	// with (creates, releases, status). Default 100; burst is twice that.
	// It is separate from the manager's client so that a large operation
	// does not starve, or get starved by, the binary's other controllers.
	ClientQPS float32
	// BatchSize is the most children created, and the most released, in
	// one reconcile. Default 500.
	BatchSize int
	// MinReconcileInterval is the least time between two reconciles of one
	// parent caused by child events. Default 1 second.
	MinReconcileInterval time.Duration
	// Now is the clock; defaults to time.Now. Tests set it.
	Now func() time.Time
}

func (o Options) withDefaults() Options {
	if o.PickupTimeout <= 0 {
		o.PickupTimeout = defaultPickupTimeout
	}

	if o.MaxConcurrentReconciles <= 0 {
		o.MaxConcurrentReconciles = defaultMaxConcurrent
	}

	if o.ClientQPS <= 0 {
		o.ClientQPS = defaultClientQPS
	}

	if o.BatchSize <= 0 {
		o.BatchSize = defaultBatchSize
	}

	if o.MinReconcileInterval <= 0 {
		o.MinReconcileInterval = defaultMinReconcileInterval
	}

	if o.Now == nil {
		o.Now = time.Now
	}

	return o
}

// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machines,verbs=get;list;watch
// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machineoperations,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machineoperations/status,verbs=get;update

// AddToManager registers the fan-out controller with mgr.
func AddToManager(mgr ctrl.Manager, options Options) error {
	if mgr == nil {
		return fmt.Errorf("controller-runtime manager is required")
	}

	options = options.withDefaults()

	writeConfig := rest.CopyConfig(mgr.GetConfig())
	writeConfig.QPS = options.ClientQPS

	burst := int(options.ClientQPS * 2)
	if burst < 1 {
		burst = 1
	}

	writeConfig.Burst = burst

	writer, err := client.New(writeConfig, client.Options{
		Scheme: mgr.GetScheme(),
		Mapper: mgr.GetRESTMapper(),
	})
	if err != nil {
		return fmt.Errorf("create MachineOperation fan-out client: %w", err)
	}

	r := &reconciler{
		Reader:               mgr.GetClient(),
		Writer:               writer,
		APIReader:            mgr.GetAPIReader(),
		PickupTimeout:        options.PickupTimeout,
		BatchSize:            options.BatchSize,
		MinReconcileInterval: options.MinReconcileInterval,
		Now:                  options.Now,
	}

	if err := ctrl.NewControllerManagedBy(mgr).
		Named(controllerName).
		Watches(&unboundedv1alpha3.MachineOperation{}, r.eventHandler()).
		WithOptions(crcontroller.Options{MaxConcurrentReconciles: options.MaxConcurrentReconciles}).
		Complete(r); err != nil {
		return fmt.Errorf("setup MachineOperation fan-out controller: %w", err)
	}

	return nil
}

type reconciler struct {
	// Reader is the manager's cache.
	Reader client.Reader
	// Writer is the controller's own client, separate from the manager's.
	Writer client.Client
	// APIReader reads objects the cache may not show yet.
	APIReader client.Reader

	PickupTimeout        time.Duration
	BatchSize            int
	MinReconcileInterval time.Duration
	Now                  func() time.Time

	// keepFailed is the most failed children a finished parent keeps. Zero
	// means MaxRolloutFailures; tests lower it.
	keepFailed int

	// expect is what this process wrote and the cache has not shown yet.
	expect expectations
}

func (r *reconciler) keepFailedLimit() int {
	if r.keepFailed > 0 {
		return r.keepFailed
	}

	return unboundedv1alpha3.MaxRolloutFailures
}

func (r *reconciler) eventHandler() handler.TypedFuncs[client.Object, reconcile.Request] {
	interval := r.reconcileInterval()

	return handler.TypedFuncs[client.Object, reconcile.Request]{
		CreateFunc: func(_ context.Context, e event.TypedCreateEvent[client.Object], queue workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueueEvent(e.Object, queue, interval)
		},
		UpdateFunc: func(_ context.Context, e event.TypedUpdateEvent[client.Object], queue workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueueEvent(e.ObjectNew, queue, interval)
		},
		DeleteFunc: func(_ context.Context, e event.TypedDeleteEvent[client.Object], queue workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueueEvent(e.Object, queue, interval)
		},
		GenericFunc: func(_ context.Context, e event.TypedGenericEvent[client.Object], queue workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueueEvent(e.Object, queue, interval)
		},
	}
}

// enqueueEvent queues a selector operation immediately. A child queues its
// controller owner after minInterval, so a burst of child events collapses
// into one pending reconcile of the parent.
func enqueueEvent(object client.Object, queue workqueue.TypedRateLimitingInterface[reconcile.Request], minInterval time.Duration) {
	operation, ok := object.(*unboundedv1alpha3.MachineOperation)
	if !ok {
		return
	}

	self := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(operation)}

	if operation.Spec.IsSelectorOperation() {
		queue.Add(self)
	}

	owner := metav1.GetControllerOf(operation)
	if owner == nil || owner.Kind != "MachineOperation" {
		return
	}

	request := reconcile.Request{NamespacedName: client.ObjectKey{Name: owner.Name}}
	if operation.Spec.IsSelectorOperation() && request.NamespacedName == self.NamespacedName {
		return
	}

	queue.AddAfter(request, minInterval)
}

func (r *reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// The parent is read from the API server, not the cache. This is one
	// small read per reconcile, and it means the decision that follows is
	// never made on a parent older than this controller's last write to it:
	// a parent that still looked unresolved would be resolved a second time,
	// against whatever Machines match by then.
	var parent unboundedv1alpha3.MachineOperation
	if err := r.apiReader().Get(ctx, req.NamespacedName, &parent); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, fmt.Errorf("get MachineOperation: %w", err)
	}

	if !parent.Spec.IsSelectorOperation() {
		return ctrl.Result{}, nil
	}

	if !parent.DeletionTimestamp.IsZero() {
		r.expect.forget(parent.UID)
		return ctrl.Result{}, nil
	}

	if parent.Status.IsTerminal() {
		r.expect.forget(parent.UID)
		return r.reconcileFinished(ctx, &parent)
	}

	children, err := r.listChildren(ctx, &parent)
	if err != nil {
		return ctrl.Result{}, err
	}

	resolved := targetsResolved(&parent)

	var (
		desired []string
		total   int
	)

	if resolved {
		// More children than were resolved cannot come from this controller.
		// They are counted, so that the counts add up and the parent ends.
		total = max(resolvedTotal(&parent, len(children)), len(children))
	} else {
		selector, err := metav1.LabelSelectorAsSelector(parent.Spec.MachineSelector)
		if err != nil {
			return r.finishParent(ctx, &parent, unboundedv1alpha3.OperationPhaseFailed, "InvalidSelector",
				fmt.Sprintf("invalid machine selector: %v", err))
		}

		desired, err = r.desiredMachines(ctx, children, selector)
		if err != nil {
			return ctrl.Result{}, err
		}

		if len(desired) == 0 {
			return r.finishParent(ctx, &parent, unboundedv1alpha3.OperationPhaseFailed,
				unboundedv1alpha3.OperationReasonNoMachinesMatched, "machine selector matched no Machines")
		}

		total = len(desired)
	}

	limit := rolloutLimit(parent.Spec.Rollout, total)
	state := newRolloutState(&parent, children, total, resolved)

	creating := false

	if !resolved {
		if missing := missingMachines(desired, children); len(missing) > 0 {
			batch := missing[:min(r.batchSize(), len(missing))]

			created, err := r.createChildren(ctx, &parent, batch, state.capacity(limit))
			if err != nil {
				return ctrl.Result{}, err
			}

			r.expect.recordCreated(parent.UID, created, r.now())

			children = append(children, created...)
			state = newRolloutState(&parent, children, total, resolved)
			creating = len(created) < len(missing)
		}
	}

	if err := r.releaseChildren(ctx, &parent, children, min(state.capacity(limit), r.batchSize())); err != nil {
		return ctrl.Result{}, err
	}

	nextPickupCheck, err := r.failExpiredChildren(ctx, &parent, children)
	if err != nil {
		return ctrl.Result{}, err
	}

	state = newRolloutState(&parent, children, total, resolved)

	updated := parent.DeepCopy()
	now := metav1.NewTime(r.now())

	if updated.Status.StartedAt == nil {
		updated.Status.StartedAt = &now
	}

	updated.Status.Targets = nil
	updated.Status.CompletedAt = nil
	updated.Status.Phase = unboundedv1alpha3.OperationPhaseInProgress

	if creating {
		// Children are still missing. The counts and TargetsResolved wait
		// until every one exists; the children that do exist already run.
		updated.Status.Message = fmt.Sprintf("creating child operations: %d of %d", len(children), total)
		setCompletedCondition(updated, metav1.ConditionFalse, "InProgress", updated.Status.Message, now)

		if result, err := r.updateParentStatus(ctx, &parent, updated); err != nil || result.RequeueAfter > 0 {
			return result, err
		}

		return ctrl.Result{RequeueAfter: creatingRequeueDelay}, nil
	}

	if !resolved {
		apimeta.SetStatusCondition(&updated.Status.Conditions, metav1.Condition{
			Type:               unboundedv1alpha3.MachineOperationConditionTargetsResolved,
			Status:             metav1.ConditionTrue,
			Reason:             "Resolved",
			Message:            fmt.Sprintf("resolved %d Machines", total),
			ObservedGeneration: updated.Generation,
			LastTransitionTime: now,
		})
	}

	counts := state.counts
	updated.Status.Summary = &unboundedv1alpha3.MachineOperationSummary{
		Total:          int32(total),
		Held:           int32(counts.held),
		Active:         int32(counts.active),
		Complete:       int32(counts.complete),
		Failed:         int32(counts.failed),
		FailedMachines: counts.failedSample(),
	}

	message := summaryMessage(total, counts.held, counts.active, counts.complete, counts.failed)

	switch {
	case counts.complete == total:
		updated.Status.Phase = unboundedv1alpha3.OperationPhaseComplete
		updated.Status.CompletedAt = &now
		setCompletedCondition(updated, metav1.ConditionTrue, "Succeeded", message, now)
	case counts.active == 0 && counts.held == 0:
		updated.Status.Phase = unboundedv1alpha3.OperationPhaseFailed
		updated.Status.CompletedAt = &now
		setCompletedCondition(updated, metav1.ConditionFalse, unboundedv1alpha3.OperationReasonTargetFailed, message, now)
	case state.stopped && counts.active == 0:
		updated.Status.Phase = unboundedv1alpha3.OperationPhaseFailed
		updated.Status.CompletedAt = &now

		reason := unboundedv1alpha3.OperationReasonRolloutStopped
		if continueOnFailure(parent.Spec.Rollout) {
			reason = unboundedv1alpha3.OperationReasonFailureLimitReached
		}

		setCompletedCondition(updated, metav1.ConditionFalse, reason, message, now)
	default:
		setCompletedCondition(updated, metav1.ConditionFalse, "InProgress", message, now)
	}

	updated.Status.Message = message

	if result, err := r.updateParentStatus(ctx, &parent, updated); err != nil || result.RequeueAfter > 0 {
		return result, err
	}

	if updated.Status.IsTerminal() {
		r.expect.forget(parent.UID)
		return ctrl.Result{}, nil
	}

	requeueAfter := nextPickupCheck
	if state.capacity(limit) > 0 && counts.held > 0 && (requeueAfter == 0 || releaseRequeueDelay < requeueAfter) {
		requeueAfter = releaseRequeueDelay
	}

	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// rolloutState is what one pass decides from: the counts of the children,
// and whether a failure has stopped the rollout.
type rolloutState struct {
	counts  childCounts
	stopped bool
}

// newRolloutState counts the children. Once the parent is resolved, fewer
// children than were resolved means some were deleted: those count as failed
// and are never created again. The count is taken before anything is
// released, so a deleted child stops a Stop rollout like any other failure.
//
// The children listed here cannot merely be behind on their own creation.
// This process adds the ones it created until the cache lists them. For a
// process that started later, the cache was filled after the parent was
// marked resolved, which happens after every child exists.
func newRolloutState(
	parent *unboundedv1alpha3.MachineOperation,
	children []*unboundedv1alpha3.MachineOperation,
	total int,
	resolved bool,
) rolloutState {
	counts := countChildren(children)

	if missing := total - len(children); resolved && missing > 0 {
		counts.failed += missing
	}

	return rolloutState{
		counts:  counts,
		stopped: counts.failed >= failureLimit(parent.Spec.Rollout),
	}
}

// capacity is how many more children may run now: the pace less the children
// already released and unfinished, and nothing once the rollout has stopped.
func (s rolloutState) capacity(limit int) int {
	if s.stopped {
		return 0
	}

	return max(limit-s.counts.active, 0)
}

func (r *reconciler) reconcileFinished(ctx context.Context, parent *unboundedv1alpha3.MachineOperation) (ctrl.Result, error) {
	var untilExpiry time.Duration

	if parent.Spec.TTLSecondsAfterFinished != nil && parent.Status.CompletedAt != nil {
		ttl := time.Duration(*parent.Spec.TTLSecondsAfterFinished) * time.Second
		expires := parent.Status.CompletedAt.Add(ttl)
		now := r.now()

		// An expired parent goes first and takes its children with it:
		// removing them one by one would only delay it.
		if !now.Before(expires) {
			err := r.Writer.Delete(ctx, parent, client.Preconditions{UID: &parent.UID})
			if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
				return ctrl.Result{}, fmt.Errorf("delete finished MachineOperation: %w", err)
			}

			return ctrl.Result{}, nil
		}

		untilExpiry = expires.Sub(now)
	}

	if result, done, err := r.pruneChildren(ctx, parent); err != nil || !done {
		if untilExpiry > 0 && (result.RequeueAfter == 0 || untilExpiry < result.RequeueAfter) {
			result.RequeueAfter = untilExpiry
		}

		return result, err
	}

	return ctrl.Result{RequeueAfter: untilExpiry}, nil
}

// pruneChildren removes, from a finished parent, the children it does not
// keep, unless the parent retains them all. A fleet-wide operation leaves one
// child per machine, which every controller that watches MachineOperations
// would otherwise hold in memory until the parent is deleted. What stays is
// what an operator needs to look at: the children that failed, up to
// MaxRolloutFailures of them, lowest machine name first. The ones that
// completed go, and so do the held ones a stopped rollout never started,
// which could otherwise still be released after the parent has finished.
// The counts in the parent's summary are final and do not change.
//
// It reports done once nothing is left to remove and the parent says so.
func (r *reconciler) pruneChildren(
	ctx context.Context,
	parent *unboundedv1alpha3.MachineOperation,
) (ctrl.Result, bool, error) {
	if retainsAllChildren(parent.Spec.Rollout) || apimeta.IsStatusConditionTrue(parent.Status.Conditions,
		unboundedv1alpha3.MachineOperationConditionChildrenPruned) {
		return ctrl.Result{}, true, nil
	}

	// A parent that matched nothing never had children.
	if parent.Status.Summary == nil || parent.Status.Summary.Total == 0 {
		return ctrl.Result{}, true, nil
	}

	children, err := r.listChildren(ctx, parent)
	// Listing starts a record of this process's writes for the parent, which
	// a finished parent no longer needs.
	r.expect.forget(parent.UID)

	if err != nil {
		return ctrl.Result{}, false, err
	}

	// The failed children that are kept are the first ones by machine name,
	// so every pass keeps the same ones.
	slices.SortFunc(children, func(a, b *unboundedv1alpha3.MachineOperation) int {
		return strings.Compare(a.Spec.MachineRef, b.Spec.MachineRef)
	})

	var (
		removable []*unboundedv1alpha3.MachineOperation
		running   int
		failed    int
	)

	for _, child := range children {
		switch {
		case !child.DeletionTimestamp.IsZero() || hasBootMedia(child):
			// Already going, or kept for metalman, which reads the media
			// history of finished operations whatever state the media is in.
		case child.Status.Phase == unboundedv1alpha3.OperationPhaseComplete, child.IsHeld():
			removable = append(removable, child)
		case child.Status.Phase == unboundedv1alpha3.OperationPhaseFailed:
			if failed++; failed > r.keepFailedLimit() {
				removable = append(removable, child)
			}
		default:
			running++
		}
	}

	if len(removable) > 0 {
		batch := removable[:min(r.batchSize(), len(removable))]

		var group errgroup.Group

		group.SetLimit(createConcurrency)

		for _, child := range batch {
			group.Go(func() error {
				// The delete names the object and the version that were
				// listed. A child that changed since, for example to Failed
				// after an agent rolled an upgrade back, or released by
				// someone, is refused and looked at again on the next pass.
				err := r.Writer.Delete(ctx, child, client.Preconditions{
					UID:             &child.UID,
					ResourceVersion: &child.ResourceVersion,
				})
				if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
					return fmt.Errorf("remove child MachineOperation %q: %w", child.Name, err)
				}

				return nil
			})
		}

		if err := group.Wait(); err != nil {
			return ctrl.Result{}, false, err
		}

		// The cache may list a removed child a little longer; the next pass
		// finds whatever is left.
		return ctrl.Result{RequeueAfter: time.Second}, false, nil
	}

	// A finished parent has no running child, unless the cache is behind or
	// someone released a held child before it was removed. Either way there
	// may be more to remove later, so the parent is not marked done yet.
	if running > 0 {
		return ctrl.Result{RequeueAfter: runningChildRecheck}, false, nil
	}

	updated := parent.DeepCopy()
	message := fmt.Sprintf("kept %d child operations that failed or carry boot media; the others were removed", len(children))

	apimeta.SetStatusCondition(&updated.Status.Conditions, metav1.Condition{
		Type:               unboundedv1alpha3.MachineOperationConditionChildrenPruned,
		Status:             metav1.ConditionTrue,
		Reason:             "Pruned",
		Message:            message,
		ObservedGeneration: updated.Generation,
		LastTransitionTime: metav1.NewTime(r.now()),
	})

	result, err := r.updateParentStatus(ctx, parent, updated)
	if err != nil || result.RequeueAfter > 0 {
		return result, false, err
	}

	return ctrl.Result{}, true, nil
}

func hasBootMedia(child *unboundedv1alpha3.MachineOperation) bool {
	for i := range child.Status.Targets {
		if child.Status.Targets[i].BootMedia != nil {
			return true
		}
	}

	return false
}

func retainsAllChildren(rollout *unboundedv1alpha3.MachineOperationRollout) bool {
	return rollout != nil && rollout.ChildRetention == unboundedv1alpha3.ChildRetentionAll
}

func (r *reconciler) desiredMachines(
	ctx context.Context,
	children []*unboundedv1alpha3.MachineOperation,
	selector labels.Selector,
) ([]string, error) {
	var machineList unboundedv1alpha3.MachineList
	if err := r.Reader.List(
		ctx, &machineList,
		client.MatchingLabelsSelector{Selector: selector},
		client.UnsafeDisableDeepCopy,
	); err != nil {
		return nil, fmt.Errorf("list Machines for selector: %w", err)
	}

	desired := make(map[string]struct{}, len(machineList.Items)+len(children))

	for i := range machineList.Items {
		machine := &machineList.Items[i]
		if !machine.DeletionTimestamp.IsZero() {
			continue
		}

		desired[machine.Name] = struct{}{}
	}

	// A child created before a restart stays in the set even if its Machine
	// no longer matches the selector.
	for _, child := range children {
		if child.Spec.MachineRef != "" {
			desired[child.Spec.MachineRef] = struct{}{}
		}
	}

	names := make([]string, 0, len(desired))
	for name := range desired {
		names = append(names, name)
	}

	sort.Strings(names)

	return names, nil
}

func (r *reconciler) listChildren(
	ctx context.Context,
	parent *unboundedv1alpha3.MachineOperation,
) ([]*unboundedv1alpha3.MachineOperation, error) {
	var list unboundedv1alpha3.MachineOperationList
	if err := r.Reader.List(
		ctx, &list,
		client.MatchingLabels{
			unboundedv1alpha3.MachineOperationParentUIDLabel: string(parent.UID),
		},
		client.UnsafeDisableDeepCopy,
	); err != nil {
		return nil, fmt.Errorf("list child MachineOperations: %w", err)
	}

	children := make([]*unboundedv1alpha3.MachineOperation, 0, len(list.Items))

	for i := range list.Items {
		child := &list.Items[i]
		if !metav1.IsControlledBy(child, parent) {
			continue
		}

		children = append(children, child)
	}

	// What this process created or released and the cache does not show yet.
	return r.expect.overlay(parent.UID, children, r.now()), nil
}

// failExpiredChildren fails the released children that no executor has
// taken, and returns how long until the next one is due.
//
// A child is due when it has been released for the pickup timeout and no
// child of this parent has been taken by an executor for that long either.
// The second half matters at scale: a provider controller works through
// thousands of released children a few at a time, and the ones still in its
// queue are not abandoned as long as the queue is moving. A machine that
// nothing serves is failed once the others have been taken.
//
// Only a child that carries the released-at annotation is timed. Every child
// this controller lets run has it. A child someone else released by hand does
// not, and is theirs to watch.
func (r *reconciler) failExpiredChildren(
	ctx context.Context,
	parent *unboundedv1alpha3.MachineOperation,
	children []*unboundedv1alpha3.MachineOperation,
) (time.Duration, error) {
	now := r.now()
	timeout := r.pickupTimeout()

	picked := 0

	for _, child := range children {
		if !child.IsHeld() && !statusEmpty(child.Status) {
			picked++
		}
	}

	lastPickup := r.expect.pickupProgress(parent.UID, picked, now)

	var next time.Duration

	// Bounded like the other writes: a fleet whose agents are all gone is
	// failed over several reconciles, not in one that never returns.
	written := 0

	for i, child := range children {
		if child.IsHeld() || !statusEmpty(child.Status) {
			continue
		}

		releasedAt, ok := childReleaseTime(child)
		if !ok {
			continue
		}

		since := releasedAt
		if lastPickup.After(since) {
			since = lastPickup
		}

		if remaining := since.Add(timeout).Sub(now); remaining > 0 || written >= r.batchSize() {
			// A zero delay would mean no requeue at all.
			remaining = max(remaining, time.Second)
			if next == 0 || remaining < next {
				next = remaining
			}

			continue
		}

		updated, wrote, err := r.failNotPickedUp(ctx, child, timeout)
		if err != nil {
			return 0, err
		}

		written++

		if wrote {
			children[i] = updated
		}
	}

	return next, nil
}

func (r *reconciler) failNotPickedUp(
	ctx context.Context,
	child *unboundedv1alpha3.MachineOperation,
	timeout time.Duration,
) (*unboundedv1alpha3.MachineOperation, bool, error) {
	now := metav1.NewTime(r.now())
	message := fmt.Sprintf("not picked up: no agent or provider controller picked it up within %s", timeout)

	updated := child.DeepCopy()
	updated.Status.Phase = unboundedv1alpha3.OperationPhaseFailed
	updated.Status.Message = message
	updated.Status.CompletedAt = &now

	setCompletedCondition(updated, metav1.ConditionFalse, unboundedv1alpha3.OperationReasonNotPickedUp, message, now)

	if err := r.Writer.Status().Update(ctx, updated); err != nil {
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			return nil, false, nil
		}

		return nil, false, fmt.Errorf("fail child MachineOperation %q as not picked up: %w", child.Name, err)
	}

	return updated, true, nil
}

func (r *reconciler) finishParent(
	ctx context.Context,
	parent *unboundedv1alpha3.MachineOperation,
	phase unboundedv1alpha3.OperationPhase,
	reason string,
	message string,
) (ctrl.Result, error) {
	now := metav1.NewTime(r.now())

	updated := parent.DeepCopy()
	if updated.Status.StartedAt == nil {
		updated.Status.StartedAt = &now
	}

	updated.Status.Phase = phase
	updated.Status.Message = message
	updated.Status.CompletedAt = &now
	updated.Status.Targets = nil
	setCompletedCondition(updated, metav1.ConditionFalse, reason, message, now)

	return r.updateParentStatus(ctx, parent, updated)
}

func (r *reconciler) updateParentStatus(
	ctx context.Context,
	parent *unboundedv1alpha3.MachineOperation,
	updated *unboundedv1alpha3.MachineOperation,
) (ctrl.Result, error) {
	if statusEqual(parent.Status, updated.Status) {
		return ctrl.Result{}, nil
	}

	if err := r.Writer.Status().Update(ctx, updated); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{RequeueAfter: conflictRequeueDelay}, nil
		}

		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, fmt.Errorf("update selector MachineOperation status: %w", err)
	}

	return ctrl.Result{}, nil
}

func (r *reconciler) apiReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}

	if r.Writer != nil {
		return r.Writer
	}

	return r.Reader
}

func (r *reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}

	return time.Now()
}

func (r *reconciler) pickupTimeout() time.Duration {
	if r.PickupTimeout <= 0 {
		return defaultPickupTimeout
	}

	return r.PickupTimeout
}

func (r *reconciler) batchSize() int {
	if r.BatchSize <= 0 {
		return defaultBatchSize
	}

	return r.BatchSize
}

func (r *reconciler) reconcileInterval() time.Duration {
	if r.MinReconcileInterval <= 0 {
		return defaultMinReconcileInterval
	}

	return r.MinReconcileInterval
}
