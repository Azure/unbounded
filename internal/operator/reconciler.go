// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"k8s.io/client-go/tools/events"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/operator/components/gantry"
	"github.com/Azure/unbounded/internal/operator/components/machina"
	"github.com/Azure/unbounded/internal/operator/components/metalman"
	netcomponent "github.com/Azure/unbounded/internal/operator/components/net"
	"github.com/Azure/unbounded/internal/operator/components/storage"
	"github.com/Azure/unbounded/internal/operator/override"
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

	// APIReader reads straight from the apiserver, bypassing the manager cache.
	// Components use it through Env.LiveReader for the readiness questions the
	// cache answers badly: it lags the apiserver by however long a watch event
	// takes to arrive, and every kind read through it establishes an informer.
	// SetupWithManager refuses a nil APIReader so a running operator never
	// falls back.
	APIReader client.Reader

	// Recorder, when set, receives Events for override state changes. Optional
	// so unit tests can construct a reconciler without one.
	Recorder events.EventRecorder

	// overrideEventMu guards lastOverrideEvent, which records the verdict
	// already reported against the overrides ConfigMap so a document that
	// keeps requeueing does not produce an Event per pass. Reconciles can run
	// concurrently when MaxConcurrentReconciles is raised.
	overrideEventMu   sync.Mutex
	lastOverrideEvent string
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
		APIReader: r.APIReader,
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

	targets := fanOutTargets(site, sites)

	// Capture each target's status baseline before any component mutates
	// conditions, so the merge patch carries exactly the condition changes.
	//
	// The patch takes an optimistic lock. Conditions are a list, and a merge
	// patch replaces a list wholesale, so two passes writing the same Site
	// concurrently do not merge their condition updates: the later write drops
	// whatever the earlier one recorded. Passes do race, because the Site-less
	// fan-out pass writes every Site's status while a per-Site pass may be
	// writing one of them. With the lock the loser is told, and retries against
	// the winner's state.
	baselines := make(map[string]client.Patch, len(targets))
	for _, target := range targets {
		baselines[target.Name] = client.MergeFromWithOptions(target.DeepCopy(), client.MergeFromWithOptimisticLock{})
	}

	// Read and validate overrides once, before anything is planned. Parsing and
	// validation are pure functions of the payload, so a failure here means
	// nothing has been written and nothing will be for any workload an override
	// could target.
	snapshot := loadOverrides(ctx, env)

	// Plan every component before executing any of it. Planning performs no
	// writes, so a planning failure leaves the cluster untouched, and the
	// merged plan is what lets shared operations deduplicate across components
	// and across Sites.
	clusterOutcomes, siteOutcomes, plan := planComponents(ctx, env, reg, sites, targets)

	report, overrideErr := applyOverrides(logger, plan, snapshot, sites)

	exec, execErr := env.Execute(ctx, plan)

	// Work removed before execution reaches the same status path as work that
	// failed during it. Without this a component whose workload was withheld
	// reported Reconciled from its planning verdict, while the Site's override
	// status said Degraded about that very object.
	if report != nil {
		exec.Results = append(exec.Results, withheldResults(report.Withheld)...)
	}

	// Operations the registry does not own, such as the namespace every
	// component installs into, are attributed to a name no component
	// publishes. Nothing computing per-component conditions can report them,
	// so they are collected here. A failed namespace skips every namespaced
	// operation, and without this the pass returned no error and no requeue:
	// reconciliation stopped and nothing was ever going to restart it.
	unowned := unownedFailures(reg, exec)

	// The document's fate is reported against the ConfigMap once per pass
	// rather than once per Site, because that is what it is: one cluster-scoped
	// document, whose failures are not any single Site's.
	r.publishOverrideConfigMapEvent(snapshot, report, exec, overrideEventScope(site))

	var (
		reconcileErrs []error
		requeueAfter  time.Duration
	)

	if overrideErr != nil {
		reconcileErrs = append(reconcileErrs, overrideErr)
	}

	reconcileErrs = append(reconcileErrs, unowned...)

	if len(exec.Stale) > 0 {
		// Something was created out from under this pass, so what it computed
		// from the earlier read no longer describes the cluster. Re-planning is
		// cheap and idempotent; leaving a workload stamped with the hash of a
		// payload that is not there is not.
		logger.V(1).Info("plan was computed from stale state; re-planning",
			"objects", len(exec.Stale))

		requeueAfter = nextRequeue(requeueAfter, stalePlanRequeue)
	}

	if len(exec.Deferred) > 0 {
		// An operation lost an optimistic lock, or depended on something that
		// had moved. Neither is a failure, so neither goes through error
		// backoff; the pass simply runs again against what is actually there.
		logger.V(1).Info("operations were deferred to the next pass",
			"objects", len(exec.Deferred))

		requeueAfter = nextRequeue(requeueAfter, statusConflictRequeue)
	}

	if execErr != nil {
		// A plan the executor refuses to run (a dependency cycle, or
		// contradictory shared operations) is a programming error, not a
		// component outcome, so it is reported once rather than per component.
		reconcileErrs = append(reconcileErrs, fmt.Errorf("execute plan: %w", execErr))
	}

	record := func(target *unboundedv1alpha3.Site, outcome componentOutcome) {
		// A Site-less pass has no Site to attribute results to, so it takes
		// the cluster-scoped operations only.
		var siteName string
		if target != nil {
			siteName = target.Name
		}

		res := component.CombineResult(outcome.name, siteName, outcome.result, exec)

		// A plan the executor refused outright wrote nothing at all, so no
		// component's planning verdict describes the cluster. Reporting each
		// of them Reconciled from a plan that never ran was the one case where
		// every condition could be simultaneously green and wrong.
		if execErr != nil {
			res = component.NotReady(component.ReasonPlanRejected,
				"the operation plan was rejected before execution: "+execErr.Error())
		}

		switch {
		case target != nil:
			if err := setComponentResult(logger, target, outcome.name, outcome.conditionType, res); err != nil {
				reconcileErrs = append(reconcileErrs, err)
			}
		case res.Err != nil:
			reconcileErrs = append(reconcileErrs, fmt.Errorf("%s: %w", outcome.name, res.Err))
		}

		requeueAfter = nextRequeue(requeueAfter, res.RequeueAfter)
	}

	// With no Sites there is nowhere to publish conditions, so cluster outcomes
	// surface only as errors and requeue requests.
	if len(targets) == 0 {
		for _, outcome := range clusterOutcomes {
			record(nil, outcome)
		}

		return ctrl.Result{RequeueAfter: requeueAfter}, errors.Join(reconcileErrs...)
	}

	for _, target := range targets {
		for _, outcome := range clusterOutcomes {
			record(target, outcome)
		}

		for _, outcome := range siteOutcomes[target.Name] {
			record(target, outcome)
		}

		pruneStaleConditions(target, reg)

		// The Event is prepared here but not emitted: it describes a status
		// transition, and the patch below may lose an optimistic lock or fail
		// outright, in which case the transition never happened. Emitting first
		// left users with an Event for a state the Site never reached, and a
		// retry emitted it again.
		emitOverrideEvent := r.prepareOverrideStatus(target, snapshot, report, plan, exec)

		if err := r.Status().Patch(ctx, target, baselines[target.Name]); err != nil {
			// A conflict means someone else wrote this Site's status while the
			// pass was running. That is not a failure and does not deserve an
			// error log or backoff: the pass simply lost, and re-running it
			// against the winner's state is the whole point of the lock.
			if apierrors.IsConflict(err) {
				logger.V(1).Info("site status changed during the pass; retrying",
					"site", target.Name)

				requeueAfter = nextRequeue(requeueAfter, statusConflictRequeue)

				continue
			}

			// The Site was deleted while the pass was running. There is nothing
			// to write the status to and nothing to retry: its owned objects
			// are garbage-collected by owner reference.
			if apierrors.IsNotFound(err) {
				logger.V(1).Info("site was deleted during the pass", "site", target.Name)

				continue
			}

			reconcileErrs = append(reconcileErrs, fmt.Errorf("patch site status for %s: %w", target.Name, err))

			continue
		}

		emitOverrideEvent()
	}

	return ctrl.Result{RequeueAfter: requeueAfter}, errors.Join(reconcileErrs...)
}

