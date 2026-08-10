// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"context"
	"errors"
	"fmt"
	"time"

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
	"github.com/Azure/unbounded/internal/operator/components/gantry"
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
			gantry.New(),
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

// registry returns the component registry, materializing the default registry
// once on first use. Materializing (rather than rebuilding DefaultRegistry on
// every call) means watch wiring and every Reconcile share the same component
// instances, so a component may safely hold state. SetupWithManager forces this
// before the manager starts, so concurrent Reconciles only ever read r.Registry.
func (r *SiteReconciler) registry() *component.Registry {
	if r.Registry == nil {
		r.Registry = DefaultRegistry()
	}

	return r.Registry
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
		// request. Run the cluster components with no specific Site; there is no
		// Site status to publish.
		return r.runComponents(ctx, logger, env, reg, nil)
	case err != nil:
		return ctrl.Result{}, err
	}

	return r.runComponents(ctx, logger, env, reg, &site)
}

// runComponents drives the registry for one reconcile pass. Cluster components
// run on every pass from the full set of Sites; per-Site components run only when
// site is non-nil. When site is non-nil the component outcomes are published as
// Site status conditions in registry order (cluster first, then site) so callers
// can `kubectl wait --for=condition=NetReady`, and conditions left by components
// no longer in the registry are pruned. The Site-less pass (deletion or a
// synthetic singleton request) has no Site status to write, so it only surfaces
// component errors and requeue requests.
//
// The pass requeues on any component Err (with controller backoff) or, absent an
// error, after the smallest positive RequeueAfter across the components.
func (r *SiteReconciler) runComponents(ctx context.Context, logger logr.Logger, env *component.Env, reg *component.Registry, site *unboundedv1alpha3.Site) (ctrl.Result, error) {
	sites, err := env.ListSites(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Capture the status baseline before any component mutates conditions so the
	// merge patch carries exactly the condition changes.
	var patch client.Patch
	if site != nil {
		patch = client.MergeFrom(site.DeepCopy())
	}

	// Plan every component before executing any of it. Planning performs no
	// writes, so a planning failure leaves the cluster untouched, and the
	// merged plan is what lets shared operations deduplicate across components.
	planned, plan := planComponents(ctx, env, reg, sites, site)

	exec, execErr := env.Execute(ctx, plan)

	var (
		reconcileErrs []error
		requeueAfter  time.Duration
	)

	if execErr != nil {
		// A plan the executor refuses to run (a dependency cycle, or
		// contradictory shared operations) is a programming error, not a
		// component outcome, so it is reported once rather than per component.
		reconcileErrs = append(reconcileErrs, fmt.Errorf("execute plan: %w", execErr))
	}

	for _, outcome := range planned {
		res := component.CombineResult(outcome.name, outcome.result, exec)

		switch {
		case site != nil:
			if err := setComponentResult(logger, site, outcome.name, outcome.conditionType, res); err != nil {
				reconcileErrs = append(reconcileErrs, err)
			}
		case res.Err != nil:
			reconcileErrs = append(reconcileErrs, fmt.Errorf("%s: %w", outcome.name, res.Err))
		}

		requeueAfter = nextRequeue(requeueAfter, res.RequeueAfter)
	}

	if site != nil {
		pruneStaleConditions(site, reg)

		if err := r.Status().Patch(ctx, site, patch); err != nil {
			reconcileErrs = append(reconcileErrs, fmt.Errorf("patch site status: %w", err))
		}
	}

	return ctrl.Result{RequeueAfter: requeueAfter}, errors.Join(reconcileErrs...)
}

// componentOutcome is a component's planning verdict, held until execution
// completes so the two can be folded together.
type componentOutcome struct {
	name          string
	conditionType string
	result        component.Result
}

// planComponents plans every component in registry order and merges the result
// into one plan. A planning error becomes that component's Result rather than
// aborting the pass, so one component failing to plan does not stop the others
// from reconciling.
func planComponents(
	ctx context.Context,
	env *component.Env,
	reg *component.Registry,
	sites []unboundedv1alpha3.Site,
	site *unboundedv1alpha3.Site,
) ([]componentOutcome, *component.Plan) {
	outcomes := make([]componentOutcome, 0, len(reg.Cluster)+len(reg.Site))
	plan := component.NewPlan()

	for _, c := range reg.Cluster {
		componentPlan, res, err := c.Plan(ctx, env, sites)
		if err != nil {
			res = component.Failed(err)
		} else {
			plan.Merge(componentPlan)
		}

		outcomes = append(outcomes, componentOutcome{name: c.Name(), conditionType: c.ConditionType(), result: res})
	}

	if site == nil {
		return outcomes, plan
	}

	for _, c := range reg.Site {
		componentPlan, res, err := planSiteComponent(ctx, env, c, site)
		if err != nil {
			res = component.Failed(err)
		} else {
			plan.Merge(componentPlan)
		}

		outcomes = append(outcomes, componentOutcome{name: c.Name(), conditionType: c.ConditionType(), result: res})
	}

	return outcomes, plan
}

// planSiteComponent owns the enable/disable branch: an enabled component plans
// its desired state, a disabled one plans the removal of its per-Site
// resources.
func planSiteComponent(
	ctx context.Context,
	env *component.Env,
	c component.SiteComponent,
	site *unboundedv1alpha3.Site,
) (*component.Plan, component.Result, error) {
	if !c.Enabled(site) {
		return c.CleanupPlan(ctx, env, site)
	}

	return c.Plan(ctx, env, site)
}

// nextRequeue returns the smallest positive of the current and candidate
// requeue-after durations, so the driver retries at the earliest requested time.
func nextRequeue(current, candidate time.Duration) time.Duration {
	if candidate <= 0 {
		return current
	}

	if current <= 0 || candidate < current {
		return candidate
	}

	return current
}

// pruneStaleConditions removes Site status conditions whose type is not published
// by any component currently in the registry, so a Site does not carry orphaned
// conditions after a component is removed or renamed. This is safe because the
// SiteReconciler is the sole writer of Site.status.conditions; if another
// controller ever writes Site conditions, this must be scoped to component
// condition types.
func pruneStaleConditions(site *unboundedv1alpha3.Site, reg *component.Registry) {
	current := make(map[string]struct{}, len(reg.Cluster)+len(reg.Site))

	for _, c := range reg.Cluster {
		current[c.ConditionType()] = struct{}{}
	}

	for _, c := range reg.Site {
		current[c.ConditionType()] = struct{}{}
	}

	var stale []string

	for i := range site.Status.Conditions {
		if _, ok := current[site.Status.Conditions[i].Type]; !ok {
			stale = append(stale, site.Status.Conditions[i].Type)
		}
	}

	for _, conditionType := range stale {
		apimeta.RemoveStatusCondition(&site.Status.Conditions, conditionType)
	}
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
	// registry() materializes r.Registry so watch wiring below and every later
	// Reconcile share the same component instances (see registry). This runs
	// before the manager starts, so the concurrent Reconciles only read it.
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
