// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package machineops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/redfish"
)

const (
	siteLabel = "unbounded-cloud.io/site"

	reasonSucceeded                = "Succeeded"
	reasonInvalidTargetScope       = "InvalidTargetScope"
	reasonNoMatchingOwnedMachines  = "NoMatchingOwnedMachines"
	reasonMachineNotFound          = "MachineNotFound"
	reasonUnsupportedTarget        = "UnsupportedTarget"
	reasonTargetNoLongerOwned      = "TargetNoLongerOwned"
	reasonExecutionFailed          = "ExecutionFailed"
	reasonWaitingForOlderOperation = "WaitingForOlderOperation"
	reasonTimedOut                 = "TimedOut"

	cloudInitTimeout = 5 * time.Minute
)

// PowerClient is the Redfish power operation subset used by MachineOperation reconciliation.
type PowerClient interface {
	PowerState(ctx context.Context) (redfish.PowerState, error)
	Reset(ctx context.Context, resetType redfish.ResetType) error
	DisableBootOverride(ctx context.Context) error
	SetBootOverride(ctx context.Context, target redfish.BootTarget, enabled redfish.BootEnabled) error
	GetBootConfig(ctx context.Context) (redfish.BootConfig, error)
	SetStaticIPv4(ctx context.Context, config redfish.StaticIPv4Config) error
	SetHTTPBootOverride(ctx context.Context, bootURL string) error
	SetBIOSStaticIPv4(ctx context.Context, config redfish.StaticIPv4Config) error
	SetBIOSHTTPBootURI(ctx context.Context, bootURL string) error
}

// PowerClientFactory builds a PowerClient for a Machine.
type PowerClientFactory interface {
	ForMachine(ctx context.Context, machine *v1alpha3.Machine) (PowerClient, error)
}

// Reconciler reconciles metalman-owned host MachineOperations.
type Reconciler struct {
	client.Client
	APIReader client.Reader

	Site                  string
	PowerClients          PowerClientFactory
	HTTPBootURL           func(*v1alpha3.Machine) (string, error)
	MaxConcurrentMachines int
	MaxAttempts           int32
	PollInterval          time.Duration
	PowerActionTimeout    time.Duration
	Now                   func() metav1.Time
}

// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machineoperations,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machineoperations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machines,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("metalman-machineoperation").
		For(&v1alpha3.MachineOperation{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				op, ok := e.Object.(*v1alpha3.MachineOperation)
				return ok && shouldReconcile(op)
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				op, ok := e.ObjectNew.(*v1alpha3.MachineOperation)
				return ok && shouldReconcile(op)
			},
			DeleteFunc: func(e event.DeleteEvent) bool { return false },
			GenericFunc: func(e event.GenericEvent) bool {
				op, ok := e.Object.(*v1alpha3.MachineOperation)
				return ok && shouldReconcile(op)
			},
		}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

