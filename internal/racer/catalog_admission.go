// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/Azure/unbounded/internal/racer/wire"
)

// Reserve a conservative DER ceiling for generated Ed25519 roots, including
// serial-number and ASN.1 time length variation. generateIssuer enforces it.
const reservedRootBytes = 1024

// catalogCapacity reserves active + prepared + ceil(retention / cycle) retiring
// generations. Actual transitions are at least Interval+PrepareFor apart. The
// extra prepared slot is reserved even when the oldest retiree expires before
// preparation. This deliberately favors a stable limit over phase-dependent fit.
func catalogCapacity(cfg Config, b wire.KeyringBundle) (int, error) {
	cycle := cfg.Rotation.Interval + cfg.Rotation.PrepareFor

	retiring := cfg.Rotation.RetainFor / cycle
	if cfg.Rotation.RetainFor%cycle != 0 {
		retiring++
	}

	generations := retiring + 2

	rootBytes := reservedRootBytes
	for _, root := range b.PeerTrustRoots {
		rootBytes = max(rootBytes, len(root))
	}

	rootCost := base64.StdEncoding.EncodedLen(rootBytes) + 3 // quotes and comma
	if int64(generations) > int64(wire.MaxBundleBytes/rootCost) {
		return 0, fmt.Errorf("rotation trust reserve: %w", wire.TooLarge)
	}

	// Measure the wire envelope and fixed-width key pair through the real codec.
	// Reserve all 20 generation digits and the longest key state spelling.
	probe := wire.KeyringBundle{SchemaVersion: wire.SchemaVersion, Cluster: cfg.Cluster, Generation: math.MaxUint64, PeerTrustRoots: b.PeerTrustRoots[:1]}

	empty, err := wire.EncodeBundle(probe)
	if err != nil {
		return 0, err
	}

	for _, purpose := range []wire.KeyPurpose{wire.PageKey, wire.OriginCredentialsKey} {
		key, err := wire.NewCacheKey(wire.CacheKeyRef{Cache: wire.CacheID(cfg.Cluster), Purpose: purpose, ID: make([]byte, 16)}, wire.ActiveKey, [32]byte{})
		if err != nil {
			return 0, err
		}

		probe.CacheKeys = append(probe.CacheKeys, key)
	}

	withKeys, err := wire.EncodeBundle(probe)
	if err != nil {
		return 0, err
	}

	pairCost := len(withKeys) - len(empty) + 1 + 2*(len(wire.RetiringKey)-len(wire.ActiveKey))
	envelope := len(empty) - base64.StdEncoding.EncodedLen(len(probe.PeerTrustRoots[0])) - 2

	available := wire.MaxBundleBytes - envelope - int(generations)*rootCost
	if available < 0 {
		return 0, fmt.Errorf("rotation trust reserve: %w", wire.TooLarge)
	}

	return available / (int(generations) * pairCost), nil
}

// keyedCaches is the durable admission record: both active purposes must exist.
// No process-local admission history is needed across leader changes.
func keyedCaches(b wire.KeyringBundle) map[wire.CacheID]bool {
	purposes := map[wire.CacheID]int{}

	for _, key := range b.CacheKeys {
		if key.State == wire.ActiveKey {
			purposes[key.Key.Cache]++
		}
	}

	ids := make(map[wire.CacheID]bool, len(purposes))
	for id, count := range purposes {
		ids[id] = count == 2
	}

	return ids
}

// admitCatalog retains existing UIDs before filling free slots in BuildCatalog's
// UID order. New low UIDs cannot evict working caches. Deletion frees a slot;
// recreation is a new identity. Rejections are input diagnostics, not key errors.
func admitCatalog(ctx context.Context, cfg Config, catalog []wire.CacheDefinition, b wire.KeyringBundle) ([]wire.CacheDefinition, error) {
	capacity, err := catalogCapacity(cfg, b)
	if err != nil {
		return nil, err
	}

	admitted := keyedCaches(b)
	existing := 0

	for _, cache := range catalog {
		if admitted[cache.ID] {
			existing++
		}
	}

	if existing > capacity {
		// An older controller or changed policy can have overcommitted durable
		// state. Never silently evict its keys or shorten retirement to make room.
		return nil, fmt.Errorf("admitted catalog exceeds rotation capacity %d: %w", capacity, wire.TooLarge)
	}

	slots := capacity - existing

	accepted := make([]wire.CacheDefinition, 0, min(len(catalog), capacity))
	for _, cache := range catalog {
		if !admitted[cache.ID] {
			if slots == 0 {
				ctrl.LoggerFrom(ctx).Info("cache catalog admission rejected", "cache", cache.Name, "uid", cache.ID, "reason", "rotation_capacity", "capacity", capacity)
				continue
			}

			slots--
		}

		accepted = append(accepted, cache)
	}

	return accepted, nil
}
