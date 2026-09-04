// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package component

import (
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/Azure/unbounded/internal/unbounded"
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
// and for updates when the generation or the operator's own metadata changed.
func (e *Env) ManagedWorkloadPredicate(match func(client.Object) bool) predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(ev event.CreateEvent) bool { return match(ev.Object) },
		DeleteFunc: func(ev event.DeleteEvent) bool { return match(ev.Object) },
		UpdateFunc: func(ev event.UpdateEvent) bool {
			return match(ev.ObjectNew) && workloadChanged(ev)
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// workloadChanged reports whether an update is worth a reconcile.
//
// Generation covers the spec, which is where everything functional lives.
// It does not cover top-level metadata: Kubernetes increments generation for
// spec changes only, so removing the override hash annotation, or editing a
// label the operator set, produced no event and no pass. The operator would
// have repaired it, because server-side apply reclaims fields it declares, but
// nothing was going to ask it to, so a Site could report Applied indefinitely
// for an object no longer carrying any sign of it.
//
// Only the operator's own reserved-prefix keys are compared. Firing on any
// metadata change would be the obvious fix and a bad one: the Deployment
// controller writes deployment.kubernetes.io/revision to top-level annotations
// on every rollout, and kubectl writes its last-applied configuration, so the
// predicate would fire on exactly the churn it exists to suppress.
//
// The residual gap is drift in user-chosen labels an override sets, which is
// repaired whenever any later pass runs rather than immediately. Closing that
// needs a periodic resync, which is a deliberate feature with a cluster-wide
// cost rather than something to smuggle in here.
func workloadChanged(ev event.UpdateEvent) bool {
	if ev.ObjectOld.GetGeneration() != ev.ObjectNew.GetGeneration() {
		return true
	}

	return reservedMetadataChanged(ev.ObjectOld, ev.ObjectNew)
}

// reservedMetadataChanged reports whether any unbounded-cloud.io/ label or
// annotation differs between two versions of an object.
func reservedMetadataChanged(older, newer client.Object) bool {
	return reservedKeysDiffer(older.GetAnnotations(), newer.GetAnnotations()) ||
		reservedKeysDiffer(older.GetLabels(), newer.GetLabels())
}

func reservedKeysDiffer(older, newer map[string]string) bool {
	for key, value := range older {
		if strings.HasPrefix(key, unbounded.ReservedPrefix) && newer[key] != value {
			return true
		}
	}

	for key, value := range newer {
		if strings.HasPrefix(key, unbounded.ReservedPrefix) && older[key] != value {
			return true
		}
	}

	return false
}

// OwnedWorkloadPredicate fires for create and delete of owned workloads, and for
// updates when the generation or the operator's own metadata changed. It carries
// no name match because the Owns() enqueue handler already scopes events to
// resources owned by the Site; the predicate exists only to drop status-only
// churn (pod counts) so drift and deletion self-heal without re-applying on
// every status update. See workloadChanged.
func (e *Env) OwnedWorkloadPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		UpdateFunc:  workloadChanged,
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}