func shouldReconcile(op *v1alpha3.MachineOperation) bool {
	if isHostOperation(op.Spec.OperationKind) {
		return true
	}

	return op.Status.IsTerminal() && op.Spec.TTLSecondsAfterFinished != nil && op.Status.CompletedAt != nil
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := slog.With("machineOperation", req.Name)

	var op v1alpha3.MachineOperation
	if err := r.Get(ctx, client.ObjectKey{Name: req.Name}, &op); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if op.Status.IsTerminal() {
		return r.reconcileTerminal(ctx, &op)
	}

	if !isHostOperation(op.Spec.OperationKind) {
		return ctrl.Result{}, nil
	}

	if older, err := r.olderConflictingOperation(ctx, &op); err != nil {
		return ctrl.Result{}, err
	} else if older != "" {
		message := fmt.Sprintf("waiting for older host operation %s", older)

		return ctrl.Result{RequeueAfter: r.pollInterval()}, r.updateOperationStatus(ctx, op.Name, func(latest *v1alpha3.MachineOperation) {
			latest.Status.Phase = v1alpha3.OperationPhasePending
			latest.Status.Message = message
		})
	}

	if len(op.Status.Targets) == 0 {
		owned, err := r.snapshotTargets(ctx, &op)
		if err != nil {
			return ctrl.Result{}, err
		}

		if !owned {
			return ctrl.Result{}, nil
		}

		if err := r.Get(ctx, client.ObjectKey{Name: op.Name}, &op); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}

	if len(op.Status.Targets) == 0 {
		logger.DebugContext(ctx, "operation has no targets", "operation", op.Name)
		return ctrl.Result{}, nil
	}

	restoreHostReplaceTriggerConditions := op.Spec.OperationKind == v1alpha3.OperationHostReplace && hostReplaceTriggerConditionsMissing(&op)

	changes, requeueAfter := r.advanceTargets(ctx, &op)
	if len(changes) > 0 || restoreHostReplaceTriggerConditions {
		if err := r.applyTargetChanges(ctx, op.Name, changes, restoreHostReplaceTriggerConditions); err != nil {
			return ctrl.Result{}, err
		}
	}

	if requeueAfter > 0 {
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	return ctrl.Result{}, nil
}

func (r *Reconciler) snapshotTargets(ctx context.Context, op *v1alpha3.MachineOperation) (bool, error) {
	targets, err := r.resolveTargets(ctx, op)
	if err != nil {
		return true, r.finishOperation(ctx, op.Name, v1alpha3.OperationPhaseFailed, reasonForError(err), err.Error())
	}

	if len(targets) == 0 {
		if op.Spec.MachineRef != "" {
			return false, nil
		}

		return true, r.finishOperation(ctx, op.Name, v1alpha3.OperationPhaseFailed, reasonNoMatchingOwnedMachines, "no owned bare-metal Machines matched the operation target")
	}

	now := r.now()

	targetStatuses := make([]v1alpha3.MachineOperationTargetStatus, 0, len(targets))
	for _, machine := range targets {
		targetStatuses = append(targetStatuses, v1alpha3.MachineOperationTargetStatus{
			MachineRef:         machine.Name,
			Phase:              v1alpha3.OperationPhasePending,
			Message:            "target snapshotted",
			ObservedGeneration: machine.Generation,
		})
	}

	return true, r.updateOperationStatus(ctx, op.Name, func(latest *v1alpha3.MachineOperation) {
		latest.Status.Phase = v1alpha3.OperationPhaseInProgress
		latest.Status.Message = fmt.Sprintf("snapshotted %d target(s)", len(targetStatuses))
		latest.Status.StartedAt = &now
		latest.Status.Targets = targetStatuses
		setCompletedCondition(latest, metav1.ConditionFalse, "InProgress", latest.Status.Message)

		if op.Spec.OperationKind == v1alpha3.OperationHostReplace {
			setHostReplaceTriggerConditions(latest)
		}
	})
}

func (r *Reconciler) resolveTargets(ctx context.Context, op *v1alpha3.MachineOperation) ([]v1alpha3.Machine, error) {
	if op.Spec.MachineRef != "" {
		reader := r.APIReader
		if reader == nil {
			reader = r.Client
		}

		var machine v1alpha3.Machine
		if err := reader.Get(ctx, client.ObjectKey{Name: op.Spec.MachineRef}, &machine); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, operationError{reason: reasonMachineNotFound, message: fmt.Sprintf("Machine %s not found", op.Spec.MachineRef)}
			}

			return nil, fmt.Errorf("get Machine %s: %w", op.Spec.MachineRef, err)
		}

		if !r.ownsMachine(&machine) {
			return nil, nil
		}

		if machine.Spec.Provider != "" || machine.Spec.ProviderID != "" {
			return nil, nil
		}

		if !isBareMetalRedfishMachine(&machine) {
			return nil, operationError{reason: reasonUnsupportedTarget, message: fmt.Sprintf("Machine %s is not a bare-metal Redfish target", machine.Name)}
		}

		return []v1alpha3.Machine{machine}, nil
	}

	if op.Spec.MachineSelector == nil {
		return nil, operationError{reason: reasonInvalidTargetScope, message: "spec.machineRef or spec.machineSelector is required"}
	}

	if err := r.validateSelectorScope(op.Spec.MachineSelector); err != nil {
		return nil, err
	}

	selector, err := metav1.LabelSelectorAsSelector(op.Spec.MachineSelector)
	if err != nil {
		return nil, operationError{reason: reasonInvalidTargetScope, message: fmt.Sprintf("invalid machineSelector: %v", err)}
	}

	var list v1alpha3.MachineList
	if err := r.List(ctx, &list, client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, fmt.Errorf("list Machines for selector: %w", err)
	}

	targets := make([]v1alpha3.Machine, 0, len(list.Items))
	for _, machine := range list.Items {
		if !r.ownsMachine(&machine) || !isBareMetalRedfishMachine(&machine) {
			continue
		}

		targets = append(targets, machine)
	}

	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })

	return targets, nil
}

func (r *Reconciler) validateSelectorScope(selector *metav1.LabelSelector) error {
	if r.Site == "" {
		return operationError{reason: reasonInvalidTargetScope, message: "selector-based host operations require a non-empty metalman site"}
	}

	if selector.MatchLabels != nil && selector.MatchLabels[siteLabel] == r.Site {
		return nil
	}

	for _, expr := range selector.MatchExpressions {
		if expr.Key == siteLabel && expr.Operator == metav1.LabelSelectorOpIn && len(expr.Values) == 1 && expr.Values[0] == r.Site {
			return nil
		}
	}

	return operationError{reason: reasonInvalidTargetScope, message: fmt.Sprintf("machineSelector must include %s=%s", siteLabel, r.Site)}
}

