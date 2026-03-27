package lifecycle

import (
	"context"
	"log/slog"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/project-unbounded/unbounded-kube/api/v1alpha3"
)

const (
	reimageTimeout    = 30 * time.Minute // TODO: Make this configurable
	conditionReimaged = "Reimaged"
)

type Reconciler struct {
	Client client.Client
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("lifecycle").
		For(&v1alpha3.Machine{}).
		Complete(r)
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := slog.With("controller", "lifecycle", "node", req.Name, "namespace", req.Namespace)

	var node v1alpha3.Machine
	if err := r.Client.Get(ctx, req.NamespacedName, &node); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if node.Spec.Operations == nil || node.Status.Operations == nil {
		return ctrl.Result{}, nil
	}

	pendingReimage := node.Spec.Operations.ReimageCounter > node.Status.Operations.ReimageCounter
	if !pendingReimage {
		return ctrl.Result{}, nil
	}

	reimagedCond := meta.FindStatusCondition(node.Status.Conditions, conditionReimaged)
	if reimagedCond == nil || reimagedCond.Status != metav1.ConditionFalse {
		return ctrl.Result{}, nil
	}

	elapsed := time.Since(reimagedCond.LastTransitionTime.Time)
	if elapsed < reimageTimeout {
		return ctrl.Result{RequeueAfter: reimageTimeout - elapsed}, nil
	}

	log.Info("reimage timed out, triggering retry", "elapsed", elapsed)
	meta.RemoveStatusCondition(&node.Status.Conditions, conditionReimaged)

	if node.Status.Operations.RebootCounter > 0 {
		node.Status.Operations.RebootCounter--
	}

	return ctrl.Result{}, r.Client.Status().Update(ctx, &node)
}