// unownedFailures returns the errors from operations no registered component
// owns, so they surface as pass errors rather than vanishing.
//
// Skipped operations are deliberately excluded: the operation that caused them
// is in this list already, and repeating its error once per dependent would
// bury the cause.
func unownedFailures(reg *component.Registry, exec component.ExecutionResult) []error {
	var errs []error

	for _, result := range exec.Results {
		if result.Status != component.OpFailed || result.Err == nil {
			continue
		}

		if reg.Knows(result.Component) {
			continue
		}

		errs = append(errs, fmt.Errorf("%s %s: %w", result.Kind, result.Ref, result.Err))
	}

	return errs
}

// stalePlanRequeue is how soon a pass re-plans after discovering that an object
// it expected to create already existed. Like a status conflict this is not a
// failure, so it does not go through error backoff.
const stalePlanRequeue = time.Second

// statusConflictRequeue is how soon a pass retries after losing a status write
// to a concurrent one. It is short because the conflict is not a failure and
// nothing needs to settle: the winner's write has already landed, and the next
// pass only has to observe it.
const statusConflictRequeue = time.Second

// applyOverrides merges user-supplied overrides into a plan, and removes the
// workloads whose overrides could not be used.
//
// The two halves are independent, which is the point. A document with one bad
// entry still applies every other entry; only the workloads the bad entry could
// have targeted are withheld. A key that failed to parse names nothing, so
// every overridable workload is withheld, because there is no way to know what
// it would have changed.
//
// The returned error requeues the pass. It is deliberately not attributed to a
// single component: the overrides ConfigMap is cluster-scoped and one document
// routinely targets several components, so a document-level failure belongs to
// the pass rather than to whichever component happened to be planned first.
func applyOverrides(logger logr.Logger, plan *component.Plan, snapshot overrideSnapshot, sites []unboundedv1alpha3.Site) (*override.Report, error) {
	withheld := dropOverridableOperations(plan, snapshot.quarantine())

	if len(withheld) > 0 {
		logger.Error(snapshot.failure(), "overrides could not be used; leaving the affected workloads unchanged",
			"configMap", override.ConfigMapName,
			"withheldWorkloads", len(withheld),
			"resourceVersion", snapshot.resourceVersion)
	}

	var report *override.Report

	if snapshot.usable() && len(snapshot.entries) > 0 {
		applied := override.Apply(plan, snapshot.entries, siteNames(sites))
		report = &applied

		logOverrideReport(logger, applied)
	} else {
		report = &override.Report{}
	}

	report.Withheld = append(report.Withheld, withheld...)

	switch {
	case report.Failed():
		return report, fmt.Errorf("overrides could not be applied to some workloads: %w", report.Err())

	case snapshot.rejected():
		// Reported whether or not anything was withheld. The quarantine decides
		// what the operator declines to write; it does not decide whether the
		// user is told their document is wrong. An entry naming a component
		// that is disabled, or not installed, withholds nothing and is still a
		// document that does not say what its author meant.
		return report, fmt.Errorf("overrides were rejected, %d workload(s) left unchanged: %w",
			len(withheld), snapshot.failure())
	}

	return report, nil
}

