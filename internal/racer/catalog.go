// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
)

// The ClusterCache CRD must be installed before the manager starts. It may be
// absent, but when installed it must be readable by the controller.
func watchCatalog(mgr ctrl.Manager, b *builder.Builder) error {
	if _, err := mgr.GetRESTMapper().RESTMapping(racerv1.GroupVersion.WithKind("ClusterCache").GroupKind(), racerv1.GroupVersion.Version); err != nil {
		if meta.IsNoMatchError(err) {
			return nil
		}

		return err
	}

	b.Watches(&racerv1.ClusterCache{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(cacheChanges()))

	return nil
}