func (r *Reconciler) advanceTargets(ctx context.Context, op *v1alpha3.MachineOperation) ([]targetChange, time.Duration) {
	maxConcurrent := r.maxConcurrentMachines()
	active := 0
	selected := make([]v1alpha3.MachineOperationTargetStatus, 0, maxConcurrent)

	for _, target := range op.Status.Targets {
		if target.Phase == v1alpha3.OperationPhaseInProgress {
			active++

			selected = append(selected, target)
		}
	}

	if active < maxConcurrent {
		for _, target := range op.Status.Targets {
			if target.Phase != "" && target.Phase != v1alpha3.OperationPhasePending {
				continue
			}

			if active >= maxConcurrent {
				break
			}

			target.Phase = v1alpha3.OperationPhaseInProgress
			selected = append(selected, target)
			active++
		}
	}

	if len(selected) == 0 {
		return []targetChange{{aggregateOnly: true}}, 0
	}

	changes := make([]targetChange, len(selected))

	var wg sync.WaitGroup
	for i, target := range selected {
		wg.Add(1)

		go func(i int, target v1alpha3.MachineOperationTargetStatus) {
			defer wg.Done()

			changes[i] = r.advanceTarget(ctx, op, target)
		}(i, target)
	}

	wg.Wait()

	requeue := r.pollInterval()

	return changes, requeue
}

func (r *Reconciler) advanceTarget(ctx context.Context, op *v1alpha3.MachineOperation, target v1alpha3.MachineOperationTargetStatus) targetChange {
	now := r.now()
	if target.StartedAt == nil {
		target.StartedAt = &now
	}

	var machine v1alpha3.Machine
	if err := r.Get(ctx, client.ObjectKey{Name: target.MachineRef}, &machine); err != nil {
		if apierrors.IsNotFound(err) {
			return failTarget(target, reasonMachineNotFound, fmt.Sprintf("Machine %s not found", target.MachineRef), now)
		}

		return retryTarget(target, fmt.Errorf("get Machine %s: %w", target.MachineRef, err), now, r.maxAttempts())
	}

	if !r.ownsMachine(&machine) {
		return failTarget(target, reasonTargetNoLongerOwned, fmt.Sprintf("Machine %s is no longer owned by this metalman instance", machine.Name), now)
	}

	if !isBareMetalRedfishMachine(&machine) {
		return failTarget(target, reasonUnsupportedTarget, fmt.Sprintf("Machine %s is not a bare-metal Redfish target", machine.Name), now)
	}

	if op.Spec.OperationKind != v1alpha3.OperationHostReplace || target.ObservedGeneration == 0 {
		target.ObservedGeneration = machine.Generation
	}

	switch op.Spec.OperationKind {
	case v1alpha3.OperationHostPowerOff:
		return r.advancePowerOff(ctx, &machine, target, now)
	case v1alpha3.OperationHostPowerOn:
		return r.advancePowerOn(ctx, op, &machine, target, now)
	case v1alpha3.OperationHostReboot:
		return r.advanceReboot(ctx, op, &machine, target, now)
	case v1alpha3.OperationHostReplace:
		return r.advanceReplace(ctx, op, &machine, target, now)
	default:
		return failTarget(target, reasonUnsupportedTarget, fmt.Sprintf("%s is not handled by metalman", op.Spec.OperationKind), now)
	}
}

func (r *Reconciler) advancePowerOff(ctx context.Context, machine *v1alpha3.Machine, target v1alpha3.MachineOperationTargetStatus, now metav1.Time) targetChange {
	pc, err := r.PowerClients.ForMachine(ctx, machine)
	if err != nil {
		return retryTarget(target, err, now, r.maxAttempts())
	}

	state, err := pc.PowerState(ctx)
	if err != nil {
		return retryTarget(target, err, now, r.maxAttempts())
	}

	if state == redfish.PowerOff {
		return completeTarget(target, "HostPowerOff completed", now)
	}

	if change, handled := r.waitForPowerAction(target, v1alpha3.OperationStageWaitingOff, "waiting for power off", "timed out waiting for power off", now); handled {
		return change
	}

	if err := pc.Reset(ctx, redfish.ResetForceOff); err != nil {
		return retryTarget(target, err, now, r.maxAttempts())
	}

	target.Stage = v1alpha3.OperationStageWaitingOff
	target.Attempts++
	target.LastAttemptAt = &now
	target.Message = "sent ForceOff"

	return targetChange{target: target}
}

func (r *Reconciler) advancePowerOn(ctx context.Context, op *v1alpha3.MachineOperation, machine *v1alpha3.Machine, target v1alpha3.MachineOperationTargetStatus, now metav1.Time) targetChange {
	pc, err := r.PowerClients.ForMachine(ctx, machine)
	if err != nil {
		return retryTarget(target, err, now, r.maxAttempts())
	}

	if err := disableBootOverride(ctx, pc, &target, op.Generation, now); err != nil {
		return retryTarget(target, err, now, r.maxAttempts())
	}

	state, err := pc.PowerState(ctx)
	if err != nil {
		return retryTarget(target, err, now, r.maxAttempts())
	}

	if state == redfish.PowerOn {
		return completeTarget(target, "HostPowerOn completed", now)
	}

	if change, handled := r.waitForPowerAction(target, v1alpha3.OperationStageWaitingOn, "waiting for power on", "timed out waiting for power on", now); handled {
		return change
	}

	if err := pc.Reset(ctx, redfish.ResetOn); err != nil {
		return retryTarget(target, err, now, r.maxAttempts())
	}

	target.Stage = v1alpha3.OperationStageWaitingOn
	target.Attempts++
	target.LastAttemptAt = &now
	target.Message = "sent On"

	return targetChange{target: target}
}

