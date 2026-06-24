// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package redfish

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/machinestatus"
)

const (
	// Condition types set by this controller.
	condPoweredOff    = "PoweredOff"
	condBootSupported = "BootOrderConfigSupported"
	condRepaved       = "Repaved"

	// Condition reasons.
	reasonPoweringOff  = "PoweringOff"
	reasonForceOff     = "ForceOff"
	reasonPoweringOn   = "PoweringOn"
	reasonNotSupported = "NotSupported"
	reasonPending      = "Pending"

	powerActionTimeout = 5 * time.Minute
)

type Reconciler struct {
	Client    client.Client
	APIReader client.Reader
	Pool      *Pool
	Recorder  events.EventRecorder
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("redfish").
		For(&v1alpha3.Machine{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				m, ok := e.Object.(*v1alpha3.Machine)
				return ok && m.Spec.PXE != nil && m.Spec.PXE.Redfish != nil
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				m, ok := e.ObjectNew.(*v1alpha3.Machine)
				return ok && m.Spec.PXE != nil && m.Spec.PXE.Redfish != nil
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				return false
			},
			GenericFunc: func(e event.GenericEvent) bool {
				m, ok := e.Object.(*v1alpha3.Machine)
				return ok && m.Spec.PXE != nil && m.Spec.PXE.Redfish != nil
			},
		}).
		Complete(r)
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := slog.With("node", req.Name, "namespace", req.Namespace)

	var machine v1alpha3.Machine
	if err := r.Client.Get(ctx, req.NamespacedName, &machine); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if machine.Spec.PXE == nil || machine.Spec.PXE.Redfish == nil {
		return ctrl.Result{}, nil
	}

	rf := machine.Spec.PXE.Redfish
	if machine.Spec.Operations == nil {
		machine.Spec.Operations = &v1alpha3.OperationsSpec{}
	}

	if machine.Status.Operations == nil {
		machine.Status.Operations = &v1alpha3.OperationsStatus{}
	}

	// TOFU: capture TLS cert fingerprint on first connection.
	fingerprint := ""
	if machine.Status.Redfish != nil {
		fingerprint = machine.Status.Redfish.CertFingerprint
	}

	if fingerprint == "" {
		if pending, err := r.redfishFingerprintPending(ctx, client.ObjectKeyFromObject(&machine), rf); err != nil {
			return ctrl.Result{}, fmt.Errorf("checking latest Redfish endpoint before fingerprint capture: %w", err)
		} else if !pending {
			return ctrl.Result{}, nil
		}

		fp, err := CaptureFingerprint(ctx, rf.URL)
		if err != nil {
			r.setRedfishReady(ctx, &machine, metav1.ConditionFalse, "FingerprintFailed", fmt.Sprintf("failed to capture Redfish TLS fingerprint from %s: %v", rf.URL, err), corev1.EventTypeWarning)
			return ctrl.Result{}, fmt.Errorf("capturing TLS cert fingerprint: %w", err)
		}

		if machine.Status.Redfish == nil {
			machine.Status.Redfish = &v1alpha3.RedfishStatus{}
		}

		machine.Status.Redfish.CertFingerprint = fp
		log.Info("TOFU: captured TLS cert fingerprint", "fingerprint", fp)

		return ctrl.Result{}, r.updateMachineStatus(ctx, client.ObjectKeyFromObject(&machine), func(latest *v1alpha3.Machine) bool {
			if !sameRedfishSpec(latest, rf) {
				return false
			}

			if latest.Status.Redfish == nil {
				latest.Status.Redfish = &v1alpha3.RedfishStatus{}
			}

			if latest.Status.Redfish.CertFingerprint != "" {
				return false
			}

			latest.Status.Redfish.CertFingerprint = fp

			return true
		})
	}

	// Retrieve Redfish password from Secret.
	var secret corev1.Secret
	if err := r.Client.Get(ctx, types.NamespacedName{
		Name:      rf.PasswordRef.Name,
		Namespace: rf.PasswordRef.Namespace,
	}, &secret); err != nil {
		r.setRedfishReady(ctx, &machine, metav1.ConditionFalse, "SecretGetFailed", fmt.Sprintf("failed to get Redfish password secret %s/%s: %v", rf.PasswordRef.Namespace, rf.PasswordRef.Name, err), corev1.EventTypeWarning)
		return ctrl.Result{}, fmt.Errorf("getting Redfish password secret: %w", err)
	}

	passwordBytes, ok := secret.Data[rf.PasswordRef.Key]
	if !ok {
		message := fmt.Sprintf("Redfish password secret %s/%s missing key %q", rf.PasswordRef.Namespace, rf.PasswordRef.Name, rf.PasswordRef.Key)
		r.setRedfishReady(ctx, &machine, metav1.ConditionFalse, "SecretKeyMissing", message, corev1.EventTypeWarning)

		return ctrl.Result{}, fmt.Errorf("%s", message)
	}

	password := string(passwordBytes)

	// Acquire Redfish client.
	c, err := r.Pool.Get(ctx, rf.URL, fingerprint, rf.Username, password, rf.DeviceID)
	if err != nil {
		r.setRedfishReady(ctx, &machine, metav1.ConditionFalse, "ClientFailed", fmt.Sprintf("failed to create Redfish client for %s: %v", rf.URL, err), corev1.EventTypeWarning)
		return ctrl.Result{}, fmt.Errorf("getting Redfish client: %w", err)
	}

	r.setRedfishReady(ctx, &machine, metav1.ConditionTrue, "Connected", fmt.Sprintf("connected to Redfish endpoint %s", rf.URL), corev1.EventTypeNormal)

	// Boot order configuration (skip if known unsupported).
	bootCond := meta.FindStatusCondition(machine.Status.Conditions, condBootSupported)
	if bootCond == nil || bootCond.Status != metav1.ConditionFalse {
		pendingRepave, ok, err := r.latestBootOverrideTarget(ctx, client.ObjectKeyFromObject(&machine), rf)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("checking latest boot override target: %w", err)
		}

		if !ok {
			return ctrl.Result{}, nil
		}

		if err := r.reconcileBootOrder(ctx, log, &machine, c, pendingRepave); err != nil {
			if errors.Is(err, ErrUnsupported) {
				// BMCs commonly reject boot order changes during POST.
				// Only conclude the feature is permanently unsupported
				// once the system has converged into a known power state
				// (On or Off). Transient states like PoweringOn indicate
				// the system is still in POST where rejections are expected.
				state, psErr := c.PowerState(ctx)
				if psErr != nil {
					r.setRedfishReady(ctx, &machine, metav1.ConditionFalse, "PowerStateFailed", fmt.Sprintf("failed to read Redfish power state after boot override rejection: %v", psErr), corev1.EventTypeWarning)
					return ctrl.Result{}, fmt.Errorf("getting power state: %w", psErr)
				}

				if !strings.EqualFold(string(state), "On") && !strings.EqualFold(string(state), "Off") {
					log.Info("boot order config rejected during transient power state, retrying",
						"powerState", state, "err", err)

					return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
				}

				log.Info("boot order config not supported", "err", err)
				meta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
					Type:               condBootSupported,
					Status:             metav1.ConditionFalse,
					Reason:             reasonNotSupported,
					Message:            fmt.Sprintf("Redfish boot override is not supported: %v", err),
					ObservedGeneration: machine.Generation,
				})

				return ctrl.Result{}, r.updateMachineStatus(ctx, client.ObjectKeyFromObject(&machine), func(latest *v1alpha3.Machine) bool {
					return machinestatus.SetConditionIfChanged(latest, machinestatus.Condition(
						condBootSupported,
						metav1.ConditionFalse,
						reasonNotSupported,
						fmt.Sprintf("Redfish boot override is not supported: %v", err),
						latest.Generation,
					))
				})
			}

			r.setRedfishReady(ctx, &machine, metav1.ConditionFalse, "BootOverrideFailed", fmt.Sprintf("failed to configure Redfish boot override: %v", err), corev1.EventTypeWarning)

			return ctrl.Result{}, fmt.Errorf("configuring boot order: %w", err)
		}
	}

	// No reboot pending - done.
	if machine.Spec.Operations.RebootCounter <= machine.Status.Operations.RebootCounter {
		return ctrl.Result{}, nil
	}

	// Reboot cycle: ForceOff → confirm Off → On → confirm On → complete.
	if !meta.IsStatusConditionTrue(machine.Status.Conditions, condPoweredOff) {
		return r.reconcilePowerOff(ctx, log, &machine, c)
	}

	return r.reconcilePowerOn(ctx, log, &machine, c)
}

