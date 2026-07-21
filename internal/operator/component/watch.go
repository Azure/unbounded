// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// RequestSingleton enqueues the synthetic singleton request. Cluster components
// use it to map events on their managed singleton workloads back to a reconcile
// that reconciles cluster-wide state without any specific Site.
func (e *Env) RequestSingleton() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []ctrl.Request {
		return e.singletonRequest()
	})
}

func (e *Env) singletonRequest() []ctrl.Request {
	return []ctrl.Request{{NamespacedName: client.ObjectKey{Name: SingletonRequestName}}}
}

// RequestSingletonAndAllSites enqueues the singleton request plus every Site, so
// a change to a shared cluster config re-reconciles cluster state and refreshes
// each Site's status conditions.
func (e *Env) RequestSingletonAndAllSites() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []ctrl.Request {
		return e.singletonAndAllSiteRequests(ctx)
	})
}

func (e *Env) singletonAndAllSiteRequests(ctx context.Context) []ctrl.Request {
	requests := e.singletonRequest()

	sites, err := e.ListSites(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "list Sites after managed config change")

		return requests
	}

	for i := range sites {
		requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKey{Name: sites[i].Name}})
	}

	return requests
}

// InNamespaceNamed returns a matcher for objects in the operator namespace whose
// name is one of names.
func (e *Env) InNamespaceNamed(names ...string) func(client.Object) bool {
	namespace := e.Namespace

	return func(obj client.Object) bool {
		if obj.GetNamespace() != namespace {
			return false
		}

		for _, name := range names {
			if obj.GetName() == name {
				return true
			}
		}

		return false
	}
}

// RequestSiteFromConfigName maps a per-site ConfigMap named "<prefix><site>"
// back to a reconcile request for that Site. Names without the prefix, or with
// an empty site suffix, are ignored.
func (e *Env) RequestSiteFromConfigName(prefix string) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []ctrl.Request {
		site := strings.TrimPrefix(obj.GetName(), prefix)
		if site == obj.GetName() || site == "" {
			return nil
		}

		return []ctrl.Request{{NamespacedName: client.ObjectKey{Name: site}}}
	})
}

// InNamespaceWithPrefix returns a matcher for objects in the operator namespace
// whose name carries prefix followed by a non-empty suffix (a per-site config).
func (e *Env) InNamespaceWithPrefix(prefix string) func(client.Object) bool {
	namespace := e.Namespace

	return func(obj client.Object) bool {
		return obj.GetNamespace() == namespace &&
			strings.HasPrefix(obj.GetName(), prefix) &&
			strings.TrimPrefix(obj.GetName(), prefix) != ""
	}
}

// ManagedConfigPredicate fires for create and delete of ConfigMaps that match,
// and for updates only when the ConfigMap payload actually changed. It filters
// out status/metadata-only churn so a component is not re-applied on every event.
func (e *Env) ManagedConfigPredicate(match func(client.Object) bool) predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(ev event.CreateEvent) bool { return match(ev.Object) },
		DeleteFunc: func(ev event.DeleteEvent) bool { return match(ev.Object) },
		UpdateFunc: func(ev event.UpdateEvent) bool {
			if !match(ev.ObjectNew) {
				return false
			}

			oldConfig, oldOK := ev.ObjectOld.(*corev1.ConfigMap)

			newConfig, newOK := ev.ObjectNew.(*corev1.ConfigMap)
			if !oldOK || !newOK {
				return false
			}

			return ConfigMapPayloadChanged(oldConfig, newConfig)
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// ManagedWorkloadPredicate fires for create and delete of workloads that match,
// and for updates only when the workload generation changed.
func (e *Env) ManagedWorkloadPredicate(match func(client.Object) bool) predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(ev event.CreateEvent) bool { return match(ev.Object) },
		DeleteFunc: func(ev event.DeleteEvent) bool { return match(ev.Object) },
		UpdateFunc: func(ev event.UpdateEvent) bool {
			return match(ev.ObjectNew) && ev.ObjectOld.GetGeneration() != ev.ObjectNew.GetGeneration()
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// OwnedWorkloadPredicate fires for create and delete of owned workloads, and for
// updates only when the generation changed. It carries no name match because the
// Owns() enqueue handler already scopes events to resources owned by the Site;
// the predicate exists only to drop status-only churn (pod counts) so drift and
// deletion self-heal without re-applying on every status update.
func (e *Env) OwnedWorkloadPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return true },
		DeleteFunc: func(event.DeleteEvent) bool { return true },
		UpdateFunc: func(ev event.UpdateEvent) bool {
			return ev.ObjectOld.GetGeneration() != ev.ObjectNew.GetGeneration()
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}