func (r *Reconciler) advanceReboot(ctx context.Context, op *v1alpha3.MachineOperation, machine *v1alpha3.Machine, target v1alpha3.MachineOperationTargetStatus, now metav1.Time) targetChange {
	pc, err := r.PowerClients.ForMachine(ctx, machine)
	if err != nil {
		return retryTarget(target, err, now, r.maxAttempts())
	}

	if err := disableBootOverride(ctx, pc, &target, op.Generation, now); err != nil {
		return retryTarget(target, err, now, r.maxAttempts())
	}

	state, err := pc.PowerState(ctx)
	if err != nil {
		return retryTarget(target, err, now, r.maxAttempts())
	}

	switch target.Stage {
	case "", v1alpha3.OperationStagePoweringOff, v1alpha3.OperationStageWaitingOff:
		if state == redfish.PowerOff {
			target.Stage = v1alpha3.OperationStagePoweringOn
			return targetChange{target: target}
		}

		if change, handled := r.waitForPowerAction(target, v1alpha3.OperationStageWaitingOff, "waiting for power off", "timed out waiting for reboot power off", now); handled {
			return change
		}

		if err := pc.Reset(ctx, redfish.ResetForceOff); err != nil {
			return retryTarget(target, err, now, r.maxAttempts())
		}

		target.Stage = v1alpha3.OperationStageWaitingOff
		target.Attempts++
		target.LastAttemptAt = &now
		target.Message = "sent ForceOff"

		return targetChange{target: target}

	case v1alpha3.OperationStagePoweringOn, v1alpha3.OperationStageWaitingOn:
		if state == redfish.PowerOn {
			return completeTarget(target, "HostReboot completed", now)
		}

		if change, handled := r.waitForPowerAction(target, v1alpha3.OperationStageWaitingOn, "waiting for power on", "timed out waiting for reboot power on", now); handled {
			return change
		}

		if err := pc.Reset(ctx, redfish.ResetOn); err != nil {
			return retryTarget(target, err, now, r.maxAttempts())
		}

		target.Stage = v1alpha3.OperationStageWaitingOn
		target.Attempts++
		target.LastAttemptAt = &now
		target.Message = "sent On"

		return targetChange{target: target}

	default:
		return failTarget(target, reasonExecutionFailed, fmt.Sprintf("unknown stage %s", target.Stage), now)
	}
}

func disableBootOverride(ctx context.Context, pc PowerClient, target *v1alpha3.MachineOperationTargetStatus, observedGeneration int64, now metav1.Time) error {
	if apimeta.IsStatusConditionTrue(target.Conditions, v1alpha3.MachineOperationTargetConditionRedfishDisableBootOverrideUnsupported) {
		return pc.SetBootOverride(ctx, redfish.BootTargetHdd, redfish.BootContinuous)
	}

	if err := pc.DisableBootOverride(ctx); err != nil {
		if !errors.Is(err, redfish.ErrUnsupported) {
			return err
		}

		apimeta.SetStatusCondition(&target.Conditions, metav1.Condition{
			Type:               v1alpha3.MachineOperationTargetConditionRedfishDisableBootOverrideUnsupported,
			Status:             metav1.ConditionTrue,
			Reason:             "Unsupported",
			Message:            "BMC does not support disabling Redfish boot override; falling back to Hdd/Continuous",
			ObservedGeneration: observedGeneration,
			LastTransitionTime: now,
		})

		return pc.SetBootOverride(ctx, redfish.BootTargetHdd, redfish.BootContinuous)
	}

	return nil
}

func (r *Reconciler) waitForPowerAction(target v1alpha3.MachineOperationTargetStatus, stage v1alpha3.OperationStage, waitingMessage, timeoutMessage string, now metav1.Time) (targetChange, bool) {
	if target.Stage != stage || target.LastAttemptAt == nil {
		return targetChange{}, false
	}

	if now.Sub(target.LastAttemptAt.Time) < r.powerActionTimeout() {
		target.Message = waitingMessage
		return targetChange{target: target}, true
	}

	if target.Attempts >= r.maxAttempts() {
		return failTarget(target, reasonExecutionFailed, fmt.Sprintf("%s after %d attempts", timeoutMessage, target.Attempts), now), true
	}

	return targetChange{}, false
}

func (r *Reconciler) advanceReplace(ctx context.Context, op *v1alpha3.MachineOperation, machine *v1alpha3.Machine, target v1alpha3.MachineOperationTargetStatus, now metav1.Time) targetChange {
	if !apimeta.IsStatusConditionTrue(op.Status.Conditions, v1alpha3.MachineOperationConditionBootImageWritten) {
		return r.waitForRepaveBoot(ctx, machine, target, now)
	}

	if change, done := cloudInitReplaceStatus(op, target, now); done {
		return change
	}

	nodeName := nodeNameForMachine(machine)

	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			target.Stage = v1alpha3.OperationStageWaitingNode
			target.Message = fmt.Sprintf("waiting for Node %s to exist", nodeName)

			return targetChange{target: target}
		}

		return targetChange{target: target, err: fmt.Errorf("get Node %s: %w", nodeName, err)}
	}

	return completeTarget(target, "HostReplace completed", now)
}

