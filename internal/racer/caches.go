// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"cmp"
	"fmt"
	"slices"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/wire"
)

// CanonicalSocketPaths validates a safe name and the complete Linux UDS length.
func CanonicalSocketPaths(name string) (clientPath, originPath string, err error) {
	return wire.CanonicalSocketPaths(name)
}

// BuildCatalog derives identities from UIDs and paths from names, sorted by UID.
// An invalid catalog never partially replaces the currently served publication.
func BuildCatalog(caches []racerv1.ClusterCache) ([]wire.CacheDefinition, error) {
	catalog := make([]wire.CacheDefinition, 0, len(caches))
	ids := make(map[wire.CacheID]bool, len(caches))

	names := make(map[string]bool, len(caches))
	for _, cache := range caches {
		id := wire.CacheID(cache.UID)
		if !wire.ValidUUID(string(id)) || ids[id] || names[cache.Name] {
			return nil, fmt.Errorf("cache identity: %w", wire.InvalidRequest)
		}

		client, origin, err := CanonicalSocketPaths(cache.Name)
		if err != nil {
			return nil, fmt.Errorf("cache socket paths: %w", err)
		}

		mode := int32(0o660)
		if cache.Spec.SocketMode != nil {
			mode = *cache.Spec.SocketMode
		}

		if mode < 0 || mode > 0o777 {
			return nil, fmt.Errorf("cache socket mode: %w", wire.InvalidRequest)
		}

		ids[id], names[cache.Name] = true, true
		catalog = append(catalog, wire.CacheDefinition{
			ID: id, Name: cache.Name, ClientSocket: client, OriginSocket: origin, SocketMode: uint32(mode),
		})
	}

	slices.SortFunc(catalog, func(a, b wire.CacheDefinition) int { return cmp.Compare(a.ID, b.ID) })

	return catalog, nil
}
