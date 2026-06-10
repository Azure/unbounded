// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package negcache implements the per-puller, in-memory negative cache
// described in the design doc of the Gantry design.
//
// When a puller's origin fetch fails terminally, the failure is
// classified (FailureAuth, FailureNotFound, FailureRateLimited,
// FailureTransient) and recorded against the digest. Subsequent
// pull_intent_query / please_pull RPCs for that digest must surface
// the cooldown state so requesters can short-circuit (the step:
// "Signal propagation via the existing probe RPCs").
//
// Cooldown ladder (configurable, defaults from the design doc):
//
//	1st failure -> 10 s (Initial)
//	2nd failure -> 30 s (Initial × Multiplier)
//	3rd failure -> 90 s (Initial × Multiplier^2 capped at Max)
//	4th+ -> 10 min (Max)
//
// The first successful pull clears the entry (the design doc "Self-healing").
//
// The cache is local-only. The design (the design doc "Why the negative cache is
// local-only, not propagated via DHT") explicitly forbids cluster-wide
// propagation because a stale "this digest failed" marker outliving an
// actual recovery would be a serious correctness bug.
package negcache

import (
	"sync"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

// Options configures the cooldown ladder. Zero values pick the
// defaults; callers should still pass an explicit value to make tests
// deterministic.
type Options struct {
	// Initial is the cooldown applied on the first failure.
	Initial time.Duration
	// Max caps the cooldown after exponential growth.
	Max time.Duration
	// Multiplier is the geometric factor between successive cooldowns.
	// Spec default is 3× (10 s -> 30 s -> 90 s -> ... -> Max).
	Multiplier int
	// Now is the clock used for cooldown comparisons. Tests override to
	// inject a deterministic time source. Defaults to time.Now.
	Now func() time.Time
	// OnEnter and OnHit are optional metric callbacks. nil-safe.
	OnEnter func(class ifaces.FailureClass)
	OnHit   func(class ifaces.FailureClass)
	// OnSize is called after every mutation with the new entry count
	// so a GaugeFunc reader can expose `p2p_negative_cache_entries`
	// without locking the map. nil-safe.
	OnSize func(count int)
}

// Entry mirrors the design doc's recent_failures[digest] record.
type Entry struct {
	LastFailure   time.Time
	FailureCount  int
	Class         ifaces.FailureClass
	CooldownUntil time.Time
}

// Cache is the per-puller the design doc negative cache. Safe for concurrent use.
type Cache struct {
	opts Options
	mu   sync.Mutex
	m    map[digest.Digest]Entry
}

// New returns an empty Cache. Required options that are zero get the
// the design doc defaults (Initial=10s, Max=10min, Multiplier=3, Now=time.Now).
func New(opts Options) *Cache {
	if opts.Initial <= 0 {
		opts.Initial = 10 * time.Second
	}

	if opts.Max <= 0 {
		opts.Max = 10 * time.Minute
	}

	if opts.Multiplier <= 1 {
		opts.Multiplier = 3
	}

	if opts.Now == nil {
		opts.Now = time.Now
	}

	return &Cache{
		opts: opts,
		m:    make(map[digest.Digest]Entry),
	}
}

// RecordFailure registers a fresh terminal failure for d. The cooldown
// grows geometrically with successive failures, capped at Options.Max.
// Idempotent for the same call site; multiple calls within a single
// cooldown window keep extending it (the puller is expected to call
// this exactly once per terminal failure).
func (c *Cache) RecordFailure(d digest.Digest, class ifaces.FailureClass) Entry {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.opts.Now()
	prev := c.m[d]
	prev.FailureCount++
	prev.LastFailure = now
	prev.Class = class
	prev.CooldownUntil = now.Add(c.cooldownFor(prev.FailureCount))
	c.m[d] = prev

	if c.opts.OnEnter != nil {
		c.opts.OnEnter(class)
	}

	if c.opts.OnSize != nil {
		c.opts.OnSize(len(c.m))
	}

	return prev
}

// RecordSuccess clears any negative-cache entry for d. The the design doc spec
// says "the first successful pull of the digest clears the entry".
// Idempotent: a no-op when no entry exists.
func (c *Cache) RecordSuccess(d digest.Digest) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.m[d]; !ok {
		return
	}

	delete(c.m, d)

	if c.opts.OnSize != nil {
		c.opts.OnSize(len(c.m))
	}
}

// Lookup implements coord.NegativeCache. Returns (Entry, true) if a
// non-expired entry exists. Expired entries are evicted on access.
func (c *Cache) Lookup(d digest.Digest) (Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.m[d]
	if !ok {
		return Entry{}, false
	}

	if c.opts.Now().After(e.CooldownUntil) {
		// Cooldown elapsed; drop the entry so the next attempt is
		// single-shot per the design doc "Self-healing".
		delete(c.m, d)

		if c.opts.OnSize != nil {
			c.opts.OnSize(len(c.m))
		}

		return Entry{}, false
	}

	if c.opts.OnHit != nil {
		c.opts.OnHit(e.Class)
	}

	return e, true
}

// Len returns the current entry count. Used by a GaugeFunc for
// `p2p_negative_cache_entries`. Cheap (single mutex acquisition).
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.m)
}

// cooldownFor returns the cooldown duration for the n-th consecutive
// failure (n=1 -> Initial, growing geometrically by Multiplier, capped
// at Max).
func (c *Cache) cooldownFor(failureCount int) time.Duration {
	d := c.opts.Initial
	for i := 1; i < failureCount; i++ {
		next := d * time.Duration(c.opts.Multiplier)
		if next > c.opts.Max || next < d {
			return c.opts.Max
		}

		d = next
	}

	if d > c.opts.Max {
		return c.opts.Max
	}

	return d
}