func (r *Reconciler) configureRepaveBoot(ctx context.Context, pc PowerClient, machine *v1alpha3.Machine) error {
	if machine.Spec.PXE.TargetBootProtocol() == v1alpha3.PXEBootProtocolHTTP {
		bootURL, staticConfig, err := r.httpBootConfig(machine)
		if err != nil {
			return err
		}

		if err := setHTTPBootOverride(ctx, pc, bootURL, staticConfig); err != nil {
			return err
		}
	} else if err := pc.SetBootOverride(ctx, redfish.BootTargetPxe, redfish.BootContinuous); err != nil {
		return err
	}

	return nil
}

func (r *Reconciler) httpBootConfig(machine *v1alpha3.Machine) (string, redfish.StaticIPv4Config, error) {
	if r.HTTPBootURL == nil {
		return "", redfish.StaticIPv4Config{}, fmt.Errorf("HTTP boot URL resolver is not configured")
	}

	bootURL, err := r.HTTPBootURL(machine)
	if err != nil {
		return "", redfish.StaticIPv4Config{}, err
	}

	staticConfig, err := httpBootStaticNetworkConfig(machine)
	if err != nil {
		return "", redfish.StaticIPv4Config{}, err
	}

	return bootURL, staticConfig, nil
}

func httpBootStaticNetworkConfig(machine *v1alpha3.Machine) (redfish.StaticIPv4Config, error) {
	if machine.Spec.PXE == nil || len(machine.Spec.PXE.DHCPLeases) == 0 {
		return redfish.StaticIPv4Config{}, fmt.Errorf("HTTP boot requires at least one static lease in spec.pxe.dhcpLeases")
	}

	lease := machine.Spec.PXE.DHCPLeases[0]
	config := redfish.StaticIPv4Config{
		MAC:        lease.MAC,
		Address:    lease.IPv4,
		SubnetMask: lease.SubnetMask,
		Gateway:    lease.Gateway,
		DNS:        lease.DNS,
	}

	if _, err := net.ParseMAC(config.MAC); err != nil {
		return redfish.StaticIPv4Config{}, fmt.Errorf("invalid HTTP boot MAC address %q: %w", config.MAC, err)
	}

	if err := redfish.ValidateStaticIPv4Config(config); err != nil {
		return redfish.StaticIPv4Config{}, err
	}

	return config, nil
}

func setHTTPBootOverride(ctx context.Context, pc PowerClient, bootURL string, staticConfig redfish.StaticIPv4Config) error {
	config, err := pc.GetBootConfig(ctx)
	if err != nil {
		return err
	}

	if !config.HasHTTPBootURI {
		return setBIOSHTTPBootOverride(ctx, pc, bootURL, staticConfig)
	}

	if err := pc.SetStaticIPv4(ctx, staticConfig); err != nil {
		if !errors.Is(err, redfish.ErrUnsupported) {
			return err
		}

		return setBIOSHTTPBootOverride(ctx, pc, bootURL, staticConfig)
	}

	if err := pc.SetHTTPBootOverride(ctx, bootURL); err != nil {
		if !errors.Is(err, redfish.ErrUnsupported) {
			return err
		}

		return setBIOSHTTPBootOverride(ctx, pc, bootURL, staticConfig)
	}

	// Some BMCs expose both locations but boot from the vendor BIOS setting.
	if err := pc.SetBIOSStaticIPv4(ctx, staticConfig); err != nil && !errors.Is(err, redfish.ErrUnsupported) {
		return err
	}

	if err := pc.SetBIOSHTTPBootURI(ctx, bootURL); err != nil && !errors.Is(err, redfish.ErrUnsupported) {
		return err
	}

	return nil
}

func setBIOSHTTPBootOverride(ctx context.Context, pc PowerClient, bootURL string, staticConfig redfish.StaticIPv4Config) error {
	if err := pc.SetBIOSStaticIPv4(ctx, staticConfig); err != nil {
		return err
	}

	if err := pc.SetBIOSHTTPBootURI(ctx, bootURL); err != nil {
		return err
	}

	return pc.SetBootOverride(ctx, redfish.BootTargetUefiHTTP, redfish.BootOnce)
}

