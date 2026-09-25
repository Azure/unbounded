// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/wire"
)

// CanonicalSocketPaths validates a safe name and the complete Linux UDS length.
func CanonicalSocketPaths(_ string) (clientPath, originPath string, err error) {
	return "", "", pending("caches.paths")
}

// BuildCatalog derives identities from UIDs and paths from names, sorted by UID.
// An invalid catalog never partially replaces the currently served publication.
func BuildCatalog(_ []racerv1.ClusterCache) ([]wire.CacheDefinition, error) {
	return nil, pending("caches.catalog")
}
