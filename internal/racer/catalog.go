// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
)

func volumeChanges() predicate.Predicate {
	return changes(func(client.Object) bool { return true }, func(a, b client.Object) bool {
		x, xok := a.(*racerv1.ClusterVolume)

		y, yok := b.(*racerv1.ClusterVolume)
		if !xok || !yok {
			return false
		}

		return x.UID == y.UID && x.Name == y.Name && x.Spec.Type == y.Spec.Type
	})
}

// Catalog CRDs must be installed before the manager starts. Either kind may
// be absent, but every installed kind must be readable by the controller.
func watchCatalog(mgr ctrl.Manager, b *builder.Builder) error {
	for _, item := range []struct {
		kind      string
		object    client.Object
		predicate predicate.Predicate
	}{
		{"ClusterCache", &racerv1.ClusterCache{}, cacheChanges()},
		{"ClusterVolume", &racerv1.ClusterVolume{}, volumeChanges()},
	} {
		if _, err := mgr.GetRESTMapper().RESTMapping(racerv1.GroupVersion.WithKind(item.kind).GroupKind(), racerv1.GroupVersion.Version); err != nil {
			if meta.IsNoMatchError(err) {
				continue
			}

			return err
		}

		b.Watches(item.object, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(item.predicate))
	}

	return nil
}
