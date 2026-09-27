// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math"
	"time"

	"github.com/Azure/unbounded/internal/racer/wire"
)

type RotationPolicy struct {
	Interval   time.Duration
	PrepareFor time.Duration
	RetainFor  time.Duration
}

// RotationState lives beside bundle.json in the shared Secret. It is sufficient
// to resume transitions after restart; no rotation-job or acknowledgment objects.
type RotationState struct {
	NextTransition time.Time            `json:"next_transition"`
	NextRotation   time.Time            `json:"next_rotation"`
	ActivateAt     time.Time            `json:"activate_at"`
	ActiveIssuer   string               `json:"active_issuer"`
	PreparedIssuer string               `json:"prepared_issuer"`
	Retiring       map[string]time.Time `json:"retiring"`
}

func rootID(der []byte) string { sum := sha256.Sum256(der); return hex.EncodeToString(sum[:]) }
func keyID(k wire.CacheKey) string {
	return string(k.Key.Cache) + "/" + string(k.Key.Purpose) + "/" + hex.EncodeToString(k.Key.ID)
}

func keyScope(k wire.CacheKey) string { return string(k.Key.Cache) + "/" + string(k.Key.Purpose) }

func (s RotationState) nextTransition() time.Time {
	deadline := s.NextRotation
	if s.PreparedIssuer != "" {
		deadline = s.ActivateAt
	}

	for _, at := range s.Retiring {
		if at.Before(deadline) {
			deadline = at
		}
	}

	return deadline
}

func newCacheKey(cache wire.CacheID, purpose wire.KeyPurpose, state wire.KeyState, generation wire.Generation) (wire.CacheKey, error) {
	if generation == 0 {
		return wire.CacheKey{}, wire.Unavailable
	}

	var material [32]byte
	if _, err := rand.Read(material[:]); err != nil {
		return wire.CacheKey{}, err
	}

	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return wire.CacheKey{}, err
	}
	// Reserve a versioned namespace in the otherwise opaque wire ID. A node can
	// reject reintroduced epochs using its bundle high-water mark, without keeping
	// every retired ID. The suffix distinguishes keys minted in competing CAS attempts.
	copy(id, "RKG1")
	binary.BigEndian.PutUint64(id[4:12], uint64(generation))

	return wire.NewCacheKey(wire.CacheKeyRef{Cache: cache, Purpose: purpose, ID: id}, state, material)
}

// PlanRotation owns its output and plans transitions using the supplied policy.
// Deadlines are measured from actual transitions, never advanced through
// missed intervals after downtime. Issuer staging is done by reconcileKeys before
// this planner; publication persists private material first.
func PlanRotation(policy RotationPolicy, b wire.KeyringBundle, s RotationState, catalog []wire.CacheDefinition, now time.Time) (wire.KeyringBundle, RotationState, error) {
	var creationGeneration wire.Generation
	if b.Generation < math.MaxUint64 {
		creationGeneration = b.Generation + 1
	}

	return planRotation(policy, b, s, catalog, now, creationGeneration)
}

