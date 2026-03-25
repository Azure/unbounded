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
	conditionPoweredOff               = "PoweredOff"
	conditionBootOrderConfigSupported = "BootOrderConfigSupported"
	conditionReimaged                 = "Reimaged"
)

type Reconciler struct {
	Client client.Client
	Pool   *Pool
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha3.Machine{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				node, ok := e.Object.(*v1alpha3.Machine)
				return ok && node.Spec.PXE != nil && node.Spec.PXE.Redfish != nil
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				node, ok := e.ObjectNew.(*v1alpha3.Machine)
				return ok && node.Spec.PXE != nil && node.Spec.PXE.Redfish != nil
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				return false
			},
			GenericFunc: func(e event.GenericEvent) bool {
				node, ok := e.Object.(*v1alpha3.Machine)
				return ok && node.Spec.PXE != nil && node.Spec.PXE.Redfish != nil
			},
		}).
		Complete(r)
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := slog.With("node", req.Name, "namespace", req.Namespace)

	var node v1alpha3.Machine
	if err := r.Client.Get(ctx, req.NamespacedName, &node); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if node.Spec.PXE == nil || node.Spec.PXE.Redfish == nil {
		return ctrl.Result{}, nil
	}

	if node.Spec.Operations == nil {
		node.Spec.Operations = &v1alpha3.OperationsSpec{}
	}

	if node.Status.Operations == nil {
		node.Status.Operations = &v1alpha3.OperationsStatus{}
	}

	rf := node.Spec.PXE.Redfish

	existingFingerprint := ""
	if node.Status.Redfish != nil {
		existingFingerprint = node.Status.Redfish.CertFingerprint
	}

	if existingFingerprint == "" {
		fp, err := CaptureFingerprint(ctx, rf.URL)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("capturing TLS cert fingerprint: %w", err)
		}

		if node.Status.Redfish == nil {
			node.Status.Redfish = &v1alpha3.RedfishStatus{}
		}

		node.Status.Redfish.CertFingerprint = fp
		log.Info("TOFU: captured TLS cert fingerprint", "fingerprint", fp)

		return ctrl.Result{}, r.Client.Status().Update(ctx, &node)
	}

	var secret corev1.Secret
	if err := r.Client.Get(ctx, types.NamespacedName{
		Name:      rf.PasswordRef.Name,
		Namespace: rf.PasswordRef.Namespace,
	}, &secret); err != nil {
		return ctrl.Result{}, fmt.Errorf("getting Redfish password secret: %w", err)
	}

	password := string(secret.Data[rf.PasswordRef.Key])

	c, err := r.Pool.Get(ctx, rf.URL, existingFingerprint, rf.Username, password, rf.DeviceID)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting Redfish client: %w", err)
	}

	// Boot order configuration (only attempt if not known unsupported).
	bootOrderCond := meta.FindStatusCondition(node.Status.Conditions, conditionBootOrderConfigSupported)

	pendingReimage := node.Spec.Operations.ReimageCounter > node.Status.Operations.ReimageCounter
	if bootOrderCond == nil || bootOrderCond.Status != metav1.ConditionFalse {
		if err := c.BootOrderConfig(ctx, log, pendingReimage); err != nil {
			if errors.Is(err, ErrUnsupported) {
				log.Info("boot order config not supported", "err", err)
				meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
					Type:               conditionBootOrderConfigSupported,
					Status:             metav1.ConditionFalse,
					Reason:             "NotSupported",
					ObservedGeneration: node.Generation,
				})

				return ctrl.Result{}, r.Client.Status().Update(ctx, &node)
			}

			return ctrl.Result{}, fmt.Errorf("configuring boot order: %w", err)
		}
	}

	if node.Spec.Operations.RebootCounter <= node.Status.Operations.RebootCounter {
		return ctrl.Result{}, nil
	}

	poweredOff := meta.IsStatusConditionTrue(node.Status.Conditions, conditionPoweredOff)
	if !poweredOff {
		state, err := c.PowerState(ctx)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("getting power state: %w", err)
		}

		if !strings.EqualFold(state, "Off") {
			cond := meta.FindStatusCondition(node.Status.Conditions, conditionPoweredOff)
			if cond != nil && cond.Reason == "PoweringOff" {
				// ForceOff was already sent — wait for it to take effect.
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}

			log.Info("sending ForceOff", "currentState", state)

			if err := c.Reset(ctx, "ForceOff"); err != nil {
				return ctrl.Result{}, fmt.Errorf("sending ForceOff: %w", err)
			}

			meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
				Type:               conditionPoweredOff,
				Status:             metav1.ConditionFalse,
				Reason:             "PoweringOff",
				Message:            fmt.Sprintf("target reboots: %d", node.Spec.Operations.RebootCounter),
				ObservedGeneration: node.Generation,
			})

			return ctrl.Result{}, r.Client.Status().Update(ctx, &node)
		}

		log.Info("machine confirmed powered off, setting condition")
		meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
			Type:               conditionPoweredOff,
			Status:             metav1.ConditionTrue,
			Reason:             "ForceOff",
			Message:            fmt.Sprintf("target reboots: %d", node.Spec.Operations.RebootCounter),
			ObservedGeneration: node.Generation,
		})

		return ctrl.Result{}, r.Client.Status().Update(ctx, &node)
	}

	// PoweredOff is True: check actual power state to decide next step.
	state, err := c.PowerState(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting power state: %w", err)
	}

	if strings.EqualFold(state, "Off") {
		cond := meta.FindStatusCondition(node.Status.Conditions, conditionPoweredOff)
		if cond != nil && cond.Reason == "PoweringOn" {
			// On was already sent — wait for it to take effect.
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}

		log.Info("sending On")

		if err := c.Reset(ctx, "On"); err != nil {
			return ctrl.Result{}, fmt.Errorf("sending On: %w", err)
		}

		meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
			Type:               conditionPoweredOff,
			Status:             metav1.ConditionTrue,
			Reason:             "PoweringOn",
			Message:            fmt.Sprintf("target reboots: %d", node.Spec.Operations.RebootCounter),
			ObservedGeneration: node.Generation,
		})

		return ctrl.Result{}, r.Client.Status().Update(ctx, &node)
	}

	// Machine reports On — clear PoweredOff and complete the reboot cycle.
	log.Info("machine confirmed powered on, completing reboot cycle")
	meta.RemoveStatusCondition(&node.Status.Conditions, conditionPoweredOff)

	if pendingReimage {
		meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
			Type:               conditionReimaged,
			Status:             metav1.ConditionFalse,
			Reason:             "Pending",
			Message:            "image=" + node.Spec.PXE.ImageRef.Name,
			ObservedGeneration: node.Generation,
		})
	}

	node.Status.Operations.RebootCounter = node.Spec.Operations.RebootCounter

	return ctrl.Result{}, r.Client.Status().Update(ctx, &node)
}
