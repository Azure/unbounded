// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

const (
	bootIDPath                 = "/proc/sys/kernel/random/boot_id"
	hostSoftRebootMessage      = "rebooting host operating system"
	hostSoftRebootWaitInterval = 10 * time.Second
)

// HostSoftRebootTarget identifies the local target of MachineOperations.
// Reference is the machineRef used by operations, not necessarily a Machine CR.
type HostSoftRebootTarget interface {
	Reference() string
	ObservedGeneration(context.Context, client.Reader) (int64, error)
}

// HostRebooter reads host boot evidence and requests a host reboot.
type HostRebooter interface {
	BootID() (string, error)
	RebootHost(context.Context, *slog.Logger) error
}

// SystemHostRebooter executes the host operating system reboot helpers.
type SystemHostRebooter struct{}

func (SystemHostRebooter) BootID() (string, error) { return ReadBootID() }
func (SystemHostRebooter) RebootHost(ctx context.Context, log *slog.Logger) error {
	return RebootHost(ctx, log)
}

// ReadBootID reads the current host kernel boot ID.
func ReadBootID() (string, error) {
	data, err := os.ReadFile(bootIDPath)
	if err != nil {
		return "", fmt.Errorf("read boot ID: %w", err)
	}

	id := strings.TrimSpace(string(data))
	if id == "" {
		return "", fmt.Errorf("read boot ID: %s is empty", bootIDPath)
	}

	return id, nil
}

// HostSoftRebootHandler implements the status and boot-evidence protocol.
// The caller serializes runtime work, holds its mutation lock during Reconcile,
// and uses requested to defer other work during an accepted reboot handoff.
type HostSoftRebootHandler struct {
	client    client.Client
	reader    client.Reader
	log       *slog.Logger
	target    HostSoftRebootTarget
	reference string
	host      HostRebooter
	requested *atomic.Bool
}

// NewHostSoftRebootHandler constructs a handler with a live API reader and an
// explicit host executor. No system reboot is used as a fallback.
func NewHostSoftRebootHandler(
	c client.Client,
	reader client.Reader,
	log *slog.Logger,
	target HostSoftRebootTarget,
	host HostRebooter,
	requested *atomic.Bool,
) (*HostSoftRebootHandler, error) {
	if nilDependency(c) || nilDependency(reader) || log == nil || nilDependency(target) || nilDependency(host) || requested == nil {
		return nil, errors.New("client, reader, logger, target, host and requested flag are required")
	}

	ref := target.Reference()
	if ref == "" {
		return nil, errors.New("target reference is required")
	}

	return &HostSoftRebootHandler{
		client:    c,
		reader:    reader,
		log:       log,
		target:    target,
		reference: ref,
		host:      host,
		requested: requested,
	}, nil
}

