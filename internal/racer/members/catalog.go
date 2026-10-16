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

// ReadCatalog builds the cache catalog from installed ClusterCaches.
// A missing CRD is allowed; authorization and other read failures are not.
func ReadCatalog(ctx context.Context, reader client.Reader) ([]wire.CacheDefinition, error) {
	var caches racerv1.ClusterCacheList
	if err := reader.List(ctx, &caches); err != nil && !meta.IsNoMatchError(err) && !apierrors.IsNotFound(err) {
		return nil, CatalogReadError{err}
	}

	return BuildCatalog(caches.Items)
}

// CatalogReadError distinguishes unavailable observations from invalid catalogs.
type CatalogReadError struct{ error }

func (e CatalogReadError) Unwrap() error { return e.error }