func (r *Reconciler) setRedfishReady(ctx context.Context, machine *v1alpha3.Machine, status metav1.ConditionStatus, reason, message, eventType string) {
	if machine == nil || r.Client == nil {
		return
	}

	changed := false

	if err := r.updateMachineStatus(ctx, client.ObjectKeyFromObject(machine), func(latest *v1alpha3.Machine) bool {
		changed = machinestatus.SetConditionIfChanged(latest, machinestatus.Condition(
			v1alpha3.MachineConditionRedfishReady,
			status,
			reason,
			message,
			latest.Generation,
		))

		return changed
	}); err != nil {
		slog.Error("updating RedfishReady condition", "node", machine.Name, "err", err)
		return
	}

	if !changed {
		return
	}

	machinestatus.Event(r.Recorder, machine, eventType, "Redfish"+reason, message)
}

func (r *Reconciler) updateMachineStatus(ctx context.Context, key client.ObjectKey, mutate func(*v1alpha3.Machine) bool) error {
	return machinestatus.Update(ctx, r.Client, key, mutate)
}

// reconcileBootOrder ensures the boot source override matches the desired state.
// Returns ErrUnsupported if the BMC does not support boot order configuration.
func (r *Reconciler) reconcileBootOrder(ctx context.Context, log *slog.Logger, machine *v1alpha3.Machine, c *Client, pendingRepave bool) error {
	config, err := c.GetBootConfig(ctx)
	if err != nil {
		return err
	}

	if pendingRepave {
		if config.Target == BootTargetPxe && config.Enabled == BootContinuous {
			return nil // Already set to PXE boot.
		}

		log.Info("setting boot source override to PXE", "currentTarget", config.Target, "currentEnabled", config.Enabled)

		return c.SetBootOverride(ctx, BootTargetPxe, BootContinuous)
	}

	if config.Enabled == BootDisabled ||
		(config.Target == BootTargetHdd && config.Enabled == BootContinuous) {
		return nil // Already disabled or set to HDD.
	}

	log.Info("disabling boot source override", "currentTarget", config.Target, "currentEnabled", config.Enabled)

	return c.DisableBootOverride(ctx)
}

