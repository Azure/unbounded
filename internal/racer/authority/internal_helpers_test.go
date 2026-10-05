// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authority

import (
	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/members"
	"github.com/Azure/unbounded/internal/racer/wire"
)

func trustReady(trust *Trust) bool {
	_, err := trust.pool()
	return err == nil
}

func BuildCatalog(caches []racerv1.ClusterCache) ([]wire.CacheDefinition, error) {
	return members.BuildCatalog(caches)
}