// logOverrideReport records the outcomes that are worth saying out loud but are
// not failures.
func logOverrideReport(logger logr.Logger, report override.Report) {
	for _, unmatched := range report.UnmatchedSites {
		logger.Info("override names a Site that does not exist; it is inert until the Site is created",
			"site", unmatched, "configMap", override.ConfigMapName)
	}

	// An entry that resolved to nothing is not an error: its component may be
	// disabled, or its Site may not exist yet. It is worth saying so, because
	// otherwise "applied" and "applied to nothing" look identical from the
	// outside.
	for _, inert := range report.InertEntries {
		logger.Info("override entry matched no workload in this pass",
			"entry", inert.String(), "configMap", override.ConfigMapName)
	}

	for _, workload := range report.Workloads {
		if workload.Err != nil {
			continue
		}

		if workload.VersionDrift != "" {
			logger.Info("override changed a container image; this component is no longer version-matched to the operator",
				"workload", workload.Ref.String(), "drift", workload.VersionDrift)
		}
	}
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
	targets []*unboundedv1alpha3.Site,
) ([]componentOutcome, map[string][]componentOutcome, *component.Plan) {
	cluster := make([]componentOutcome, 0, len(reg.Cluster))
	perSite := make(map[string][]componentOutcome, len(targets))
	plan := component.NewPlan()

	// The namespace has one owner rather than one per component. It is planned
	// here rather than by any component because no component owns it: they all
	// ship it, they do not agree on its labels, and applying it under a single
	// field owner from several places made the labels flip on every pass. The
	// executor orders it ahead of everything namespaced regardless of where it
	// appears in the plan.
	plan.Add(component.NamespaceOperation(env.Namespace))

	for _, c := range reg.Cluster {
		componentPlan, res, err := c.Plan(ctx, env, sites)
		if err != nil {
			res = component.Failed(err)
		} else {
			plan.Merge(componentPlan)
		}

		cluster = append(cluster, componentOutcome{name: c.Name(), conditionType: c.ConditionType(), result: res})
	}

	for _, target := range targets {
		outcomes := make([]componentOutcome, 0, len(reg.Site))

		for _, c := range reg.Site {
			componentPlan, res, err := planSiteComponent(ctx, env, c, target)
			if err != nil {
				res = component.Failed(err)
			} else {
				plan.Merge(componentPlan)
			}

			outcomes = append(outcomes, componentOutcome{name: c.Name(), conditionType: c.ConditionType(), result: res})
		}

		perSite[target.Name] = outcomes
	}

	return cluster, perSite, plan
}