// creationGeneration is the publication that will first contain new keys. Zero
// forbids key creation when generations are exhausted, while allowing idle plans.
func planRotation(policy RotationPolicy, b wire.KeyringBundle, s RotationState, catalog []wire.CacheDefinition, now time.Time, creationGeneration wire.Generation) (wire.KeyringBundle, RotationState, error) {
	// Keep wire validation and the encoded size bound at the input boundary.
	// Ownership does not require decoding the just-validated representation.
	if _, err := wire.EncodeBundle(b); err != nil {
		return wire.KeyringBundle{}, RotationState{}, err
	}

	rootsCopy := make([][]byte, len(b.PeerTrustRoots))
	for i, root := range b.PeerTrustRoots {
		rootsCopy[i] = bytes.Clone(root)
	}

	b.PeerTrustRoots = rootsCopy
	// Like DecodeBundle, normalize an empty key collection to a non-nil slice.
	keysCopy := make([]wire.CacheKey, len(b.CacheKeys))
	for i, key := range b.CacheKeys {
		keysCopy[i] = key // Includes the value-owned [32]byte material.
		keysCopy[i].Key.ID = bytes.Clone(key.Key.ID)
	}

	b.CacheKeys = keysCopy

	retiring := make(map[string]time.Time, len(s.Retiring))
	for id, deadline := range s.Retiring {
		retiring[id] = deadline
	}

	s.Retiring = retiring

	wanted := map[wire.CacheID]bool{}
	for _, cache := range catalog {
		if !wire.ValidUUID(string(cache.ID)) || wanted[cache.ID] {
			return b, s, wire.InvalidRequest
		}

		wanted[cache.ID] = true
	}

	keys := b.CacheKeys[:0]
	for _, k := range b.CacheKeys {
		deadline, retiring := s.Retiring[keyID(k)]
		if !wanted[k.Key.Cache] || retiring && !now.Before(deadline) {
			delete(s.Retiring, keyID(k))
			continue
		}

		keys = append(keys, k)
	}

	b.CacheKeys = keys

	roots := b.PeerTrustRoots[:0]
	for _, root := range b.PeerTrustRoots {
		id := rootID(root)
		if deadline, ok := s.Retiring[id]; ok && !now.Before(deadline) {
			delete(s.Retiring, id)
			continue
		}

		roots = append(roots, root)
	}

	b.PeerTrustRoots = roots

	present := map[string]bool{}
	for _, key := range b.CacheKeys {
		present[keyScope(key)] = true
	}

	for _, cache := range catalog {
		for _, purpose := range []wire.KeyPurpose{wire.PageKey, wire.OriginCredentialsKey} {
			if !present[string(cache.ID)+"/"+string(purpose)] {
				k, err := newCacheKey(cache.ID, purpose, wire.ActiveKey, creationGeneration)
				if err != nil {
					return b, s, err
				}

				b.CacheKeys = append(b.CacheKeys, k)
			}
		}
	}

	if !s.ActivateAt.IsZero() && !now.Before(s.ActivateAt) {
		prepared := map[string]bool{}

		for _, key := range b.CacheKeys {
			if key.State == wire.PreparedKey {
				prepared[keyScope(key)] = true
			}
		}

		for i := range b.CacheKeys {
			k := &b.CacheKeys[i]
			// A cache added during preparation can have only its initial active key.
			if k.State == wire.ActiveKey && prepared[keyScope(*k)] {
				k.State = wire.RetiringKey
				s.Retiring[keyID(*k)] = now.Add(policy.RetainFor)
			}
		}

		for i := range b.CacheKeys {
			if b.CacheKeys[i].State == wire.PreparedKey {
				b.CacheKeys[i].State = wire.ActiveKey
			}
		}

		s.Retiring[s.ActiveIssuer] = now.Add(policy.RetainFor)
		s.ActiveIssuer, s.PreparedIssuer = s.PreparedIssuer, ""
		s.ActivateAt = time.Time{}
		s.NextRotation = now.Add(policy.Interval)
	} else if s.ActivateAt.IsZero() && !now.Before(s.NextRotation) {
		if s.PreparedIssuer == "" {
			return b, s, wire.Unavailable
		}

		var prepared []wire.CacheKey

		for _, k := range b.CacheKeys {
			if k.State != wire.ActiveKey {
				continue
			}

			next, err := newCacheKey(k.Key.Cache, k.Key.Purpose, wire.PreparedKey, creationGeneration)
			if err != nil {
				return b, s, err
			}

			prepared = append(prepared, next)
		}

		b.CacheKeys = append(b.CacheKeys, prepared...)
		s.ActivateAt = now.Add(policy.PrepareFor)
	}

	s.NextTransition = s.nextTransition()

	if _, err := wire.EncodeBundle(b); err != nil {
		return b, s, err
	}

	return b, s, nil
}

func containsRoot(b wire.KeyringBundle, id string) bool {
	for _, root := range b.PeerTrustRoots {
		if rootID(root) == id {
			return true
		}
	}

	return false
}