func nilDependency(value any) bool {
	if value == nil {
		return true
	}

	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

// Reconcile handles a HostSoftReboot routed to this target. The caller must
// hold its mutation lock. Status recording is required before the host command runs.
func (h *HostSoftRebootHandler) Reconcile(ctx context.Context, store MachineOperationStore[int64], op MachineOperation) (ctrl.Result, error) {
	if h.requested.Load() {
		h.log.Info("not starting a HostSoftReboot: this host is already rebooting", "operation", op.Name)
		return ctrl.Result{}, nil
	}

	blocker, err := h.unfinishedOutOfBandHostOperation(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	if blocker != "" {
		message := fmt.Sprintf("waiting for out-of-band host operation %s", blocker)
		h.log.Info("not starting a HostSoftReboot: an out-of-band host operation on this machine is unfinished",
			"operation", op.Name, "blockedBy", blocker)

		return ctrl.Result{RequeueAfter: hostSoftRebootWaitInterval}, h.markWaiting(ctx, op.Name, message)
	}

	bootID, err := h.host.BootID()
	if err != nil {
		return ctrl.Result{}, store.Finish(ctx, op, MachineOperationResult[int64]{
			Phase:   v1alpha3.OperationPhaseFailed,
			Reason:  "ExecutionFailed",
			Message: err.Error(),
		})
	}

	generation, err := h.target.ObservedGeneration(ctx, h.reader)
	if err != nil {
		return ctrl.Result{}, err
	}

	started, err := h.start(ctx, op.Name, generation, bootID)
	if err != nil || !started {
		return ctrl.Result{}, err
	}

	h.log.Info("rebooting host for HostSoftReboot", "operation", op.Name, "bootID", bootID)
	h.requested.Store(true)

	if err := h.host.RebootHost(ctx, h.log); err != nil {
		if ctx.Err() != nil {
			// Shutdown may be the reboot itself; the next daemon judges the boot ID.
			h.log.Warn("daemon stopping while rebooting host; the next daemon reports HostSoftReboot",
				"operation", op.Name, "error", err)

			return ctrl.Result{}, nil
		}

		h.requested.Store(false)

		return ctrl.Result{}, h.finish(ctx, op.Name, MachineOperationResult[int64]{
			Phase:   v1alpha3.OperationPhaseFailed,
			Reason:  "Failed",
			Message: err.Error(),
		})
	}

	return ctrl.Result{}, nil
}

func operationTargetStatus(status *v1alpha3.MachineOperationStatus, ref string) *v1alpha3.MachineOperationTargetStatus {
	for i := range status.Targets {
		if status.Targets[i].MachineRef == ref {
			return &status.Targets[i]
		}
	}

	return nil
}

// matches reports whether op names this machine. An operation without a
// machineRef is never a match, whatever its selector says: a central controller
// resolves selectors into one machineRef operation per machine.
func (h *HostSoftRebootHandler) matches(op *v1alpha3.MachineOperation) bool {
	return op.Spec.MachineRef == h.reference
}

func (h *HostSoftRebootHandler) start(ctx context.Context, name string, generation int64, bootID string) (bool, error) {
	started := false

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var op v1alpha3.MachineOperation
		if err := h.client.Get(ctx, client.ObjectKey{Name: name}, &op); err != nil {
			return client.IgnoreNotFound(err)
		}

		if op.Status.IsTerminal() {
			started = false
			return nil
		}

		now := metav1.Now()
		op.Status.Phase = v1alpha3.OperationPhaseInProgress
		op.Status.Message = hostSoftRebootMessage

		op.Status.ObservedMachineGeneration = generation
		if op.Status.StartedAt == nil {
			op.Status.StartedAt = &now
		}

		apimeta.SetStatusCondition(&op.Status.Conditions, metav1.Condition{
			Type:               "Completed",
			Status:             metav1.ConditionFalse,
			Reason:             "InProgress",
			Message:            hostSoftRebootMessage,
			ObservedGeneration: op.Generation,
		})

		target := operationTargetStatus(&op.Status, h.reference)
		if target == nil {
			op.Status.Targets = append(op.Status.Targets, v1alpha3.MachineOperationTargetStatus{MachineRef: h.reference})
			target = &op.Status.Targets[len(op.Status.Targets)-1]
		}

		target.Phase = v1alpha3.OperationPhaseInProgress
		target.Message = hostSoftRebootMessage
		target.StartedAt = &now
		target.CompletedAt = nil
		target.ObservedGeneration = generation
		target.BootID = bootID

		if err := h.client.Status().Update(ctx, &op); err != nil {
			return err
		}

		started = true

		return nil
	})
	if err != nil {
		return false, fmt.Errorf("record HostSoftReboot boot ID: %w", err)
	}

	return started, nil
}

func (h *HostSoftRebootHandler) finish(ctx context.Context, name string, result MachineOperationResult[int64]) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var op v1alpha3.MachineOperation
		if err := h.client.Get(ctx, client.ObjectKey{Name: name}, &op); err != nil {
			return client.IgnoreNotFound(err)
		}

		if op.Status.IsTerminal() {
			return nil
		}

		now := metav1.Now()
		SetMachineOperationResult(&op, result, now)

		if target := operationTargetStatus(&op.Status, h.reference); target != nil {
			target.Phase = result.Phase
			target.Message = result.Message
			target.CompletedAt = &now
		}

		return h.client.Status().Update(ctx, &op)
	})
}

func isOutOfBandHostOperation(kind v1alpha3.OperationKind) bool {
	return strings.HasPrefix(string(kind), "Host") && kind != v1alpha3.OperationHostSoftReboot && !kind.IsAllocationOperation()
}

func operationBefore(a, b *v1alpha3.MachineOperation) bool {
	// An operation that was held for its turn in a rollout joins the queue
	// when it is released, so it waits for one that started in the meantime.
	aQueued, bQueued := a.QueueTime(), b.QueueTime()
	if !aQueued.Equal(&bQueued) {
		return aQueued.Before(&bQueued)
	}

	return a.Name < b.Name
}