// reconcilePowerOff drives the machine to the Off state by sending ForceOff
// and polling until the BMC reports Off.
func (r *Reconciler) reconcilePowerOff(ctx context.Context, log *slog.Logger, machine *v1alpha3.Machine, c *Client) (ctrl.Result, error) {
	state, err := c.PowerState(ctx)
	if err != nil {
		r.setRedfishReady(ctx, machine, metav1.ConditionFalse, "PowerStateFailed", fmt.Sprintf("failed to read Redfish power state: %v", err), corev1.EventTypeWarning)
		return ctrl.Result{}, fmt.Errorf("getting power state: %w", err)
	}

	targetReboot := specRebootCounter(machine)
	targetRepave := specRepaveCounter(machine)
	targetImage := pxeImage(machine)
	targetRedfish := redfishSpec(machine)

	if state == PowerOff {
		log.Info("machine confirmed powered off, setting condition")

		return ctrl.Result{}, r.updateMachineStatus(ctx, client.ObjectKeyFromObject(machine), func(latest *v1alpha3.Machine) bool {
			if !sameOperationTarget(latest, targetReboot, targetRepave, targetImage, targetRedfish) {
				return false
			}

			if statusRebootCounter(latest) >= targetReboot {
				return false
			}

			return machinestatus.SetConditionIfChanged(latest, machinestatus.Condition(
				condPoweredOff,
				metav1.ConditionTrue,
				reasonForceOff,
				fmt.Sprintf("target reboots: %d", targetReboot),
				latest.Generation,
			))
		})
	}

	// Machine is still on. Check if ForceOff was already sent.
	cond := meta.FindStatusCondition(machine.Status.Conditions, condPoweredOff)
	if cond != nil && cond.Reason == reasonPoweringOff {
		if time.Since(cond.LastTransitionTime.Time) < powerActionTimeout {
			return ctrl.Result{RequeueAfter: time.Second}, nil // Wait for ForceOff to take effect.
		}

		log.Info("ForceOff timed out, retrying", "elapsed", time.Since(cond.LastTransitionTime.Time))
	}

	log.Info("sending ForceOff", "currentState", state)
	if pending, err := r.operationTargetPending(ctx, client.ObjectKeyFromObject(machine), targetReboot, targetRepave, targetImage, targetRedfish); err != nil {
		return ctrl.Result{}, fmt.Errorf("checking latest reboot target before ForceOff: %w", err)
	} else if !pending {
		return ctrl.Result{}, nil
	}

	if err := c.Reset(ctx, ResetForceOff); err != nil {
		r.setRedfishReady(ctx, machine, metav1.ConditionFalse, "ResetFailed", fmt.Sprintf("failed to send Redfish ForceOff: %v", err), corev1.EventTypeWarning)
		return ctrl.Result{}, fmt.Errorf("sending ForceOff: %w", err)
	}

	return ctrl.Result{}, r.updateMachineStatus(ctx, client.ObjectKeyFromObject(machine), func(latest *v1alpha3.Machine) bool {
		if !sameOperationTarget(latest, targetReboot, targetRepave, targetImage, targetRedfish) {
			return false
		}

		if statusRebootCounter(latest) >= targetReboot {
			return false
		}

		// Remove before set so LastTransitionTime is reset on retries.
		meta.RemoveStatusCondition(&latest.Status.Conditions, condPoweredOff)
		meta.SetStatusCondition(&latest.Status.Conditions, machinestatus.Condition(
			condPoweredOff,
			metav1.ConditionFalse,
			reasonPoweringOff,
			fmt.Sprintf("target reboots: %d", targetReboot),
			latest.Generation,
		))

		return true
	})
}

