// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/wire"
)

const (
	podNodeIndex       = "spec.nodeName"
	retryConflictDelay = 10 * time.Millisecond
)

// Controllers start sources only under leadership and wait for cache sync before
// workers run. Queueing directly guarantees a first reconcile even for empty lists.
func initialEnqueue() source.Source {
	return source.Func(func(ctx context.Context, q workqueue.TypedRateLimitingInterface[reconcile.Request]) error {
		if err := ctx.Err(); err != nil {
			return err
		}

		q.Add(singleton(ctx, nil)[0])

		return nil
	})
}

func podNodeKeys(obj client.Object) []string {
	pod, ok := obj.(*corev1.Pod)
	if !ok || pod.Spec.NodeName == "" {
		return nil
	}

	return []string{pod.Spec.NodeName}
}

func changes(relevant func(client.Object) bool, equal func(client.Object, client.Object) bool) predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return relevant(e.Object) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return relevant(e.Object) },
		GenericFunc: func(e event.GenericEvent) bool { return relevant(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return (relevant(e.ObjectOld) || relevant(e.ObjectNew)) && !equal(e.ObjectOld, e.ObjectNew)
		},
	}
}

func nodeChanges() predicate.Predicate {
	return changes(func(client.Object) bool { return true }, func(a, b client.Object) bool {
		_, excludedA := a.GetLabels()[wire.ExclusionLabel]

		_, excludedB := b.GetLabels()[wire.ExclusionLabel]
		if a.GetUID() != b.GetUID() || a.GetName() != b.GetName() || excludedA != excludedB {
			return false
		}

		for _, key := range []string{wire.SharesAnnotation, wire.RailsAnnotation, wire.AlignmentAnnotation} {
			av, ap := a.GetAnnotations()[key]

			bv, bp := b.GetAnnotations()[key]
			if ap != bp || av != bv {
				return false
			}
		}

		return true
	})
}

func managedPodChanges(cfg Config) predicate.Predicate {
	return changes(func(obj client.Object) bool {
		if obj.GetNamespace() != cfg.Namespace {
			return false
		}

		owner := metav1.GetControllerOf(obj)

		return owner != nil && owner.APIVersion == "apps/v1" && owner.Kind == "DaemonSet" && owner.Name == cfg.DaemonSetName
	}, func(a, b client.Object) bool {
		x, xok := a.(*corev1.Pod)

		y, yok := b.(*corev1.Pod)
		if !xok || !yok {
			return false
		}

		return x.UID == y.UID && x.Spec.NodeName == y.Spec.NodeName && x.Status.PodIP == y.Status.PodIP && x.CreationTimestamp.Equal(&y.CreationTimestamp) && reflect.DeepEqual(x.DeletionTimestamp, y.DeletionTimestamp) && reflect.DeepEqual(x.OwnerReferences, y.OwnerReferences)
	})
}

func cacheChanges() predicate.Predicate {
	return changes(func(client.Object) bool { return true }, func(a, b client.Object) bool {
		x, xok := a.(*racerv1.ClusterCache)

		y, yok := b.(*racerv1.ClusterCache)
		if !xok || !yok {
			return false
		}

		return x.UID == y.UID && x.Name == y.Name && reflect.DeepEqual(x.Spec, y.Spec)
	})
}

func namedChanges(namespace string, names ...string) predicate.Predicate {
	return changes(func(obj client.Object) bool {
		if obj.GetNamespace() != namespace {
			return false
		}

		for _, name := range names {
			if obj.GetName() == name {
				return true
			}
		}

		return false
	}, func(a, b client.Object) bool { return a.GetResourceVersion() == b.GetResourceVersion() })
}

// Ignore our own resource-version-only CAS writes to avoid an endless hot loop.
func versionChanges(cfg Config) predicate.Predicate {
	named := namedChanges(cfg.Namespace, cfg.VersionConfigMapName, cfg.InstallationConfigMapName)

	return changes(func(obj client.Object) bool { return named.Generic(event.GenericEvent{Object: obj}) }, func(a, b client.Object) bool {
		x, xok := a.(*corev1.ConfigMap)

		y, yok := b.(*corev1.ConfigMap)
		if !xok || !yok {
			return false
		}

		return x.UID == y.UID && reflect.DeepEqual(x.Data, y.Data) && reflect.DeepEqual(x.Annotations, y.Annotations) && reflect.DeepEqual(x.Immutable, y.Immutable) && reflect.DeepEqual(x.DeletionTimestamp, y.DeletionTimestamp)
	})
}