// fanOutTargets returns the Sites whose per-Site components this pass runs.
//
// A Site-scoped request reconciles just that Site. The Site-less pass, which a
// singleton request uses, reconciles every Site.
//
// That fan-out is why it exists. The overrides ConfigMap watch enqueues only
// the singleton request, so without it metalman and storage would never see an
// override change. Doing the fan-out here rather than in the watch handler is
// what makes it retryable: a handler that listed Sites at event-delivery time
// would consume the event and lose the fan-out permanently if that List failed.
//
// It is bounded by Site count, which is per-location and small, and every
// operation is idempotent, so re-running costs an apply rather than a change.
func fanOutTargets(site *unboundedv1alpha3.Site, sites []unboundedv1alpha3.Site) []*unboundedv1alpha3.Site {
	if site != nil {
		return []*unboundedv1alpha3.Site{site}
	}

	targets := make([]*unboundedv1alpha3.Site, 0, len(sites))
	for i := range sites {
		targets = append(targets, &sites[i])
	}

	return targets
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
	// A nil APIReader is refused rather than tolerated, for the same reason the
	// legacy reaper refuses one. Env.LiveReader falls back to the cached
	// client, which is fine for a unit test constructing the reconciler
	// directly and is not fine under a manager: a readiness gate reading a
	// lagging cache reports a stale rollout as the current one, and every kind
	// it touches gains an informer it does not otherwise need.
	if r.APIReader == nil {
		return errors.New("site reconciler requires an APIReader: readiness gating must see the " +
			"apiserver rather than the cache (use mgr.GetAPIReader())")
	}

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
		// Pin the worker count rather than inherit controller-runtime's default.
		// A pass reads the overrides ConfigMap once and derives everything from
		// that snapshot, and single-threading is what makes two passes unable to
		// interleave writes to the same workload. It says nothing about external
		// writers: users and other controllers still edit these objects
		// concurrently, which the snapshot model handles instead.
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		For(&unboundedv1alpha3.Site{}, builder.WithPredicates(predicate.GenerationChangedPredicate{}))

	// The overrides ConfigMap spans components, so it is watched here rather
	// than by any one of them.
	//
	// It deliberately does not use RequestSingletonAndAllSites: that handler
	// lists Sites at event-delivery time and, when the List fails, logs and
	// returns only the singleton request. The event is consumed and the per-Site
	// fan-out is lost permanently, with no retry. Enqueuing the singleton
	// request alone keeps the fan-out inside Reconcile, where a failure can be
	// returned and retried with backoff.
	b.Watches(&corev1.ConfigMap{}, env.RequestSingleton(),
		builder.WithPredicates(env.ManagedConfigPredicate(env.InNamespaceNamed(override.ConfigMapName))))

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

// publishOverrideStatus records the Site's override state and emits an Event
// when it changes.
//
// Events fire on a change of hash or phase rather than on every reconcile, so
// a steady state is quiet. They are emitted against the Site here; the
// overrides ConfigMap gets its own Events for failures that no Site can carry,
// including the case where no Site exists at all.
func (r *SiteReconciler) prepareOverrideStatus(
	site *unboundedv1alpha3.Site,
	snapshot overrideSnapshot,
	report *override.Report,
	plan *component.Plan,
	exec component.ExecutionResult,
) func() {
	status := overrideStatusFor(site.Name, snapshot, report, plan, exec)

	previous := site.Status.Overrides
	site.Status.Overrides = status

	if r.Recorder == nil || !overrideStatusChanged(previous, status) {
		return func() {}
	}

	switch status.Phase {
	case unboundedv1alpha3.OverridePhaseDegraded:
		return func() {
			r.Recorder.Eventf(site, nil, corev1.EventTypeWarning,
				"OverridesDegraded", "ApplyOverrides", "%s", status.Message)
		}
	case unboundedv1alpha3.OverridePhaseApplied:
		return func() {
			r.Recorder.Eventf(site, nil, corev1.EventTypeNormal,
				"OverridesApplied", "ApplyOverrides", "%d workload(s) overridden", len(status.Workloads))
		}
	}

	return func() {}
}

// overrideStatusChanged reports whether anything worth an Event changed.
func overrideStatusChanged(previous, current *unboundedv1alpha3.OverrideStatus) bool {
	if previous == nil {
		return current != nil && current.Phase != unboundedv1alpha3.OverridePhaseNone
	}

	if current == nil {
		return true
	}

	if previous.Phase != current.Phase || len(previous.Workloads) != len(current.Workloads) {
		return true
	}

	for i := range current.Workloads {
		if previous.Workloads[i] != current.Workloads[i] {
			return true
		}
	}

	return false
}

// publishOverrideConfigMapEvent records the fate of the overrides document
// against the ConfigMap itself.
//
// Site conditions are the wrong and sometimes the only wrong place for this.
// The overrides ConfigMap is cluster-scoped and one document routinely targets
// several components, so a document-level failure does not belong to any one
// Site; worse, when no Site exists yet there is no Site to carry it at all, and
// a user who mistyped a patch would get nothing but an operator log line. The
// object the user edited is where they will look, and where `kubectl describe`
// will show it.
//
// Events fire only when the observed resourceVersion or the verdict changes, so
// a broken document that keeps requeueing does not produce an Event per pass.
func (r *SiteReconciler) publishOverrideConfigMapEvent(
	snapshot overrideSnapshot,
	report *override.Report,
	exec component.ExecutionResult,
	scope string,
) {
	if r.Recorder == nil || snapshot.configMap == nil {
		return
	}

	eventType, reason, note := overrideConfigMapEvent(snapshot, report, exec)
	if reason == "" {
		return
	}

	if !r.overrideEventIsNew(snapshot.resourceVersion, reason, scope, note) {
		return
	}

	r.Recorder.Eventf(snapshot.configMap, nil, eventType, reason, "ApplyOverrides", "%s", note)
}

// overrideEventScope names how much of the cluster a pass looked at.
//
// A pass for one Site resolves overrides against that Site alone, so its
// verdict describes part of the document. Deduplicating on the document version
// and verdict alone let the first such pass claim "1 workload overridden" for a
// two-Site document and then suppress the complete fan-out verdict that
// followed, because both were Normal OverridesApplied at the same
// resourceVersion.
func overrideEventScope(site *unboundedv1alpha3.Site) string {
	if site == nil {
		return "all-sites"
	}

	return "site/" + site.Name
}

// overrideConfigMapEvent chooses the Event for a snapshot, or returns an empty
// reason when the document deserves no Event.
func overrideConfigMapEvent(
	snapshot overrideSnapshot,
	report *override.Report,
	exec component.ExecutionResult,
) (eventType, reason, note string) {
	if report == nil {
		return "", "", ""
	}

	// A ConfigMap that exists but asks for nothing deserves no Event. Without
	// this, a document with no entries would produce a Normal "0 workload(s)
	// overridden" every time its resourceVersion changed.
	if len(report.Withheld) == 0 && len(report.Workloads) == 0 && len(report.InertEntries) == 0 {
		return "", "", ""
	}

	// A rejected document outranks everything below. This Event is the only
	// verdict a user gets when no Site exists, and the ConfigMap is the object
	// they edited, so it has to say plainly that the document is wrong even
	// when nothing was withheld.
	if snapshot.rejected() {
		reason := "OverridesRejected"
		if len(report.Workloads) > 0 {
			// Some entries did apply, so calling the whole document rejected
			// would misdescribe what happened to the rest of it.
			reason = "OverridesPartiallyRejected"
		}

		return corev1.EventTypeWarning, reason,
			fmt.Sprintf("%d workload(s) were left unchanged: %v", len(report.Withheld), snapshot.failure())
	}

	if err := report.Err(); err != nil {
		return corev1.EventTypeWarning, "OverridesPartiallyApplied", err.Error()
	}

	// A successful merge is not a successful override. The write can still be
	// rejected by admission, fail against the API server, or be skipped because
	// something it depends on failed, and with no Sites this Event is the only
	// verdict a user gets.
	if unwritten := unwrittenOverrides(report, exec); len(unwritten) > 0 {
		return corev1.EventTypeWarning, "OverridesNotWritten",
			fmt.Sprintf("%d of %d overridden workload(s) were not written: %s",
				len(unwritten), len(report.Workloads), strings.Join(unwritten, "; "))
	}

	if len(report.InertEntries) > 0 {
		return corev1.EventTypeNormal, "OverridesApplied",
			fmt.Sprintf("%d workload(s) overridden; %d entr%s matched nothing (%s)",
				len(report.Workloads), len(report.InertEntries),
				map[bool]string{true: "y", false: "ies"}[len(report.InertEntries) == 1],
				describeSources(report.InertEntries))
	}

	return corev1.EventTypeNormal, "OverridesApplied",
		fmt.Sprintf("%d workload(s) overridden", len(report.Workloads))
}

// unwrittenOverrides names the overridden workloads the executor did not write,
// rendered for an Event.
func unwrittenOverrides(report *override.Report, exec component.ExecutionResult) []string {
	outcome := map[component.ObjectRef]component.OperationResult{}

	for _, result := range exec.Results {
		// A failure or a skip outranks a success for the same object, since a
		// plan may hold more than one operation on it.
		if existing, seen := outcome[result.Ref]; seen && existing.Status != component.OpSucceeded {
			continue
		}

		outcome[result.Ref] = result
	}

	var unwritten []string

	for _, workload := range report.Workloads {
		if workload.Err != nil {
			continue
		}

		result, ran := outcome[workload.Ref]
		switch {
		case !ran:
			unwritten = append(unwritten, workload.Ref.String()+" (never executed)")
		case result.Status == component.OpDeferred:
			// Transient by construction: the pass requeues and the next one
			// writes it. Warning about a race that resolves in a second would
			// train users to ignore this Event.
			continue
		case result.Status != component.OpSucceeded:
			reason := result.Status.String()
			if result.Err != nil {
				reason = result.Err.Error()
			}

			unwritten = append(unwritten, workload.Ref.String()+" ("+reason+")")
		}
	}

	sort.Strings(unwritten)

	return unwritten
}

// describeSources renders entry sources for a message, capped so one Event
// cannot carry an unbounded list.
func describeSources(sources []override.Source) string {
	const max = 5

	rendered := make([]string, 0, max)

	for i, source := range sources {
		if i == max {
			rendered = append(rendered, fmt.Sprintf("+%d more", len(sources)-max))

			break
		}

		rendered = append(rendered, source.String())
	}

	return strings.Join(rendered, ", ")
}

// overrideEventIsNew reports whether this verdict has already been recorded for
// this version of the document, and remembers it when it has not.
func (r *SiteReconciler) overrideEventIsNew(resourceVersion, reason, scope, note string) bool {
	r.overrideEventMu.Lock()
	defer r.overrideEventMu.Unlock()

	// The scope and the note both take part, so a partial verdict from a
	// single-Site pass cannot suppress the complete one that follows it.
	current := strings.Join([]string{resourceVersion, reason, scope, note}, "/")
	if r.lastOverrideEvent == current {
		return false
	}

	r.lastOverrideEvent = current

	return true
}