// reconcilePowerOn drives the machine from Off to On and completes the
// reboot cycle.
func (r *Reconciler) reconcilePowerOn(ctx context.Context, log *slog.Logger, machine *v1alpha3.Machine, c *Client) (ctrl.Result, error) {
	state, err := c.PowerState(ctx)
	if err != nil {
		r.setRedfishReady(ctx, machine, metav1.ConditionFalse, "PowerStateFailed", fmt.Sprintf("failed to read Redfish power state: %v", err), corev1.EventTypeWarning)
		return ctrl.Result{}, fmt.Errorf("getting power state: %w", err)
	}

	targetReboot := specRebootCounter(machine)
	targetRepave := specRepaveCounter(machine)
	targetImage := pxeImage(machine)
	targetRedfish := redfishSpec(machine)

	if state != PowerOff {
		// Machine is on - complete the reboot cycle.
		log.Info("machine confirmed powered on, completing reboot cycle")

		return ctrl.Result{}, r.updateMachineStatus(ctx, client.ObjectKeyFromObject(machine), func(latest *v1alpha3.Machine) bool {
			if !sameOperationTarget(latest, targetReboot, targetRepave, targetImage, targetRedfish) {
				return false
			}

			if statusRebootCounter(latest) >= targetReboot {
				return false
			}

			meta.RemoveStatusCondition(&latest.Status.Conditions, condPoweredOff)

			if pendingRepaveFor(latest) {
				image := ""
				if latest.Spec.PXE != nil {
					image = latest.Spec.PXE.Image
				}

				meta.SetStatusCondition(&latest.Status.Conditions, machinestatus.Condition(
					condRepaved,
					metav1.ConditionFalse,
					reasonPending,
					"image="+image,
					latest.Generation,
				))
			}

			if latest.Status.Operations == nil {
				latest.Status.Operations = &v1alpha3.OperationsStatus{}
			}

			latest.Status.Operations.RebootCounter = targetReboot

			return true
		})
	}

	// Machine is still off. Check if On was already sent.
	cond := meta.FindStatusCondition(machine.Status.Conditions, condPoweredOff)
	if cond != nil && cond.Reason == reasonPoweringOn {
		if time.Since(cond.LastTransitionTime.Time) < powerActionTimeout {
			return ctrl.Result{RequeueAfter: time.Second}, nil // Wait for On to take effect.
		}

		log.Info("On timed out, retrying", "elapsed", time.Since(cond.LastTransitionTime.Time))
	}

	log.Info("sending On")
	if pending, err := r.operationTargetPending(ctx, client.ObjectKeyFromObject(machine), targetReboot, targetRepave, targetImage, targetRedfish); err != nil {
		return ctrl.Result{}, fmt.Errorf("checking latest reboot target before On: %w", err)
	} else if !pending {
		return ctrl.Result{}, nil
	}

	if err := c.Reset(ctx, ResetOn); err != nil {
		r.setRedfishReady(ctx, machine, metav1.ConditionFalse, "ResetFailed", fmt.Sprintf("failed to send Redfish On: %v", err), corev1.EventTypeWarning)
		return ctrl.Result{}, fmt.Errorf("sending On: %w", err)
	}

	return ctrl.Result{}, r.updateMachineStatus(ctx, client.ObjectKeyFromObject(machine), func(latest *v1alpha3.Machine) bool {
		if !sameOperationTarget(latest, targetReboot, targetRepave, targetImage, targetRedfish) {
			return false
		}

		if statusRebootCounter(latest) >= targetReboot {
			return false
		}

		// Remove before set so LastTransitionTime is reset on retries.
		meta.RemoveStatusCondition(&latest.Status.Conditions, condPoweredOff)
		meta.SetStatusCondition(&latest.Status.Conditions, machinestatus.Condition(
			condPoweredOff,
			metav1.ConditionTrue,
			reasonPoweringOn,
			fmt.Sprintf("target reboots: %d", targetReboot),
			latest.Generation,
		))

		return true
	})
}

