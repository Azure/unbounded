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
type ClusterComponent interface {
	// Name is the stable, unique component identifier (for example "net"). It is
	// used in logs and must be unique within a Registry.
	Name() string

	// ConditionType is the Site status condition type published for this
	// component (for example "NetReady"). It must be CamelCase alphanumeric and
	// unique within a Registry.
	ConditionType() string

	// Reconcile drives the component to its desired state given every Site and
	// reports the outcome.
	Reconcile(ctx context.Context, env *Env, sites []unboundedv1alpha3.Site) Result
}

// SiteComponent is a per-Site unit of desired state (for example metalman or
// storage). The SiteReconciler runs it only when a Site is present, so
// Reconcile and Cleanup always receive a non-nil Site. The driver owns the
// enable/disable branch: it calls Reconcile when Enabled reports true and
// Cleanup when it reports false (or the Site is deleted via owner-reference
// garbage collection).
type SiteComponent interface {
	// Name is the stable, unique component identifier (for example "metalman").
	Name() string

	// ConditionType is the Site status condition type published for this
	// component (for example "MetalmanReady").
	ConditionType() string

	// Enabled reports whether the component should run for the Site.
	Enabled(site *unboundedv1alpha3.Site) bool

	// Reconcile applies the component's desired state for the Site.
	Reconcile(ctx context.Context, env *Env, site *unboundedv1alpha3.Site) Result

	// Cleanup removes the component's per-Site resources when it is disabled.
	Cleanup(ctx context.Context, env *Env, site *unboundedv1alpha3.Site) error
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