func (r *Reconciler) waitForRepaveBoot(ctx context.Context, machine *v1alpha3.Machine, target v1alpha3.MachineOperationTargetStatus, now metav1.Time) targetChange {
	if target.Stage == v1alpha3.OperationStageWaitingRepave && target.LastAttemptAt != nil {
		if now.Sub(target.LastAttemptAt.Time) < r.powerActionTimeout() {
			target.Message = "waiting for PXE installer to write the boot image"

			return targetChange{target: target}
		}

		if target.Attempts >= r.maxAttempts() {
			return failTarget(target, reasonExecutionFailed, fmt.Sprintf("timed out waiting for PXE repave after %d attempts", target.Attempts), now)
		}

		target.Stage = v1alpha3.OperationStagePoweringOff
		target.LastAttemptAt = nil
	}

	if machine.Spec.PXE.TargetBootProtocol() == v1alpha3.PXEBootProtocolHTTP {
		if _, _, err := r.httpBootConfig(machine); err != nil {
			return retryTarget(target, err, now, r.maxAttempts())
		}
	}

	pc, err := r.PowerClients.ForMachine(ctx, machine)
	if err != nil {
		return targetChange{target: target, err: err}
	}

	state, err := pc.PowerState(ctx)
	if err != nil {
		return targetChange{target: target, err: err}
	}

	if state != redfish.PowerOff {
		if target.Stage == v1alpha3.OperationStageWaitingOff && target.LastAttemptAt != nil {
			if now.Sub(target.LastAttemptAt.Time) < r.powerActionTimeout() {
				target.Message = "waiting for power off before configuring repave boot"

				return targetChange{target: target}
			}
		}

		if err := pc.Reset(ctx, redfish.ResetForceOff); err != nil {
			return targetChange{target: target, err: err}
		}

		target.Stage = v1alpha3.OperationStageWaitingOff
		target.LastAttemptAt = &now
		target.Message = "sent ForceOff before configuring repave boot"

		return targetChange{target: target}
	}

	if err := r.configureRepaveBoot(ctx, pc, machine); err != nil {
		return retryTarget(target, err, now, r.maxAttempts())
	}

	if err := pc.Reset(ctx, redfish.ResetOn); err != nil {
		return targetChange{target: target, err: err}
	}

	target.Stage = v1alpha3.OperationStageWaitingRepave
	target.Attempts++
	target.LastAttemptAt = &now
	target.Message = "requested PXE repave boot"

	return targetChange{target: target}
}

func cloudInitReplaceStatus(op *v1alpha3.MachineOperation, target v1alpha3.MachineOperationTargetStatus, now metav1.Time) (targetChange, bool) {
	cond := apimeta.FindStatusCondition(op.Status.Conditions, v1alpha3.MachineOperationConditionCloudInitDone)
	if cond != nil {
		switch cond.Status {
		case metav1.ConditionTrue:
			return targetChange{}, false
		case metav1.ConditionFalse:
			if cond.Reason == "Failed" {
				return failTarget(target, reasonExecutionFailed, cond.Message, now), true
			}

			if !cond.LastTransitionTime.IsZero() && now.Sub(cond.LastTransitionTime.Time) >= cloudInitTimeout {
				return failTarget(target, reasonTimedOut, fmt.Sprintf("cloud-init did not complete within %s", cloudInitTimeout), now), true
			}
		}
	}

	target.Stage = v1alpha3.OperationStageWaitingCloudInit
	target.Message = "waiting for first-boot cloud-init to complete"

	return targetChange{target: target}, true
}

func nodeNameForMachine(machine *v1alpha3.Machine) string {
	if machine.Spec.Kubernetes != nil && machine.Spec.Kubernetes.NodeRef != nil && machine.Spec.Kubernetes.NodeRef.Name != "" {
		return machine.Spec.Kubernetes.NodeRef.Name
	}

	return machine.Name
}

func (r *Reconciler) applyTargetChanges(ctx context.Context, opName string, changes []targetChange, restoreHostReplaceTriggerConditions bool) error {
	for _, change := range changes {
		if change.err != nil {
			return change.err
		}
	}

	return r.updateOperationStatus(ctx, opName, func(latest *v1alpha3.MachineOperation) {
		byName := map[string]v1alpha3.MachineOperationTargetStatus{}

		for _, change := range changes {
			if change.aggregateOnly || change.target.MachineRef == "" {
				continue
			}

			byName[change.target.MachineRef] = change.target
		}

		for i, target := range latest.Status.Targets {
			if updated, ok := byName[target.MachineRef]; ok {
				latest.Status.Targets[i] = updated
			}
		}

		if restoreHostReplaceTriggerConditions {
			setHostReplaceTriggerConditions(latest)
		}

		r.aggregateStatus(latest)
	})
}

func (r *Reconciler) aggregateStatus(op *v1alpha3.MachineOperation) {
	if len(op.Status.Targets) == 0 {
		return
	}

	var complete, failed, inProgress, pending int

	for _, target := range op.Status.Targets {
		switch target.Phase {
		case v1alpha3.OperationPhaseComplete:
			complete++
		case v1alpha3.OperationPhaseFailed:
			failed++
		case v1alpha3.OperationPhaseInProgress:
			inProgress++
		default:
			pending++
		}
	}

	message := fmt.Sprintf("targets complete=%d failed=%d inProgress=%d pending=%d", complete, failed, inProgress, pending)
	op.Status.Message = message

	if complete+failed == len(op.Status.Targets) {
		now := r.now()

		op.Status.CompletedAt = &now
		if failed > 0 {
			op.Status.Phase = v1alpha3.OperationPhaseFailed
			setCompletedCondition(op, metav1.ConditionFalse, "TargetFailed", message)

			return
		}

		op.Status.Phase = v1alpha3.OperationPhaseComplete
		setCompletedCondition(op, metav1.ConditionTrue, reasonSucceeded, message)

		return
	}

	op.Status.Phase = v1alpha3.OperationPhaseInProgress
	setCompletedCondition(op, metav1.ConditionFalse, "InProgress", message)
}

