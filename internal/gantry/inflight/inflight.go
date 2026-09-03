// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package inflight tracks per-digest pulls currently being executed on
// this agent. The map is shared between two callers:
//
// - The puller-side `please_pull` handler (the step 7) which calls
// Start to atomically claim a digest. The "already started"
// bool return is what produces the wire-level
// OUTCOME_ALREADY_PULLING vs OUTCOME_STARTED distinction.
//
// - The responder-side `pull_intent_query` handler (the step 4)
// which calls LookupForIntent to report in_flight / started_at to
// the requester.
//
// The requester-side the design doc stall check (IsStale) is also implemented
// here, so the stall threshold lives in exactly one place.
//
// All operations are O(1) under a single sync.Mutex. The map is in-
// memory only - restarts clear it. Restart-during-pull is treated as a
// stalled pull from the requester's point of view (the design doc), which is the
// correct behavior: the puller is gone, rank-1 takes over.
package inflight

import (
	"sync"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

// Entry records the in-flight state for a single digest. ExpectedClass
// is set only after the puller learns whether this is a manifest /
// config (kB-scale, fixed timeout) vs a layer (size-aware timeout);
// ExpectedSize is set when known (manifest gives it).
type Entry struct {
	StartedAt     time.Time
	ExpectedClass ifaces.OriginRefKind
	ExpectedSize  int64
}

// Handle is returned by Start. Calling Done on Handle removes the
// digest from the in-flight map. Handles are single-use; subsequent
// calls are no-ops.
type Handle struct {
	mu       sync.Mutex
	released bool
	m        *Map
	digest   digest.Digest
}

// Done releases the in-flight slot. Safe to call multiple times. Safe
// to call from a defer.
func (h *Handle) Done() {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.released {
		return
	}

	h.released = true
	h.m.release(h.digest)
}

// Map is the per-agent in-flight registry. The zero value is not
// usable; construct via New.
type Map struct {
	now    func() time.Time
	mu     sync.Mutex
	byKey  map[string]Entry
	stalls Stalls
}

// Stalls bundles the per-digest-kind timeouts used by IsStale. See
// the design doc for the per-kind values. v1 defaults - manifest/config 5s,
// layer = max(10s, expected_size/50MB/s) × 3 - are produced by
// DefaultStalls and ResolveStall.
type Stalls struct {
	ManifestConfig   time.Duration
	LayerFloor       time.Duration // floor portion of expected_pull_seconds
	LayerBytesPerSec int64         // 50 * 1024 * 1024 in v1
	LayerMultiplier  int           // 3 in v1
}

// DefaultStalls returns the defaults.
func DefaultStalls() Stalls {
	return Stalls{
		ManifestConfig:   5 * time.Second,
		LayerFloor:       10 * time.Second,
		LayerBytesPerSec: 50 * 1024 * 1024,
		LayerMultiplier:  3,
	}
}

// ResolveStall computes the per-digest stall threshold from kind and
// the known/expected size. Per the design doc:
//
// - manifest/config -> ManifestConfig (size irrelevant; kB-scale)
// - layer -> max(LayerFloor, size / bytesPerSec) × multiplier
//
// expectedSize <= 0 (unknown) falls back to LayerFloor × multiplier
// for layers - the safest assumption when the manifest hasn't been
// parsed yet on this side.
func (s Stalls) ResolveStall(kind ifaces.OriginRefKind, expectedSize int64) time.Duration {
	if kind == ifaces.KindManifest {
		return s.ManifestConfig
	}
	// Layer (or unset). v1 currently routes config blobs through KindBlob
	// because we don't track config-vs-layer separately at the cache
	// layer; cold-start's caller passes KindManifest explicitly when it
	// knows the request is a manifest, so KindBlob covers both "config"
	// and "layer" in this code path. The manifest/config 5s timeout
	// applies when callers explicitly identify a config digest (	// can refine this with a dedicated kind if measurement says so).
	floor := s.LayerFloor
	if s.LayerBytesPerSec > 0 && expectedSize > 0 {
		bps := time.Duration(expectedSize/s.LayerBytesPerSec) * time.Second
		if bps > floor {
			floor = bps
		}
	}

	mul := s.LayerMultiplier
	if mul <= 0 {
		mul = 1
	}

	return floor * time.Duration(mul)
}

// New constructs an empty in-flight Map with the supplied stall config
// and clock. now == nil falls back to time.Now.
func New(stalls Stalls, now func() time.Time) *Map {
	if now == nil {
		now = time.Now
	}

	return &Map{now: now, byKey: map[string]Entry{}, stalls: stalls}
}

// Start atomically claims the digest. If alreadyPulling is true the
// returned handle is a no-op handle for that digest (callers MUST
// still receive a non-nil Handle so deferred Done works
// unconditionally). The Entry return is the existing entry when
// alreadyPulling is true; otherwise it's the entry just inserted.
//
// Used by the puller-side `please_pull` handler to atomically decide
// between OUTCOME_STARTED and OUTCOME_ALREADY_PULLING.
func (m *Map) Start(d digest.Digest, kind ifaces.OriginRefKind, expectedSize int64) (*Handle, Entry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if e, ok := m.byKey[d.String()]; ok {
		// already pulling - return a noop handle so caller's defer is
		// safe even though we don't want to release.
		return &Handle{released: true, m: m, digest: d}, e, true
	}

	e := Entry{
		StartedAt:     m.now(),
		ExpectedClass: kind,
		ExpectedSize:  expectedSize,
	}
	m.byKey[d.String()] = e

	return &Handle{m: m, digest: d}, e, false
}

// LookupForIntent returns the in-flight state for d, used to fill
// PullIntentResponse.in_flight / .started_at on the responder side.
// in_flight is false (and the entry zero) when d is not currently
// being pulled by this agent.
func (m *Map) LookupForIntent(d digest.Digest) (Entry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.byKey[d.String()]

	return e, ok
}

// IsStale reports whether an in-flight Entry observed at startedAt for
// a digest of the given kind has exceeded the per-the design doc timeout. The
// expectedSize parameter is the size the requester believes the digest
// to be (0 means unknown). Used by the requester-side the design doc stall
// check - the requester is comparing a PullIntentResponse from a
// remote node to its local clock, so the time arrives via the
// response, not via a Map lookup.
func (m *Map) IsStale(kind ifaces.OriginRefKind, expectedSize int64, startedAt time.Time) bool {
	if startedAt.IsZero() {
		return false
	}

	return m.now().Sub(startedAt) > m.stalls.ResolveStall(kind, expectedSize)
}

// Len returns the current number of in-flight entries. Used by the
// `p2p_in_flight_pulls` gauge.
func (m *Map) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return len(m.byKey)
}

// Stalls returns the configured stall thresholds (read-only).
func (m *Map) Stalls() Stalls { return m.stalls }

// release removes the digest from the map. Called by Handle.Done.
func (m *Map) release(d digest.Digest) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.byKey, d.String())
}