func specRebootCounter(machine *v1alpha3.Machine) int64 {
	if machine.Spec.Operations == nil {
		return 0
	}

	return machine.Spec.Operations.RebootCounter
}

func specRepaveCounter(machine *v1alpha3.Machine) int64 {
	if machine.Spec.Operations == nil {
		return 0
	}

	return machine.Spec.Operations.RepaveCounter
}

func pxeImage(machine *v1alpha3.Machine) string {
	if machine.Spec.PXE == nil {
		return ""
	}

	return machine.Spec.PXE.Image
}

func redfishSpec(machine *v1alpha3.Machine) *v1alpha3.RedfishSpec {
	if machine.Spec.PXE == nil {
		return nil
	}

	return machine.Spec.PXE.Redfish
}

func sameRedfishSpec(machine *v1alpha3.Machine, rf *v1alpha3.RedfishSpec) bool {
	latestRF := redfishSpec(machine)
	if latestRF == nil || rf == nil {
		return latestRF == nil && rf == nil
	}

	return latestRF.URL == rf.URL &&
		latestRF.Username == rf.Username &&
		latestRF.DeviceID == rf.DeviceID &&
		latestRF.PasswordRef == rf.PasswordRef
}

func sameOperationTarget(machine *v1alpha3.Machine, rebootCounter, repaveCounter int64, image string, rf *v1alpha3.RedfishSpec) bool {
	return specRebootCounter(machine) == rebootCounter && specRepaveCounter(machine) == repaveCounter && pxeImage(machine) == image && sameRedfishSpec(machine, rf)
}

func statusRebootCounter(machine *v1alpha3.Machine) int64 {
	if machine.Status.Operations == nil {
		return 0
	}

	return machine.Status.Operations.RebootCounter
}

func (r *Reconciler) latestBootOverrideTarget(ctx context.Context, key client.ObjectKey, rf *v1alpha3.RedfishSpec) (bool, bool, error) {
	reader := r.latestReader()

	var latest v1alpha3.Machine
	if err := reader.Get(ctx, key, &latest); err != nil {
		return false, false, err
	}

	if !sameRedfishSpec(&latest, rf) {
		return false, false, nil
	}

	return pendingRepaveFor(&latest), true, nil
}

func (r *Reconciler) redfishFingerprintPending(ctx context.Context, key client.ObjectKey, rf *v1alpha3.RedfishSpec) (bool, error) {
	reader := r.latestReader()

	var latest v1alpha3.Machine
	if err := reader.Get(ctx, key, &latest); err != nil {
		return false, err
	}

	if !sameRedfishSpec(&latest, rf) {
		return false, nil
	}

	return latest.Status.Redfish == nil || latest.Status.Redfish.CertFingerprint == "", nil
}

func (r *Reconciler) operationTargetPending(ctx context.Context, key client.ObjectKey, rebootCounter, repaveCounter int64, image string, rf *v1alpha3.RedfishSpec) (bool, error) {
	reader := r.latestReader()

	var latest v1alpha3.Machine
	if err := reader.Get(ctx, key, &latest); err != nil {
		return false, err
	}

	return sameOperationTarget(&latest, rebootCounter, repaveCounter, image, rf) && statusRebootCounter(&latest) < rebootCounter, nil
}

func (r *Reconciler) latestReader() client.Reader {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}

	return reader
}

func pendingRepaveFor(machine *v1alpha3.Machine) bool {
	if machine.Spec.Operations == nil {
		return false
	}

	var statusRepave int64
	if machine.Status.Operations != nil {
		statusRepave = machine.Status.Operations.RepaveCounter
	}

	return machine.Spec.Operations.RepaveCounter > statusRepave
}