func (r *Reconciler) olderConflictingOperation(ctx context.Context, op *v1alpha3.MachineOperation) (string, error) {
	opKeys, err := r.operationConflictKeys(ctx, op)
	if err != nil {
		return "", err
	}

	if len(opKeys) == 0 {
		return "", nil
	}

	var list v1alpha3.MachineOperationList
	if err := r.List(ctx, &list); err != nil {
		return "", fmt.Errorf("list MachineOperations: %w", err)
	}

	candidates := make([]v1alpha3.MachineOperation, 0, len(list.Items))

	for _, candidate := range list.Items {
		if candidate.Name == op.Name || candidate.Status.IsTerminal() || !isHostOperation(candidate.Spec.OperationKind) {
			continue
		}

		if !operationBefore(&candidate, op) {
			continue
		}

		candidates = append(candidates, candidate)
	}

	sort.Slice(candidates, func(i, j int) bool { return operationBefore(&candidates[i], &candidates[j]) })

	for _, candidate := range candidates {
		candidateKeys, err := r.operationConflictKeys(ctx, &candidate)
		if err != nil {
			return "", err
		}

		if conflictSetsOverlap(opKeys, candidateKeys) {
			return candidate.Name, nil
		}
	}

	return "", nil
}

func (r *Reconciler) operationConflictKeys(ctx context.Context, op *v1alpha3.MachineOperation) (map[string]struct{}, error) {
	keys := map[string]struct{}{}

	if len(op.Status.Targets) > 0 {
		for _, target := range op.Status.Targets {
			if isTerminalTarget(target) {
				continue
			}

			if err := r.addTargetConflictKeys(ctx, keys, target.MachineRef); err != nil {
				return nil, err
			}
		}

		return keys, nil
	}

	targets, err := r.resolveTargets(ctx, op)
	if err != nil {
		var opErr operationError
		if errors.As(err, &opErr) {
			return keys, nil
		}

		return nil, err
	}

	for i := range targets {
		addMachineConflictKeys(keys, &targets[i])
	}

	return keys, nil
}

func (r *Reconciler) addTargetConflictKeys(ctx context.Context, keys map[string]struct{}, machineName string) error {
	if machineName == "" {
		return nil
	}

	keys["machine:"+machineName] = struct{}{}

	var machine v1alpha3.Machine
	if err := r.Get(ctx, client.ObjectKey{Name: machineName}, &machine); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return fmt.Errorf("get Machine %s for operation conflict detection: %w", machineName, err)
	}

	if !r.ownsMachine(&machine) {
		delete(keys, "machine:"+machineName)

		return nil
	}

	addMachineConflictKeys(keys, &machine)

	return nil
}

func addMachineConflictKeys(keys map[string]struct{}, machine *v1alpha3.Machine) {
	keys["machine:"+machine.Name] = struct{}{}

	if machine.Spec.PXE == nil || machine.Spec.PXE.Redfish == nil || machine.Spec.PXE.Redfish.URL == "" {
		return
	}

	keys["redfish:"+normalizeRedfishURL(machine.Spec.PXE.Redfish.URL)] = struct{}{}
}

func isTerminalTarget(target v1alpha3.MachineOperationTargetStatus) bool {
	return target.Phase == v1alpha3.OperationPhaseComplete || target.Phase == v1alpha3.OperationPhaseFailed
}

func conflictSetsOverlap(a, b map[string]struct{}) bool {
	if len(a) > len(b) {
		a, b = b, a
	}

	for key := range a {
		if _, ok := b[key]; ok {
			return true
		}
	}

	return false
}

func normalizeRedfishURL(raw string) string {
	return strings.TrimRight(raw, "/")
}

func operationBefore(a, b *v1alpha3.MachineOperation) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}

	return a.Name < b.Name
}

func (r *Reconciler) reconcileTerminal(ctx context.Context, op *v1alpha3.MachineOperation) (ctrl.Result, error) {
	if op.Spec.TTLSecondsAfterFinished == nil || op.Status.CompletedAt == nil {
		return ctrl.Result{}, nil
	}

	deadline := op.Status.CompletedAt.Add(time.Duration(*op.Spec.TTLSecondsAfterFinished) * time.Second)

	now := r.now().Time
	if now.Before(deadline) {
		return ctrl.Result{RequeueAfter: deadline.Sub(now)}, nil
	}

	if err := r.Delete(ctx, op); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	return ctrl.Result{}, nil
}

func (r *Reconciler) updateOperationStatus(ctx context.Context, name string, mutate func(*v1alpha3.MachineOperation)) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest v1alpha3.MachineOperation
		if err := r.Get(ctx, client.ObjectKey{Name: name}, &latest); err != nil {
			return err
		}

		mutate(&latest)

		return r.Status().Update(ctx, &latest)
	})
}

