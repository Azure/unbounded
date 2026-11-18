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
	"k8s.io/apimachinery/pkg/labels"
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

// HostSoftRebootTarget resolves the identity and selector labels of the local target.
// Reference is the machineRef used by operations, not necessarily a Machine CR.
// MatchSelector receives a compiled selector and the operation kind so the
// target can select the appropriate live labels for each kind.
type HostSoftRebootTarget interface {
	Reference() string
	ObservedGeneration(context.Context, client.Reader) (int64, error)
	MatchSelector(context.Context, client.Reader, v1alpha3.OperationKind, labels.Selector) (bool, error)
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

// Reconcile handles one HostSoftReboot. The caller must hold its mutation lock.
// Acknowledged status recording is required before the host command runs.
func (h *HostSoftRebootHandler) Reconcile(ctx context.Context, op MachineOperation) (ctrl.Result, error) {
	if h.requested.Load() {
		return ctrl.Result{}, nil
	}

	var latest v1alpha3.MachineOperation
	if err := h.reader.Get(ctx, client.ObjectKey{Name: op.Name}, &latest); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	matches, err := h.pendingMatch(ctx, &latest)
	if err != nil || !matches {
		return ctrl.Result{}, err
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
	if err == nil && strings.TrimSpace(bootID) == "" {
		err = errors.New("read boot ID: empty boot ID")
	}

	if err != nil {
		return ctrl.Result{}, h.failBootRead(ctx, op.Name, err)
	}

	started, err := h.start(ctx, op.Name, bootID)
	if err != nil || !started {
		return ctrl.Result{}, err
	}

	h.requested.Store(true)
	h.log.Info("rebooting host for HostSoftReboot", "operation", op.Name, "bootID", bootID)

	if err := h.host.RebootHost(ctx, h.log); err != nil {
		if ctx.Err() != nil {
			// Shutdown may be the reboot itself; the next daemon judges the boot ID.
			h.log.Warn("daemon stopping while rebooting host; the next daemon reports HostSoftReboot",
				"operation", op.Name, "error", err)

			return ctrl.Result{}, nil
		}

		h.requested.Store(false)

		return ctrl.Result{}, h.finish(ctx, op.Name, bootID, MachineOperationResult[int64]{
			Phase:   v1alpha3.OperationPhaseFailed,
			Reason:  "Failed",
			Message: err.Error(),
		})
	}

	return ctrl.Result{}, nil
}

func operationPending(phase v1alpha3.OperationPhase) bool {
	return phase == "" || phase == v1alpha3.OperationPhasePending
}

func operationTargetStatus(status *v1alpha3.MachineOperationStatus, ref string) *v1alpha3.MachineOperationTargetStatus {
	for i := range status.Targets {
		if status.Targets[i].MachineRef == ref {
			return &status.Targets[i]
		}
	}

	return nil
}

func (h *HostSoftRebootHandler) matches(ctx context.Context, op *v1alpha3.MachineOperation) (bool, error) {
	if op.Spec.MachineRef != "" {
		return op.Spec.MachineRef == h.reference, nil
	}

	if op.Spec.MachineSelector == nil {
		return false, nil
	}

	selector, err := metav1.LabelSelectorAsSelector(op.Spec.MachineSelector)
	if err != nil {
		return false, nil
	}

	return h.target.MatchSelector(ctx, h.reader, op.Spec.OperationKind, selector)
}

func selectorOperation(op *v1alpha3.MachineOperation) bool {
	return op.Spec.MachineRef == "" && op.Spec.MachineSelector != nil
}

func (h *HostSoftRebootHandler) pendingMatch(ctx context.Context, op *v1alpha3.MachineOperation) (bool, error) {
	if op.Spec.OperationKind != v1alpha3.OperationHostSoftReboot {
		return false, nil
	}

	// A peer may have started a selector request; this target must still be unstarted.
	if !operationPending(op.Status.Phase) && (!selectorOperation(op) || op.Status.Phase != v1alpha3.OperationPhaseInProgress) {
		return false, nil
	}

	// Never overwrite boot evidence after a start, including an uncertain write.

	if target := operationTargetStatus(&op.Status, h.reference); target != nil && !operationPending(target.Phase) {
		return false, nil
	}

	return h.matches(ctx, op)
}

func (h *HostSoftRebootHandler) start(ctx context.Context, name, bootID string) (bool, error) {
	started := false

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var op v1alpha3.MachineOperation
		if err := h.reader.Get(ctx, client.ObjectKey{Name: name}, &op); err != nil {
			return client.IgnoreNotFound(err)
		}

		matches, err := h.pendingMatch(ctx, &op)
		if err != nil || !matches {
			return err
		}

		generation, err := h.target.ObservedGeneration(ctx, h.reader)
		if err != nil {
			return err
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

func (h *HostSoftRebootHandler) failBootRead(ctx context.Context, name string, cause error) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var op v1alpha3.MachineOperation
		if err := h.reader.Get(ctx, client.ObjectKey{Name: name}, &op); err != nil {
			return client.IgnoreNotFound(err)
		}

		matches, err := h.pendingMatch(ctx, &op)
		if err != nil || !matches {
			return err
		}

		// A boot read failure retains the existing aggregate ExecutionFailed result,
		// even when a selector peer has already started.
		SetMachineOperationResult(&op, MachineOperationResult[int64]{
			Phase:   v1alpha3.OperationPhaseFailed,
			Reason:  "ExecutionFailed",
			Message: cause.Error(),
		}, metav1.Now())

		return h.client.Status().Update(ctx, &op)
	})
}

func (h *HostSoftRebootHandler) finish(ctx context.Context, name, bootID string, result MachineOperationResult[int64]) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var op v1alpha3.MachineOperation
		if err := h.reader.Get(ctx, client.ObjectKey{Name: name}, &op); err != nil {
			return client.IgnoreNotFound(err)
		}

		if op.Spec.OperationKind != v1alpha3.OperationHostSoftReboot {
			return nil
		}

		// A selector's peer can finish the aggregate before this issued target reports.
		// A terminal single-reference operation remains immutable.
		if op.Status.IsTerminal() && !selectorOperation(&op) {
			return nil
		}

		target := operationTargetStatus(&op.Status, h.reference)
		// A stale startup list or command result must not finish newer evidence.
		if target == nil || target.Phase != v1alpha3.OperationPhaseInProgress || target.BootID != bootID {
			return nil
		}

		if result.Phase == v1alpha3.OperationPhaseComplete {
			result.ObservedMachineGeneration = target.ObservedGeneration
		}

		now := metav1.Now()
		if !op.Status.IsTerminal() {
			SetMachineOperationResult(&op, result, now)
		}

		target.Phase = result.Phase
		target.Message = result.Message
		target.CompletedAt = &now

		return h.client.Status().Update(ctx, &op)
	})
}

