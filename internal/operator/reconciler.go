// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/operator/components/machina"
	"github.com/Azure/unbounded/internal/operator/components/metalman"
	netcomponent "github.com/Azure/unbounded/internal/operator/components/net"
	"github.com/Azure/unbounded/internal/operator/components/storage"
)

// FieldOwner is the server-side apply field manager the operator uses. It is
// re-exported from the component package for callers within this package.
const FieldOwner = component.FieldOwner

// DefaultNamespace is the namespace the operator installs components into.
var DefaultNamespace = component.DefaultNamespace

// Config carries operator-level settings handed to components.
type Config = component.Config

// SiteReconciler reconciles the registered components for every Site. It drives
// a component.Registry: cluster components (net, machina) run on every pass, and
// per-Site components (metalman, storage) run when a Site is present. Adding a
// component is a matter of implementing component.ClusterComponent or
// component.SiteComponent and adding it to the registry; this loop, the status
// conditions, ordering, and the Site-less pass are all registry-driven.
type SiteReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Config Config

	// Namespace is the namespace components are reconciled into. When empty it
	// falls back to component.DefaultNamespace so the operator follows the
	// namespace it is installed in.
	Namespace string

	// Registry is the set of components to reconcile. When nil it defaults to
	// DefaultRegistry.
	Registry *component.Registry
}

// DefaultRegistry returns the built-in component registry: the net and machina
// cluster singletons and the metalman and storage per-Site components. The slice
// order is the stable Site status condition order (cluster first, then site).
func DefaultRegistry() *component.Registry {
	return &component.Registry{
		Cluster: []component.ClusterComponent{
			netcomponent.New(),
			machina.New(),
		},
		Site: []component.SiteComponent{
			metalman.New(),
			storage.New(),
		},
	}
}

// namespace returns the namespace the reconciler installs components into,
// falling back to the package default when unset.
func (r *SiteReconciler) namespace() string {
	if r.Namespace != "" {
		return r.Namespace
	}

	return component.DefaultNamespace
}

func (r *SiteReconciler) env() *component.Env {
	return &component.Env{
		Client:    r.Client,
		Scheme:    r.Scheme,
		Namespace: r.namespace(),
		Config:    r.Config,
	}
}

func (r *SiteReconciler) registry() *component.Registry {
	if r.Registry != nil {
		return r.Registry
	}

	return DefaultRegistry()
}

func (r *SiteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	env := r.env()
	reg := r.registry()

	var site unboundedv1alpha3.Site

	err := r.Get(ctx, req.NamespacedName, &site)
	switch {
	case apierrors.IsNotFound(err):
		// A Site was deleted, or a managed singleton event used the synthetic
		// request. Reconcile the cluster components with no specific Site; there
		// is no Site status to publish.
		return ctrl.Result{}, r.reconcileClusters(ctx, env, reg)
	case err != nil:
		return ctrl.Result{}, err
	}

	sites, err := env.ListSites(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	patch := client.MergeFrom(site.DeepCopy())

	var reconcileErrs []error

	// Cluster singletons run on every Site event; per-Site components run for
	// this Site. Conditions are published in registry order (cluster then site)
	// so the status patch is deterministic and callers can
	// `kubectl wait --for=condition=NetReady`.
	for _, c := range reg.Cluster {
		res := c.Reconcile(ctx, env, sites)
		if err := setComponentResult(logger, &site, c.Name(), c.ConditionType(), res); err != nil {
			reconcileErrs = append(reconcileErrs, err)
		}
	}

	for _, c := range reg.Site {
		res := reconcileSiteComponent(ctx, env, c, &site)
		if err := setComponentResult(logger, &site, c.Name(), c.ConditionType(), res); err != nil {
			reconcileErrs = append(reconcileErrs, err)
		}
	}

	if err := r.Status().Patch(ctx, &site, patch); err != nil {
		reconcileErrs = append(reconcileErrs, fmt.Errorf("patch site status: %w", err))
	}

	return ctrl.Result{}, errors.Join(reconcileErrs...)
}

// reconcileClusters reconciles just the cluster components; used for deleted
// Sites and synthetic singleton requests, where there is no Site status.
func (r *SiteReconciler) reconcileClusters(ctx context.Context, env *component.Env, reg *component.Registry) error {
	sites, err := env.ListSites(ctx)
	if err != nil {
		return err
	}

	var errs []error

	for _, c := range reg.Cluster {
		res := c.Reconcile(ctx, env, sites)
		if res.Err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c.Name(), res.Err))
		}
	}

	return errors.Join(errs...)
}

// reconcileSiteComponent reconciles a per-Site component when enabled and tears
// it down when disabled, turning the outcome into a component.Result.
func reconcileSiteComponent(ctx context.Context, env *component.Env, c component.SiteComponent, site *unboundedv1alpha3.Site) component.Result {
	if !c.Enabled(site) {
		if err := c.Cleanup(ctx, env, site); err != nil {
			return component.Failed(err)
		}

		return component.Disabled("component disabled")
	}

	return c.Reconcile(ctx, env, site)
}

// setComponentResult publishes a component's Result as a Site status condition
// and returns the reconcile error to aggregate, if any.
func setComponentResult(logger logr.Logger, site *unboundedv1alpha3.Site, name, conditionType string, res component.Result) error {
	if !res.Ready {
		logger.Info("component not ready", "site", site.Name, "component", name, "message", res.Message)
	}

	status := metav1.ConditionTrue
	if !res.Ready {
		status = metav1.ConditionFalse
	}

	apimeta.SetStatusCondition(&site.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             res.Reason,
		Message:            res.Message,
		ObservedGeneration: site.Generation,
	})

	if res.Err != nil {
		return fmt.Errorf("%s: %w", name, res.Err)
	}

	return nil
}

func (r *SiteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	reg := r.registry()
	if err := reg.Validate(); err != nil {
		return fmt.Errorf("invalid component registry: %w", err)
	}

	env := r.env()

	// Site status is written by the net controller (nodeCount/sliceCount) on
	// node churn; reconciling on those status-only updates would re-apply the
	// full component manifest sets on every event. Filter to spec/generation
	// changes (Create/Delete still pass) so component reconcile is driven by
	// intent, not status noise.
	b := ctrl.NewControllerManagedBy(mgr).
		For(&unboundedv1alpha3.Site{}, builder.WithPredicates(predicate.GenerationChangedPredicate{}))

	for _, c := range reg.Cluster {
		if wp, ok := c.(component.WatchProvider); ok {
			wp.SetupWatches(b, env)
		}
	}

	for _, c := range reg.Site {
		if wp, ok := c.(component.WatchProvider); ok {
			wp.SetupWatches(b, env)
		}
	}

	return b.Complete(r)
}
