// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package mirror_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/inflight"
	"github.com/Azure/unbounded/internal/gantry/mirror"
)

// stubInflightDefaults returns an inflight.Map with the design doc defaults
// suitable for direct-origin-fallback tests (kind+expectedSize don't drive any test
// assertions here).
func nf5Inflight() *inflight.Map {
	return inflight.New(inflight.DefaultStalls(), nil)
}

func nf5Digest(t *testing.T, b byte) digest.Digest {
	t.Helper()
	// Caller passes a hex character (0-9 a-f). Build a 64-char hex
	// string by repeating it.
	if (b < '0' || b > '9') && (b < 'a' || b > 'f') {
		t.Fatalf("nf5Digest: byte %q is not hex", b)
	}

	hex := make([]byte, 64)
	for i := range hex {
		hex[i] = b
	}

	d, err := digest.Parse("sha256:" + string(hex))
	if err != nil {
		t.Fatalf("digest.Parse: %v", err)
	}

	return d
}

// TestNF5_DeclinesInBootstrapWindow asserts the bootstrap-window
// suppression: direct-origin-fallback must not fire while the local DHT is still
// converging.
func TestNF5_DeclinesInBootstrapWindow(t *testing.T) {
	var declineReason string

	ctrl := mirror.NewDirectOriginFallback(mirror.DirectOriginFallbackOptions{
		Inflight:      nf5Inflight(),
		InBootstrap:   func() bool { return true },
		HealthyEnough: func() bool { return true },
		ClusterSize:   func() int { return 10 },
		OnFallback:    func() { t.Fatalf("NF5 must not fire in bootstrap window") },
		OnDecline:     func(r string) { declineReason = r },
	})

	proceed, _, err := ctrl.Allow(context.Background(), nf5Digest(t, 'a'), ifaces.KindBlob, 0)
	if err != nil {
		t.Fatalf("Allow err = %v", err)
	}

	if proceed {
		t.Fatalf("Allow proceed = true; want false (bootstrap window)")
	}

	if declineReason != "bootstrap_window" {
		t.Fatalf("decline reason = %q; want bootstrap_window", declineReason)
	}
}

// TestNF5_DeclinesWhenUnhealthy asserts direct-origin-fallback declines when the DHT
// health gate reports unhealthy.
func TestNF5_DeclinesWhenUnhealthy(t *testing.T) {
	var declineReason string

	ctrl := mirror.NewDirectOriginFallback(mirror.DirectOriginFallbackOptions{
		Inflight:      nf5Inflight(),
		InBootstrap:   func() bool { return false },
		HealthyEnough: func() bool { return false },
		ClusterSize:   func() int { return 10 },
		OnDecline:     func(r string) { declineReason = r },
	})

	proceed, _, _ := ctrl.Allow(context.Background(), nf5Digest(t, 'b'), ifaces.KindManifest, 0)
	if proceed {
		t.Fatalf("Allow proceed = true; want false (unhealthy)")
	}

	if declineReason != "dht_unhealthy" {
		t.Fatalf("decline reason = %q; want dht_unhealthy", declineReason)
	}
}

// TestNF5_DeclinesOnInflightCollision asserts that a second concurrent
// caller for the same digest declines.
func TestNF5_DeclinesOnInflightCollision(t *testing.T) {
	infl := nf5Inflight()
	ctrl := mirror.NewDirectOriginFallback(mirror.DirectOriginFallbackOptions{
		Inflight:      infl,
		InBootstrap:   func() bool { return false },
		HealthyEnough: func() bool { return true },
		ClusterSize:   func() int { return 1 }, // no jitter
		OnFallback:    func() {},
	})

	d := nf5Digest(t, 'c')

	// First call grabs the in-flight slot.
	proceed1, release1, err := ctrl.Allow(context.Background(), d, ifaces.KindBlob, 0)
	if err != nil || !proceed1 {
		t.Fatalf("first Allow: proceed=%v err=%v; want true/nil", proceed1, err)
	}
	defer release1()

	// Second call sees the existing in-flight entry and declines.
	var declineReason string

	ctrl2 := mirror.NewDirectOriginFallback(mirror.DirectOriginFallbackOptions{
		Inflight:      infl,
		InBootstrap:   func() bool { return false },
		HealthyEnough: func() bool { return true },
		ClusterSize:   func() int { return 1 },
		OnDecline:     func(r string) { declineReason = r },
	})

	proceed2, _, _ := ctrl2.Allow(context.Background(), d, ifaces.KindBlob, 0)
	if proceed2 {
		t.Fatalf("second Allow: proceed=true; want false (in_flight)")
	}

	if declineReason != "in_flight" {
		t.Fatalf("decline reason = %q; want in_flight", declineReason)
	}
}