func isOutOfBandHostOperation(kind v1alpha3.OperationKind) bool {
	return strings.HasPrefix(string(kind), "Host") && kind != v1alpha3.OperationHostSoftReboot && !kind.IsAllocationOperation()
}

func operationBefore(a, b *v1alpha3.MachineOperation) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
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

func (h *HostSoftRebootHandler) hostOperationUnfinishedOn(ctx context.Context, op *v1alpha3.MachineOperation) (bool, error) {
	target := operationTargetStatus(&op.Status, h.reference)
	// Cleanup or an unconfirmed media boot can outlast a terminal operation.
	if target != nil && target.HasOutstandingBootMedia() {
		return true, nil
	}

	if op.Status.IsTerminal() {
		return false, nil
	}
	// Resolved targets are authoritative over the original selector.
	if len(op.Status.Targets) > 0 {
		return target != nil && target.Phase != v1alpha3.OperationPhaseComplete && target.Phase != v1alpha3.OperationPhaseFailed, nil
	}

	return h.matches(ctx, op)
}

func (h *HostSoftRebootHandler) markWaiting(ctx context.Context, name, message string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var op v1alpha3.MachineOperation
		if err := h.reader.Get(ctx, client.ObjectKey{Name: name}, &op); err != nil {
			return client.IgnoreNotFound(err)
		}

		matches, err := h.pendingMatch(ctx, &op)
		if err != nil || !matches {
			return err
		}

		if !operationPending(op.Status.Phase) {
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
	if err := h.reader.List(ctx, &ops); err != nil {
		return fmt.Errorf("list MachineOperations: %w", err)
	}

	var (
		bootID string
		errs   []error
	)

	for i := range ops.Items {
		op := &ops.Items[i]
		if op.Spec.OperationKind != v1alpha3.OperationHostSoftReboot || (op.Status.IsTerminal() && !selectorOperation(op)) {
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

			if strings.TrimSpace(id) == "" {
				return errors.New("read boot ID: empty boot ID")
			}

			bootID = id
		}

		result := MachineOperationResult[int64]{
			Phase:   v1alpha3.OperationPhaseComplete,
			Reason:  "Succeeded",
			Message: "HostSoftReboot completed",
		}
		if target.BootID == bootID {
			result = MachineOperationResult[int64]{
				Phase:   v1alpha3.OperationPhaseFailed,
				Reason:  "RebootNotObserved",
				Message: "host did not reboot after HostSoftReboot was issued",
			}
		}

		if err := h.finish(ctx, op.Name, target.BootID, result); err != nil {
			errs = append(errs, fmt.Errorf("report HostSoftReboot %s: %w", op.Name, err))
			continue
		}

		h.log.Info("reported HostSoftReboot", "operation", op.Name, "phase", result.Phase)
	}

	return errors.Join(errs...)
}
