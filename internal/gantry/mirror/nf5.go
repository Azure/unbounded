// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// direct-origin-fallback direct-origin fallback (last resort).
//
// Design :
// when the cold-start cascade is exhausted (no cache, no in-flight,
// no provider returned by HRW probe + DHT, top-K expansion has
// already been tried) and the local DHT is healthy enough to trust
// that empty answer, the mirror is permitted to do a direct origin
// pull rather than returning 5xx. direct-origin-fallback is a controlled escape valve
// that costs at most ~N origin pulls cluster-wide under genuine
// cold-cluster bootstrap; it is NOT a substitute for the warm path.
//
// Gates (must all hold for direct-origin-fallback to fire):
//
// 1. **Bootstrap window passed.** During the first
// `bootstrap_window` (default 30s) or while the routing table is
// under `bootstrap_routing_table_pct` (default 25%), an empty
// DHT result is a false negative and direct-origin-fallback must not fire.
// 2. **DHT not Unhealthy.** Under DHT health < 0.3 the empty DHT
// answer is unreliable; we'd rather 5xx and let kubelet retry
// than thunder the origin.
// 3. **≤1 direct-origin-fallback in-flight per digest.** Re-uses
// `inflight.Map`; concurrent callers for the same digest see
// `alreadyPulling=true` and decline.
// 4. **Per-node token bucket.** Replenishes at
// `nf5_per_node_rate_limit` tokens/minute (default 2). Empty
// bucket -> decline (5xx).
// 5. **Jitter `[0, nf5_jitter_base × ln(N))`.** Randomizes direct-origin-fallback
// timing across the cluster so the first node to complete an
// origin pull can publish its provider record before others
// fire their own fallback.
// 6. **Re-check after jitter.** If the warm path materialized
// during the jitter window (peer published a provider record),
// cancel and let the caller retry via the warm path.
//
// On success: emits `p2p_origin_fallback_total`. The caller is
// expected to release the inflight handle when the origin pull
// completes (success or failure).

package mirror

import (
	"context"
	"errors"
	"log/slog"
	"math"
	mathrand "math/rand"
	"sync"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/inflight"
)

// ErrColdStartExhausted is the boundary-stable sentinel that
// `ColdStartResolver.Resolve` returns when the cascade ran to
// its final fallthrough (rule-7 cold-start, top-2K expansion both
// failed). direct-origin-fallback fallback may attempt a direct origin pull only when
// this is the underlying error. Other cold-start errors (failure
// short-circuit, cooldown active) intentionally map to other errors
// so direct-origin-fallback cannot circumvent them.
var ErrColdStartExhausted = errors.New("mirror: cold-start cascade exhausted")

// DirectOriginFallbackOptions configures the last-resort fallback controller.
type DirectOriginFallbackOptions struct {
	// Logger is the structured logger. Required.
	Logger *slog.Logger

	// Now is the time source; defaults to time.Now.
	Now func() time.Time

	// JitterBase is the `nf5_jitter_base` (default 3s when ≤0).
	// The jitter window is `[0, JitterBase × ln(ClusterSize))`.
	JitterBase time.Duration

	// JitterCap is a hard ceiling on the computed jitter window
	// (`nf5_jitter_cap`). Zero means no cap. When set, the effective
	// window is min(JitterBase*ln(N), JitterCap).
	JitterCap time.Duration

	// PerNodeRateLimit is the `nf5_per_node_rate_limit`
	// (default 2 tokens/minute when ≤0). Refill is continuous
	// (fractional tokens accrue at PerNodeRateLimit/60 per second).
	PerNodeRateLimit int

	// ClusterSize returns the current membership count; used as
	// `N` in the `ln(N)` jitter scaling. Nil or returning ≤1
	// disables the jitter component.
	ClusterSize func() int

	// InBootstrap reports whether the local DHT is still in the
	// bootstrap-suppression window. direct-origin-fallback is forbidden while this
	// returns true.
	InBootstrap func() bool

	// HealthyEnough reports whether the local DHT health is in a
	// state where the empty-DHT answer can be trusted (i.e. not
	// Unhealthy). When this returns false direct-origin-fallback declines.
	HealthyEnough func() bool

	// Inflight is the per-digest dedup map; direct-origin-fallback takes a handle so
	// concurrent direct-origin-fallback calls for the same digest collapse to one.
	Inflight *inflight.Map

	// Recheck performs a final DHT + cache + peer probe at the end
	// of the jitter window. Returns true if a provider materialized
	// during jitter (direct-origin-fallback cancels and the caller retries the warm
	// path).
	Recheck func(context.Context, digest.Digest) bool

	// OnFallback is invoked once per origin pull that direct-origin-fallback permits.
	// Maps to the design doc metric `p2p_origin_fallback_total`.
	OnFallback func()

	// OnDecline reports the reason direct-origin-fallback declined a request. Useful
	// for ops dashboards. reason ∈ {"bootstrap_window",
	// "dht_unhealthy", "in_flight", "rate_limited", "recheck_hit",
	// "context_canceled"}. Optional.
	OnDecline func(reason string)
}

// DirectOriginFallbackController runs the gating sequence. Safe for concurrent
// use.
type DirectOriginFallbackController struct {
	opts DirectOriginFallbackOptions

	rngMu sync.Mutex
	rng   *mathrand.Rand

	bucketMu   sync.Mutex
	tokens     float64
	lastRefill time.Time
}