// TestNF5_TokenBucketExhausts asserts that more than `PerNodeRateLimit`
// rapid calls hit the bucket-empty branch.
func TestNF5_TokenBucketExhausts(t *testing.T) {
	infl := nf5Inflight()
	now := time.Now()
	clock := now

	var (
		fallbacks      atomic.Int32
		declines       int
		declinesMu     sync.Mutex
		declineReasons []string
	)

	ctrl := mirror.NewDirectOriginFallback(mirror.DirectOriginFallbackOptions{
		Now:              func() time.Time { return clock },
		Inflight:         infl,
		InBootstrap:      func() bool { return false },
		HealthyEnough:    func() bool { return true },
		ClusterSize:      func() int { return 1 }, // no jitter
		PerNodeRateLimit: 2,
		OnFallback:       func() { fallbacks.Add(1) },
		OnDecline: func(r string) {
			declinesMu.Lock()
			declines++

			declineReasons = append(declineReasons, r)
			declinesMu.Unlock()
		},
	})

	// Burn 2 tokens (distinct digests so dedup doesn't intervene).
	for i, b := range []byte{'d', 'e'} {
		_, release, _ := ctrl.Allow(context.Background(), nf5Digest(t, b), ifaces.KindBlob, 0)
		if release == nil {
			t.Fatalf("call #%d: expected release fn, got nil", i)
		}

		release()
	}
	// Third call: empty bucket -> decline.
	proceed, _, _ := ctrl.Allow(context.Background(), nf5Digest(t, 'f'), ifaces.KindBlob, 0)
	if proceed {
		t.Fatalf("3rd call: proceed=true; want false (rate_limited)")
	}

	declinesMu.Lock()
	gotRate := false

	for _, r := range declineReasons {
		if r == "rate_limited" {
			gotRate = true
		}
	}
	declinesMu.Unlock()

	if !gotRate {
		t.Fatalf("no rate_limited decline observed; got %v", declineReasons)
	}

	if fallbacks.Load() != 2 {
		t.Fatalf("OnFallback fires = %d; want 2", fallbacks.Load())
	}

	// Advance clock 30s -> 30s × 2/60 = 1 token replenished.
	clock = clock.Add(30 * time.Second)

	proceed, release, _ := ctrl.Allow(context.Background(), nf5Digest(t, '0'), ifaces.KindBlob, 0)
	if !proceed {
		t.Fatalf("after 30s refill: proceed=false; want true")
	}

	release()
}

// TestNF5_DeclinesAfterRecheckHit asserts that if Recheck reports
// a provider materialised during jitter, direct-origin-fallback cancels.
func TestNF5_DeclinesAfterRecheckHit(t *testing.T) {
	ctrl := mirror.NewDirectOriginFallback(mirror.DirectOriginFallbackOptions{
		Inflight:      nf5Inflight(),
		InBootstrap:   func() bool { return false },
		HealthyEnough: func() bool { return true },
		ClusterSize:   func() int { return 1 }, // no jitter - recheck still runs
		Recheck:       func(context.Context, digest.Digest) bool { return true },
		OnFallback:    func() { t.Fatalf("NF5 must not fire when recheck hits") },
	})

	var reason string

	ctrl2 := mirror.NewDirectOriginFallback(mirror.DirectOriginFallbackOptions{
		Inflight:      nf5Inflight(),
		InBootstrap:   func() bool { return false },
		HealthyEnough: func() bool { return true },
		ClusterSize:   func() int { return 1 },
		Recheck:       func(context.Context, digest.Digest) bool { return true },
		OnFallback:    func() { t.Fatalf("NF5 must not fire when recheck hits") },
		OnDecline:     func(r string) { reason = r },
	})
	_ = ctrl // first ctrl uses no decline hook //nolint:errcheck // best-effort

	proceed, _, _ := ctrl2.Allow(context.Background(), nf5Digest(t, '1'), ifaces.KindBlob, 0)
	if proceed {
		t.Fatalf("proceed=true; want false (recheck_hit)")
	}

	if reason != "recheck_hit" {
		t.Fatalf("decline reason = %q; want recheck_hit", reason)
	}
}

// TestNF5_ProceedsWhenGatesPass is the happy path: bootstrap done,
// healthy, no inflight collision, token available, recheck empty -
// direct-origin-fallback proceeds and OnFallback fires exactly once.
func TestNF5_ProceedsWhenGatesPass(t *testing.T) {
	var fallbacks int

	ctrl := mirror.NewDirectOriginFallback(mirror.DirectOriginFallbackOptions{
		Inflight:      nf5Inflight(),
		InBootstrap:   func() bool { return false },
		HealthyEnough: func() bool { return true },
		ClusterSize:   func() int { return 1 }, // no jitter
		Recheck:       func(context.Context, digest.Digest) bool { return false },
		OnFallback:    func() { fallbacks++ },
	})

	proceed, release, err := ctrl.Allow(context.Background(), nf5Digest(t, '2'), ifaces.KindBlob, 0)
	if err != nil {
		t.Fatalf("Allow err = %v", err)
	}

	if !proceed {
		t.Fatalf("Allow proceed = false; want true")
	}

	if release == nil {
		t.Fatalf("Allow release = nil; want non-nil")
	}

	release()

	if fallbacks != 1 {
		t.Fatalf("OnFallback fires = %d; want 1", fallbacks)
	}
}

// TestNF5_ContextCancelledDuringJitter asserts that a context cancel
// during the jitter sleep aborts the call and returns the context
// error.
func TestNF5_ContextCancelledDuringJitter(t *testing.T) {
	ctrl := mirror.NewDirectOriginFallback(mirror.DirectOriginFallbackOptions{
		Inflight:      nf5Inflight(),
		InBootstrap:   func() bool { return false },
		HealthyEnough: func() bool { return true },
		ClusterSize:   func() int { return 1000 }, // ~ln(1000)≈6.9 -> ~20s jitter
		JitterBase:    3 * time.Second,
		OnFallback:    func() { t.Fatalf("must not fire on ctx cancel") },
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	proceed, _, err := ctrl.Allow(ctx, nf5Digest(t, '3'), ifaces.KindBlob, 0)
	if proceed {
		t.Fatalf("proceed=true; want false (ctx cancelled)")
	}

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; want context.Canceled", err)
	}
}
