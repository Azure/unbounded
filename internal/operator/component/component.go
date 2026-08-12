// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/builder"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// ClusterComponent is a cluster-wide unit of desired state (for example net or
// machina). It reconciles once per pass from the full set of Sites and is never
// tied to a single Site, so it also runs on the Site-less pass (a Site deletion
// or a managed singleton-resource event).
//
// Plan runs on every pass and must be idempotent. A not-ready Result does
// not by itself schedule a retry: the driver requeues on Result.Err or after
// Result.RequeueAfter, otherwise the component only re-runs when one of the
// watches it registers via WatchProvider fires. On the Site-less pass there is
// no Site status to publish, so a not-ready Result there is meaningful only
// through its Err or RequeueAfter.
type ClusterComponent interface {
	// Name is the stable, unique component identifier (for example "net"). It is
	// used in logs and must be unique within a Registry.
	Name() string

	// ConditionType is the Site status condition type published for this
	// component (for example "NetReady"). It must be CamelCase alphanumeric and
	// unique within a Registry.
	ConditionType() string

	// Plan computes the operations that would drive the component to its
	// desired state given every Site, and reports the planning verdict.
	//
	// Planning may read cluster state, because decisions like "does this
	// ConfigMap already exist" and "is this singleton retained" depend on it.
	// Planning must not write: that is the executor's job, and it is what lets
	// a pass validate everything before anything is written.
	//
	// The Result carries verdicts only planning can reach, such as Disabled or
	// NoSites. The driver folds execution outcomes into it.
	Plan(ctx context.Context, env *Env, sites []unboundedv1alpha3.Site) (*Plan, Result, error)
}

// SiteComponent is a per-Site unit of desired state (for example metalman or
// storage). The SiteReconciler runs it only when a Site is present, so
// Plan and CleanupPlan always receive a non-nil Site. The driver owns the
// enable/disable branch: it calls Plan when Enabled reports true and
// CleanupPlan when it reports false (or the Site is deleted via owner-reference
// garbage collection).
//
// Plan runs on every Site event and must be idempotent. To self-heal owned
// resources on drift or deletion, set a controller owner reference (see
// Env.SiteOwnerReference) and register Owns via WatchProvider; a not-ready
// Result requeues only through Result.Err or Result.RequeueAfter.
type SiteComponent interface {
	// Name is the stable, unique component identifier (for example "metalman").
	Name() string

	// ConditionType is the Site status condition type published for this
	// component (for example "MetalmanReady").
	ConditionType() string

	// Enabled reports whether the component should run for the Site.
	Enabled(site *unboundedv1alpha3.Site) bool

	// Plan computes the operations that would apply the component's desired
	// state for the Site. The same read-not-write rule as ClusterComponent.Plan
	// applies.
	Plan(ctx context.Context, env *Env, site *unboundedv1alpha3.Site) (*Plan, Result, error)

	// CleanupPlan computes the operations that remove the component's per-Site
	// resources when it is disabled.
	CleanupPlan(ctx context.Context, env *Env, site *unboundedv1alpha3.Site) (*Plan, Result, error)
}

// WatchProvider is implemented by a ClusterComponent or SiteComponent that owns
// or watches resources whose churn should re-trigger its reconcile. The
// SiteReconciler calls SetupWatches on every component that implements it when
// wiring the controller.
type WatchProvider interface {
	SetupWatches(b *builder.Builder, env *Env)
}

// Registry is the ordered set of components the SiteReconciler drives. Cluster
// components run in every pass; Site components run only when a Site is present.
// Conditions are published Cluster-first then Site, so the order of these slices
// is the stable condition order on the Site status.
type Registry struct {
	Cluster []ClusterComponent
	Site    []SiteComponent
}

// Knows reports whether a component name belongs to this registry.
//
// The executor also runs operations no component owns, such as the namespace
// every component installs into. Their results are attributed to a name the
// registry does not know, so nothing publishing per-component conditions can
// report them, and without this the reconciler had no way to notice.
func (r *Registry) Knows(name string) bool {
	for _, c := range r.Cluster {
		if c.Name() == name {
			return true
		}
	}

	for _, c := range r.Site {
		if c.Name() == name {
			return true
		}
	}

	return false
}

// Validate rejects an empty registry and duplicate Name or ConditionType values
// across both lists, so a misconfigured registry fails fast at startup rather
// than silently dropping or double-publishing a condition.
func (r *Registry) Validate() error {
	if len(r.Cluster)+len(r.Site) == 0 {
		return fmt.Errorf("registry has no components")
	}

	names := map[string]struct{}{}
	conditions := map[string]struct{}{}

	register := func(kind, name, condition string) error {
		if name == "" {
			return fmt.Errorf("%s component has an empty name", kind)
		}

		if condition == "" {
			return fmt.Errorf("%s component %q has an empty condition type", kind, name)
		}

		if _, exists := names[name]; exists {
			return fmt.Errorf("component %q is registered more than once", name)
		}

		if _, exists := conditions[condition]; exists {
			return fmt.Errorf("condition type %q is registered more than once", condition)
		}

		names[name] = struct{}{}
		conditions[condition] = struct{}{}

		return nil
	}

	for _, c := range r.Cluster {
		if err := register("cluster", c.Name(), c.ConditionType()); err != nil {
			return err
		}
	}

	for _, c := range r.Site {
		if err := register("site", c.Name(), c.ConditionType()); err != nil {
			return err
		}
	}

	return nil
}