// NewDirectOriginFallback builds a controller. Inflight must be non-nil; everything
// else receives reasonable defaults.
func NewDirectOriginFallback(opts DirectOriginFallbackOptions) *DirectOriginFallbackController {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	opts.Logger = opts.Logger.With(slog.String("subsystem", "nf5"))

	if opts.Inflight == nil {
		panic("NewNF5: Inflight is required")
	}

	if opts.Now == nil {
		opts.Now = time.Now
	}

	if opts.JitterBase <= 0 {
		opts.JitterBase = 3 * time.Second
	}

	if opts.PerNodeRateLimit <= 0 {
		opts.PerNodeRateLimit = 2
	}

	now := opts.Now()

	return &DirectOriginFallbackController{
		opts:       opts,
		rng:        mathrand.New(mathrand.NewSource(now.UnixNano())),
		tokens:     float64(opts.PerNodeRateLimit), // start with full bucket
		lastRefill: now,
	}
}

// Allow runs the direct-origin-fallback gating sequence for digest d. When it returns
// (true, release, nil), the caller MUST invoke `release` once the
// origin pull completes (success or failure) - this frees the
// in-flight slot. The token has already been consumed; releasing
// does not refund it.
//
// When it returns (false, nil, nil), direct-origin-fallback has declined. The caller
// should respond 5xx (warm path exhausted).
//
// On context cancellation (e.g. client disconnect during jitter),
// returns (false, nil, ctx.Err) and any in-flight handle is
// released.
//
// kind and expectedSize are forwarded to inflight.Map.Start so the
// in-flight entry carries enough context for the design doc stall detection
// in case the direct-origin-fallback origin pull itself stalls.
func (n *DirectOriginFallbackController) Allow(ctx context.Context, d digest.Digest, kind ifaces.OriginRefKind, expectedSize int64) (bool, func(), error) {
	if n.opts.InBootstrap != nil && n.opts.InBootstrap() {
		n.decline("bootstrap_window")
		return false, nil, nil
	}

	if n.opts.HealthyEnough != nil && !n.opts.HealthyEnough() {
		n.decline("dht_unhealthy")
		return false, nil, nil
	}

	// ≤1 direct-origin-fallback in-flight per digest. We rely on `inflight.Map.Start`
	// for atomicity: if it reports alreadyPulling, direct-origin-fallback declines so
	// the caller 5xxs and lets the existing pull complete and
	// publish.
	handle, _, alreadyPulling := n.opts.Inflight.Start(d, kind, expectedSize)
	if alreadyPulling {
		n.decline("in_flight")
		return false, nil, nil
	}

	release := func() { handle.Done() }

	// Per-node token bucket. Take a token before sleeping for
	// jitter so we don't sleep just to discover the bucket is
	// empty.
	if !n.takeToken() {
		release()
		n.decline("rate_limited")

		return false, nil, nil
	}

	// Jitter `[0, JitterBase × ln(N))`. ClusterSize ≤ 1 ⇒ no
	// jitter (single-node cluster has nothing to coordinate with).
	if j := n.computeJitter(); j > 0 {
		t := time.NewTimer(j)
		select {
		case <-ctx.Done():
			t.Stop()
			release()
			n.decline("context_canceled")

			return false, nil, ctx.Err()
		case <-t.C:
		}
	}

	// Final re-check: the warm path may have materialized during
	// jitter. Canceling here keeps `p2p_origin_fallback_total`
	// near zero even under chaos scenarios.
	if n.opts.Recheck != nil && n.opts.Recheck(ctx, d) {
		release()
		n.decline("recheck_hit")

		return false, nil, nil
	}

	if n.opts.OnFallback != nil {
		n.opts.OnFallback()
	}

	return true, release, nil
}

// takeToken refills the bucket continuously at `PerNodeRateLimit`
// tokens/minute and consumes 1 token if available. Returns true on
// success.
func (n *DirectOriginFallbackController) takeToken() bool {
	n.bucketMu.Lock()
	defer n.bucketMu.Unlock()

	now := n.opts.Now()

	elapsed := now.Sub(n.lastRefill)
	if elapsed > 0 {
		n.tokens += elapsed.Seconds() * float64(n.opts.PerNodeRateLimit) / 60.0

		maxTokens := float64(n.opts.PerNodeRateLimit)
		if n.tokens > maxTokens {
			n.tokens = maxTokens
		}

		n.lastRefill = now
	}

	if n.tokens < 1.0 {
		return false
	}

	n.tokens -= 1.0

	return true
}

// computeJitter returns a uniform random duration in
// `[0, JitterBase × ln(N))`. Returns 0 when N ≤ 1.
func (n *DirectOriginFallbackController) computeJitter() time.Duration {
	N := 1
	if n.opts.ClusterSize != nil {
		N = n.opts.ClusterSize()
	}

	if N < 2 {
		return 0
	}

	maxJ := time.Duration(float64(n.opts.JitterBase) * math.Log(float64(N)))
	if maxJ <= 0 {
		return 0
	}

	if n.opts.JitterCap > 0 && maxJ > n.opts.JitterCap {
		maxJ = n.opts.JitterCap
	}

	n.rngMu.Lock()
	j := time.Duration(n.rng.Int63n(int64(maxJ)))
	n.rngMu.Unlock()

	return j
}

// decline calls the optional OnDecline hook with the supplied reason.
func (n *DirectOriginFallbackController) decline(reason string) {
	if n.opts.OnDecline != nil {
		n.opts.OnDecline(reason)
	}
}
