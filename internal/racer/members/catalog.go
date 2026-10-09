// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package members

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/wire"
)

// BuildVolumeCatalog selects Cache volumes without changing their Kubernetes UID.
func BuildVolumeCatalog(volumes []racerv1.ClusterVolume) ([]wire.CacheDefinition, error) {
	return BuildCatalog(cacheVolumes(volumes))
}

func cacheVolumes(volumes []racerv1.ClusterVolume) []racerv1.ClusterCache {
	caches := make([]racerv1.ClusterCache, 0, len(volumes))
	for _, volume := range volumes {
		if volume.Spec.Type == racerv1.ClusterVolumeTypeCache {
			caches = append(caches, racerv1.ClusterCache{ObjectMeta: volume.ObjectMeta})
		}
	}

	return caches
}

// ReadCatalog accepts either API and rejects collisions across both kinds.
// A missing CRD is allowed; authorization and other read failures are not.
func ReadCatalog(ctx context.Context, reader client.Reader) ([]wire.CacheDefinition, error) {
	var caches racerv1.ClusterCacheList
	if err := reader.List(ctx, &caches); err != nil && !meta.IsNoMatchError(err) && !apierrors.IsNotFound(err) {
		return nil, CatalogReadError{err}
	}

	var volumes racerv1.ClusterVolumeList
	if err := reader.List(ctx, &volumes); err != nil && !meta.IsNoMatchError(err) && !apierrors.IsNotFound(err) {
		return nil, CatalogReadError{err}
	}

	return BuildCatalog(append(caches.Items, cacheVolumes(volumes.Items)...))
}

// CatalogReadError distinguishes unavailable observations from invalid catalogs.
type CatalogReadError struct{ error }

func (e CatalogReadError) Unwrap() error { return e.error }
