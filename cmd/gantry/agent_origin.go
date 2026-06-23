// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"fmt"
	"log/slog"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/origin"
	"github.com/Azure/unbounded/internal/gantry/unstore"
)

// buildOriginClients constructs the two origin.Client instances the
// agent needs:
//
// - puller: used by runOriginPull / please_pull / direct-origin-fallback background
// ingest. Wired with the legacy p2p_origin_pull_* metric hooks
// because that path writes into containerd itself and can honestly
// define success after commit.
// - mirror: used by the mirror's live stream-through path. Has NO
// pull-arithmetic hooks because final commit belongs to the
// requesting containerd; live mirror traffic uses
// gantry_origin_stream_* + gantry_containerd_commit_{observed,
// missing_after_stream}_total instead.
//
// When CacheOrigins are configured, both puller and mirror are wrapped in a
// PriorityChain that consults each cache entry before falling back to the OCI
// registry client.
//
// The returned bgSuccess and bgDownstreamFailure closures are wired
// into the puller pump (see newPullerPump in main.go) so the puller
// can fire p2p_origin_pull_success_total only after its own
// containerd commit passes, and so downstream copy/commit failures
// land in p2p_origin_pull_failure_total{class=transient} without
// double-counting them as origin-side failures in
// p2p_origin_failure_total.
//
// Why two clients: a prior review caught that wiring success
// metrics into the origin Client's Close hook over-counted whenever
// the caller never drained the body (HEAD, io.Copy interruption,
// commit-time digest mismatch). Splitting the clients keeps the
// pull-arithmetic invariants honest while leaving the mirror's
// live-stream path free to use the asymmetric "started / completed /
// failed / commit-observed" counters that match its actual lifecycle.
func buildOriginClients(
	c *config.Config,
	inst *phase1Metrics,
	logger *slog.Logger,
) (puller, mirror ifaces.OriginPuller, bgSuccess func(kind string, bytes int64), bgDownstreamFailure func(kind, class string), err error) {
	bgSuccess = func(kind string, _ int64) {
		inst.originPullSuccess.WithLabelValues(kind).Inc()
	}
	// Twelfth-review fix: terminal counter for DOWNSTREAM failures
	// (io.Copy stall after origin returned 2xx / cw.Commit /
	// directVerifier mismatch / cache writer open). These are
	// pull-arithmetic terminals - without them
	// p2p_origin_pull_total{kind} drifted above
	// p2p_origin_pull_success_total + p2p_origin_pull_failure_total
	// on every downstream failure path. We route them to
	// p2p_origin_pull_failure_total{kind,class=transient} but
	// deliberately do NOT bump p2p_origin_failure_total: that
	// counter is reserved for true origin-side failures (origin
	// returned a non-2xx response) so the operator-facing "is
	// origin sick?" alert doesn't false-positive on local cache
	// I/O errors or origin truncations that look upstream from
	// here.
	bgDownstreamFailure = func(kind, class string) {
		inst.originPullFailure.WithLabelValues(kind, class).Inc()
	}

	ociPuller, err := origin.New(
		c,
		origin.WithLogger(logger),
		origin.WithMetrics(
			func(kind string) { inst.originPullTotal.WithLabelValues(kind).Inc() },
			// Failure: bump BOTH the per-(kind,class) counter
			// p2p_origin_pull_failure_total AND the per-class
			// p2p_origin_failure_total. This closure fires only
			// on TRUE origin-side failures (origin.recordFailure
			// inside origin.Client). Downstream failures (io.Copy
			// / Commit after origin returned 2xx) go through a
			// separate closure - see bgDownstreamFailure above -
			// that bumps ONLY the per-(kind,class) detail counter
			// and leaves p2p_origin_failure_total undisturbed so
			// the "is origin sick?" alert stays accurate.
			//
			// Previously the per-class counter was only fed by
			// the mirror direct path; this left the please_pull-
			// coordinated path (the bulk of pulls on a hot
			// cluster) silently uncounted. Wiring it here at the
			// single origin chokepoint means both paths feed
			// both counters from the same closure - there is
			// one source of truth for both metrics, and the
			// two are guaranteed to agree on what counts as a
			// "terminal origin-SIDE failure".
			func(kind, class string) {
				inst.originPullFailure.WithLabelValues(kind, class).Inc()
				inst.originFailureTotal.WithLabelValues(class).Inc()
			},
		),
	)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("origin: %w", err)
	}

	ociMirror, err := origin.New(c, origin.WithLogger(logger))
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("mirror origin: %w", err)
	}

	cacheEntries := buildCacheOriginEntries(c, inst, logger)

	puller = origin.NewPriorityChain(cacheEntries, ociPuller, logger)
	mirror = origin.NewPriorityChain(cacheEntries, ociMirror, logger)

	return puller, mirror, bgSuccess, bgDownstreamFailure, nil
}

// buildCacheOriginEntries builds the ordered list of cache OriginPullers from
// the CacheOrigins config. Returns nil (empty chain) if none are configured.
func buildCacheOriginEntries(c *config.Config, inst *phase1Metrics, logger *slog.Logger) []ifaces.OriginPuller {
	if len(c.CacheOrigins) == 0 {
		return nil
	}

	entries := make([]ifaces.OriginPuller, 0, len(c.CacheOrigins))

	for _, spec := range c.CacheOrigins {
		client := unstore.New(
			spec.Endpoint,
			0, // use default 30s timeout
			unstore.WithLogger(logger),
			unstore.WithMetrics(
				func(kind string) { inst.cacheOriginPull.WithLabelValues(kind).Inc() },
				func(kind string) { inst.cacheOriginHit.WithLabelValues(kind).Inc() },
				func(kind string) { inst.cacheOriginMiss.WithLabelValues(kind).Inc() },
				func(kind string) { inst.cacheOriginUnavailable.WithLabelValues(kind).Inc() },
			),
		)
		entries = append(entries, client)
	}

	return entries
}