func (h *HostSoftRebootHandler) unfinishedOutOfBandHostOperation(ctx context.Context) (string, error) {
	var ops v1alpha3.MachineOperationList
	if err := h.reader.List(ctx, &ops); err != nil {
		return "", fmt.Errorf("list MachineOperations: %w", err)
	}

	var oldest *v1alpha3.MachineOperation

	for i := range ops.Items {
		op := &ops.Items[i]
		if !isOutOfBandHostOperation(op.Spec.OperationKind) {
			continue
		}

		unfinished, err := h.hostOperationUnfinishedOn(ctx, op)
		if err != nil {
			return "", fmt.Errorf("match out-of-band MachineOperation %s: %w", op.Name, err)
		}

		if unfinished && (oldest == nil || operationBefore(op, oldest)) {
			oldest = op
		}
	}

	if oldest == nil {
		return "", nil
	}

	return oldest.Name, nil
}

func (h *HostSoftRebootHandler) hostOperationUnfinishedOn(_ context.Context, op *v1alpha3.MachineOperation) (bool, error) {
	// A selector operation lists this machine as a target long before its
	// turn comes. Its machineRef operation for this machine is what acts on
	// the host, so that one holds the reboot and the parent does not.
	if op.Spec.IsSelectorOperation() {
		return false, nil
	}

	// A held operation has not started and may not for a long time: it does
	// not hold the reboot. Once released it does.
	if op.IsHeld() {
		return false, nil
	}

	target := operationTargetStatus(&op.Status, h.reference)
	// Cleanup or an unconfirmed media boot can outlast a terminal operation.
	if target != nil && target.HasOutstandingBootMedia() {
		return true, nil
	}

	if op.Status.IsTerminal() {
		return false, nil
	}
	// Resolved targets are authoritative over the spec.
	if len(op.Status.Targets) > 0 {
		return target != nil && target.Phase != v1alpha3.OperationPhaseComplete && target.Phase != v1alpha3.OperationPhaseFailed, nil
	}

	return h.matches(op), nil
}

func (h *HostSoftRebootHandler) markWaiting(ctx context.Context, name, message string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var op v1alpha3.MachineOperation
		if err := h.client.Get(ctx, client.ObjectKey{Name: name}, &op); err != nil {
			return client.IgnoreNotFound(err)
		}

		if op.Status.IsTerminal() || op.Status.Phase == v1alpha3.OperationPhaseInProgress {
			return nil
		}

		if op.Status.Phase == v1alpha3.OperationPhasePending && op.Status.Message == message {
			return nil
		}

		op.Status.Phase = v1alpha3.OperationPhasePending
		op.Status.Message = message

		return h.client.Status().Update(ctx, &op)
	})
}

// PublishStartup judges previous reboot handoffs before the caller admits work.
// It must only run during startup, not after this process issues a reboot.
// Errors are returned to the caller, which controls startup retry behavior.
func (h *HostSoftRebootHandler) PublishStartup(ctx context.Context) error {
	var ops v1alpha3.MachineOperationList
	if err := h.client.List(ctx, &ops); err != nil {
		return fmt.Errorf("list MachineOperations: %w", err)
	}

	var (
		bootID string
		errs   []error
	)

	for i := range ops.Items {
		op := &ops.Items[i]
		// Only this machine's own operation is reported: a selector operation
		// lists the machine as a target, but its status belongs to the
		// controller that fans it out.
		if op.Spec.OperationKind != v1alpha3.OperationHostSoftReboot || !h.matches(op) {
			continue
		}

		target := operationTargetStatus(&op.Status, h.reference)
		if target == nil || target.Phase != v1alpha3.OperationPhaseInProgress || target.BootID == "" {
			continue
		}

		if bootID == "" {
			id, err := h.host.BootID()
			if err != nil {
				return err
			}

			bootID = id
		}

		result := MachineOperationResult[int64]{
			Phase:                     v1alpha3.OperationPhaseComplete,
			Reason:                    "Succeeded",
			Message:                   "HostSoftReboot completed",
			ObservedMachineGeneration: target.ObservedGeneration,
		}
		if target.BootID == bootID {
			result = MachineOperationResult[int64]{
				Phase:   v1alpha3.OperationPhaseFailed,
				Reason:  "RebootNotObserved",
				Message: "host did not reboot after HostSoftReboot was issued",
			}
		}

		if err := h.finish(ctx, op.Name, result); err != nil {
			errs = append(errs, fmt.Errorf("report HostSoftReboot %s: %w", op.Name, err))
			continue
		}

		h.log.Info("reported HostSoftReboot", "operation", op.Name, "phase", result.Phase)
	}

	return errors.Join(errs...)
}