func (r *Reconciler) finishOperation(ctx context.Context, name string, phase v1alpha3.OperationPhase, reason, message string) error {
	now := r.now()

	return r.updateOperationStatus(ctx, name, func(latest *v1alpha3.MachineOperation) {
		if latest.Status.StartedAt == nil {
			latest.Status.StartedAt = &now
		}

		latest.Status.Phase = phase
		latest.Status.Message = message
		latest.Status.CompletedAt = &now

		conditionStatus := metav1.ConditionTrue
		if phase == v1alpha3.OperationPhaseFailed {
			conditionStatus = metav1.ConditionFalse
		}

		setCompletedCondition(latest, conditionStatus, reason, message)
	})
}

func setCompletedCondition(op *v1alpha3.MachineOperation, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&op.Status.Conditions, metav1.Condition{
		Type:               v1alpha3.MachineOperationConditionCompleted,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: op.Generation,
	})
}

func hostReplaceTriggerConditionsMissing(op *v1alpha3.MachineOperation) bool {
	return apimeta.FindStatusCondition(op.Status.Conditions, v1alpha3.MachineOperationConditionBootLoaderDownloaded) == nil ||
		apimeta.FindStatusCondition(op.Status.Conditions, v1alpha3.MachineOperationConditionBootImageWritten) == nil ||
		apimeta.FindStatusCondition(op.Status.Conditions, v1alpha3.MachineOperationConditionCloudInitDone) == nil
}

func setHostReplaceTriggerConditions(op *v1alpha3.MachineOperation) {
	setConditionIfMissing(op, v1alpha3.MachineOperationConditionBootLoaderDownloaded, "Pending", "waiting for initial boot loader download")
	setConditionIfMissing(op, v1alpha3.MachineOperationConditionBootImageWritten, "Pending", "waiting for PXE installer to finish writing the boot image")
	setConditionIfMissing(op, v1alpha3.MachineOperationConditionCloudInitDone, "Pending", "waiting for first-boot cloud-init to complete")
}

func setConditionIfMissing(op *v1alpha3.MachineOperation, conditionType, reason, message string) {
	if apimeta.FindStatusCondition(op.Status.Conditions, conditionType) != nil {
		return
	}

	apimeta.SetStatusCondition(&op.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             metav1.ConditionUnknown,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: op.Generation,
	})
}

func (r *Reconciler) ownsMachine(machine *v1alpha3.Machine) bool {
	if r.Site == "" {
		_, ok := machine.Labels[siteLabel]
		return !ok
	}

	return machine.Labels[siteLabel] == r.Site
}

func isBareMetalRedfishMachine(machine *v1alpha3.Machine) bool {
	return machine.Spec.Provider == "" && machine.Spec.ProviderID == "" && machine.Spec.PXE != nil && machine.Spec.PXE.Redfish != nil
}

func isHostOperation(operation v1alpha3.OperationKind) bool {
	switch operation {
	case v1alpha3.OperationHostReboot, v1alpha3.OperationHostPowerOff, v1alpha3.OperationHostPowerOn, v1alpha3.OperationHostReplace:
		return true
	default:
		return false
	}
}

type targetChange struct {
	target        v1alpha3.MachineOperationTargetStatus
	err           error
	aggregateOnly bool
}

func completeTarget(target v1alpha3.MachineOperationTargetStatus, message string, now metav1.Time) targetChange {
	target.Phase = v1alpha3.OperationPhaseComplete
	target.Message = message
	target.CompletedAt = &now

	return targetChange{target: target}
}

func failTarget(target v1alpha3.MachineOperationTargetStatus, reason, message string, now metav1.Time) targetChange {
	target.Phase = v1alpha3.OperationPhaseFailed
	target.Message = fmt.Sprintf("%s: %s", reason, message)
	target.CompletedAt = &now

	return targetChange{target: target}
}

func retryTarget(target v1alpha3.MachineOperationTargetStatus, err error, now metav1.Time, maxAttempts int32) targetChange {
	if target.Attempts >= maxAttempts {
		return failTarget(target, reasonExecutionFailed, err.Error(), now)
	}

	target.Attempts++
	target.LastAttemptAt = &now
	target.Message = err.Error()

	return targetChange{target: target}
}

type operationError struct {
	reason  string
	message string
}

func (e operationError) Error() string { return e.message }

func reasonForError(err error) string {
	var opErr operationError
	if errors.As(err, &opErr) {
		return opErr.reason
	}

	return reasonExecutionFailed
}

func (r *Reconciler) now() metav1.Time {
	if r.Now != nil {
		return r.Now()
	}

	return metav1.Now()
}

func (r *Reconciler) maxConcurrentMachines() int {
	if r.MaxConcurrentMachines <= 0 {
		return 1
	}

	return r.MaxConcurrentMachines
}

func (r *Reconciler) maxAttempts() int32 {
	if r.MaxAttempts <= 0 {
		return 3
	}

	return r.MaxAttempts
}

func (r *Reconciler) pollInterval() time.Duration {
	if r.PollInterval <= 0 {
		return 5 * time.Second
	}

	return r.PollInterval
}

func (r *Reconciler) powerActionTimeout() time.Duration {
	if r.PowerActionTimeout <= 0 {
		return 5 * time.Minute
	}

	return r.PowerActionTimeout
}
