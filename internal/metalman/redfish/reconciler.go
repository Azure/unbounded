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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	v1alpha3 "github.com/project-unbounded/unbounded-kube/api/v1alpha3"
)

const (
	// Condition types set by this controller.
	condPoweredOff    = "PoweredOff"
	condBootSupported = "BootOrderConfigSupported"
	condReimaged      = "Reimaged"

	// Condition reasons.
	reasonPoweringOff  = "PoweringOff"
	reasonForceOff     = "ForceOff"
	reasonPoweringOn   = "PoweringOn"
	reasonNotSupported = "NotSupported"
	reasonPending      = "Pending"

	powerActionTimeout = 5 * time.Minute
)

type Reconciler struct {
	Client client.Client
	Pool   *Pool
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
		fp, err := CaptureFingerprint(ctx, rf.URL)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("capturing TLS cert fingerprint: %w", err)
		}

		if machine.Status.Redfish == nil {
			machine.Status.Redfish = &v1alpha3.RedfishStatus{}
		}

		machine.Status.Redfish.CertFingerprint = fp
		log.Info("TOFU: captured TLS cert fingerprint", "fingerprint", fp)

		return ctrl.Result{}, r.Client.Status().Update(ctx, &machine)
	}

	// Retrieve Redfish password from Secret.
	var secret corev1.Secret
	if err := r.Client.Get(ctx, types.NamespacedName{
		Name:      rf.PasswordRef.Name,
		Namespace: rf.PasswordRef.Namespace,
	}, &secret); err != nil {
		return ctrl.Result{}, fmt.Errorf("getting Redfish password secret: %w", err)
	}

	password := string(secret.Data[rf.PasswordRef.Key])

	// Acquire Redfish client.
	c, err := r.Pool.Get(ctx, rf.URL, fingerprint, rf.Username, password, rf.DeviceID)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting Redfish client: %w", err)
	}

	// Boot order configuration (skip if known unsupported).
	pendingReimage := machine.Spec.Operations.ReimageCounter > machine.Status.Operations.ReimageCounter

	bootCond := meta.FindStatusCondition(machine.Status.Conditions, condBootSupported)
	if bootCond == nil || bootCond.Status != metav1.ConditionFalse {
		if err := r.reconcileBootOrder(ctx, log, &machine, c, pendingReimage); err != nil {
			if errors.Is(err, ErrUnsupported) {
				// BMCs commonly reject boot order changes during POST.
				// Only conclude the feature is permanently unsupported
				// once the system has converged into a known power state
				// (On or Off). Transient states like PoweringOn indicate
				// the system is still in POST where rejections are expected.
				state, psErr := c.PowerState(ctx)
				if psErr != nil {
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
					ObservedGeneration: machine.Generation,
				})

				return ctrl.Result{}, r.Client.Status().Update(ctx, &machine)
			}

			return ctrl.Result{}, fmt.Errorf("configuring boot order: %w", err)
		}
	}

	// No reboot pending — done.
	if machine.Spec.Operations.RebootCounter <= machine.Status.Operations.RebootCounter {
		return ctrl.Result{}, nil
	}

	// Reboot cycle: ForceOff → confirm Off → On → confirm On → complete.
	if !meta.IsStatusConditionTrue(machine.Status.Conditions, condPoweredOff) {
		return r.reconcilePowerOff(ctx, log, &machine, c)
	}

	return r.reconcilePowerOn(ctx, log, &machine, c, pendingReimage)
}

// reconcileBootOrder ensures the boot source override matches the desired state.
// Returns ErrUnsupported if the BMC does not support boot order configuration.
func (r *Reconciler) reconcileBootOrder(ctx context.Context, log *slog.Logger, machine *v1alpha3.Machine, c *Client, pendingReimage bool) error {
	config, err := c.GetBootConfig(ctx)
	if err != nil {
		return err
	}

	if pendingReimage {
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
		return ctrl.Result{}, fmt.Errorf("getting power state: %w", err)
	}

	if state == PowerOff {
		log.Info("machine confirmed powered off, setting condition")
		meta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
			Type:               condPoweredOff,
			Status:             metav1.ConditionTrue,
			Reason:             reasonForceOff,
			Message:            fmt.Sprintf("target reboots: %d", machine.Spec.Operations.RebootCounter),
			ObservedGeneration: machine.Generation,
		})

		return ctrl.Result{}, r.Client.Status().Update(ctx, machine)
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

	if err := c.Reset(ctx, ResetForceOff); err != nil {
		return ctrl.Result{}, fmt.Errorf("sending ForceOff: %w", err)
	}

	// Remove before set so LastTransitionTime is reset on retries.
	meta.RemoveStatusCondition(&machine.Status.Conditions, condPoweredOff)
	meta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type:               condPoweredOff,
		Status:             metav1.ConditionFalse,
		Reason:             reasonPoweringOff,
		Message:            fmt.Sprintf("target reboots: %d", machine.Spec.Operations.RebootCounter),
		ObservedGeneration: machine.Generation,
	})

	return ctrl.Result{}, r.Client.Status().Update(ctx, machine)
}

// reconcilePowerOn drives the machine from Off to On and completes the
// reboot cycle.
func (r *Reconciler) reconcilePowerOn(ctx context.Context, log *slog.Logger, machine *v1alpha3.Machine, c *Client, pendingReimage bool) (ctrl.Result, error) {
	state, err := c.PowerState(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting power state: %w", err)
	}

	if state != PowerOff {
		// Machine is on — complete the reboot cycle.
		log.Info("machine confirmed powered on, completing reboot cycle")
		meta.RemoveStatusCondition(&machine.Status.Conditions, condPoweredOff)

		if pendingReimage {
			meta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
				Type:               condReimaged,
				Status:             metav1.ConditionFalse,
				Reason:             reasonPending,
				Message:            "image=" + machine.Spec.PXE.Image,
				ObservedGeneration: machine.Generation,
			})
		}

		machine.Status.Operations.RebootCounter = machine.Spec.Operations.RebootCounter

		return ctrl.Result{}, r.Client.Status().Update(ctx, machine)
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

	if err := c.Reset(ctx, ResetOn); err != nil {
		return ctrl.Result{}, fmt.Errorf("sending On: %w", err)
	}

	// Remove before set so LastTransitionTime is reset on retries.
	meta.RemoveStatusCondition(&machine.Status.Conditions, condPoweredOff)
	meta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type:               condPoweredOff,
		Status:             metav1.ConditionTrue,
		Reason:             reasonPoweringOn,
		Message:            fmt.Sprintf("target reboots: %d", machine.Spec.Operations.RebootCounter),
		ObservedGeneration: machine.Generation,
	})

	return ctrl.Result{}, r.Client.Status().Update(ctx, machine)
}
