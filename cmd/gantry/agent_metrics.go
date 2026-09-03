// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

// Prometheus metric constructors used by `runAgent`. Each
// `newPhaseNMetrics` function registers a related group of
// instruments and returns a struct of pre-named counter/gauge
// handles for the agent to bump from the relevant subsystem hooks.
//
// Splitting these out of main.go keeps `runAgent` focused on wiring
// dependencies between subsystems instead of declaring metric metadata.

import (
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/inflight"
	"github.com/Azure/unbounded/internal/gantry/manifest"
	"github.com/Azure/unbounded/internal/gantry/metrics"
)

// phase1Metrics groups the metric subset that emits.
type phase1Metrics struct {
	cacheHit           prometheus.Counter
	cacheMiss          prometheus.Counter
	originPullTotal    *prometheus.CounterVec
	originPullSuccess  *prometheus.CounterVec
	originPullFailure  *prometheus.CounterVec
	originBytes        *prometheus.CounterVec
	originFailureTotal *prometheus.CounterVec
}

func newPhase1Metrics(reg *metrics.Registry) *phase1Metrics {
	p := &phase1Metrics{
		cacheHit: reg.NewCounter("cache", prometheus.CounterOpts{
			Name: "p2p_cache_hit_total",
			Help: "Local content-store hits on the containerd-facing mirror endpoint.",
		}),
		cacheMiss: reg.NewCounter("cache", prometheus.CounterOpts{
			Name: "p2p_cache_miss_total",
			Help: "Local content-store misses on the containerd-facing mirror endpoint. A miss may still be served by a peer or cold-start without ever reaching origin.",
		}),
		originPullTotal: reg.NewCounterVec("origin", prometheus.CounterOpts{
			Name: "p2p_origin_pull_total",
			Help: "Origin pulls started, labeled by OCI URL kind.",
		}, []string{"kind"}),
		originPullSuccess: reg.NewCounterVec("origin", prometheus.CounterOpts{
			Name: "p2p_origin_pull_success_total",
			Help: "Origin pulls that streamed to completion.",
		}, []string{"kind"}),
		originPullFailure: reg.NewCounterVec("origin", prometheus.CounterOpts{
			Name: "p2p_origin_pull_failure_total",
			Help: "Origin pulls that failed terminally: classified *OriginError responses from upstream plus downstream commit/copy failures observed after a successful HEAD/body fetch.",
		}, []string{"kind", "class"}),
		originBytes: reg.NewCounterVec("origin", prometheus.CounterOpts{
			Name: "gantry_origin_bytes_total",
			Help: "Bytes read from upstream OCI registries, including partial failed transfers and retries, labeled by OCI content kind.",
		}, []string{"kind"}),
		originFailureTotal: reg.NewCounterVec("mirror", prometheus.CounterOpts{
			Name: "p2p_origin_failure_total",
			Help: "Origin failures observed by the mirror, by class.",
		}, []string{"class"}),
	}

	// Materialize all bounded kind labels at zero so direct-mode preflight can
	// prove that every Gantry pod exposes the byte metric before any pull occurs.
	for _, kind := range []string{"manifest", "config", "layer"} {
		p.originBytes.WithLabelValues(kind).Add(0)
	}

	return p
}

// phase2Metrics groups metrics for peer fallback, DHT advertise,
// and transfer endpoint (the design doc).
type phase2Metrics struct {
	peerServe         prometheus.Counter
	peerServeBytes    *prometheus.CounterVec
	peerMiss          prometheus.Counter
	peerFetch         *prometheus.CounterVec
	peerFetchLastAt   *prometheus.GaugeVec
	peerFetchBytes    *prometheus.CounterVec
	mirrorServeBytes  *prometheus.CounterVec
	mirrorCompletedAt *prometheus.GaugeVec
	layerCompletedAt  *prometheus.GaugeVec
	peerFetchDur      *prometheus.HistogramVec
	peerDialSuccess   prometheus.Counter
	peerDialFailure   prometheus.Counter
	dhtProvide        prometheus.Counter
	dhtProvideErr     *prometheus.CounterVec
	dhtReconcile      prometheus.Counter
	dhtLookup         *prometheus.CounterVec
	dhtLookupDur      *prometheus.HistogramVec
	dhtAdvertise      prometheus.Counter
	cdsubReconnect    prometheus.Counter
}

func newPhase2Metrics(reg *metrics.Registry) *phase2Metrics {
	p := &phase2Metrics{
		peerServe: reg.NewCounter("transfer", prometheus.CounterOpts{
			Name: "p2p_peer_serve_total",
			Help: "Peer-fetch endpoint requests served from the local containerd content store.",
		}),
		peerServeBytes: reg.NewCounterVec("transfer", prometheus.CounterOpts{
			Name: "gantry_peer_serve_bytes_total",
			Help: "Bytes transmitted from this Gantry agent to peer agents, labeled by OCI content kind. Range requests count only transmitted range bytes.",
		}, []string{"kind"}),
		peerMiss: reg.NewCounter("transfer", prometheus.CounterOpts{
			Name: "p2p_peer_miss_total",
			Help: "Peer-fetch endpoint requests that 404'd because the local content store had no entry.",
		}),
		peerFetch: reg.NewCounterVec("mirror", prometheus.CounterOpts{
			Name: "p2p_peer_fetch_total",
			Help: "Peer fetches initiated by the mirror miss path.",
		}, []string{"outcome"}),
		peerFetchLastAt: reg.NewGaugeVec("mirror", prometheus.GaugeOpts{
			Name: "gantry_peer_fetch_last_timestamp_seconds",
			Help: "Unix timestamp of the most recent peer fetch event, retained for busy and stall outcomes.",
		}, []string{"outcome"}),
		peerFetchBytes: reg.NewCounterVec("mirror", prometheus.CounterOpts{
			Name: "gantry_peer_fetch_bytes_total",
			Help: "Bytes received from peer Gantry agents, including partial failed transfers and retries, labeled by OCI content kind.",
		}, []string{"kind"}),
		mirrorServeBytes: reg.NewCounterVec("mirror", prometheus.CounterOpts{
			Name: "gantry_mirror_bytes_served_total",
			Help: "Bytes written by the containerd-facing mirror, labeled by OCI content kind and source path (cache, peer, or origin).",
		}, []string{"kind", "source"}),
		mirrorCompletedAt: reg.NewGaugeVec("mirror", prometheus.GaugeOpts{
			Name: "gantry_mirror_response_completed_timestamp_seconds",
			Help: "Unix timestamp when a complete response body was most recently written to the local containerd client, labeled by content kind and source path.",
		}, []string{"kind", "source"}),
		layerCompletedAt: reg.NewGaugeVec("mirror", prometheus.GaugeOpts{
			Name: "gantry_layer_download_completed_timestamp_seconds",
			Help: "Unix timestamp when a current-image layer response completed to the local containerd client. Zero means pending. Labels are bounded to the current manifest and deleted when the manifest changes.",
		}, []string{"node", "image_digest", "layer_digest", "layer_index"}),
		peerFetchDur: reg.NewHistogramVec("mirror", prometheus.HistogramOpts{
			Name:    "p2p_peer_fetch_duration_seconds",
			Help:    "End-to-end peer-fetch latency from FetchFromPeer dial to terminal outcome (hit = cache commit, error/stall/notfound = first failing branch). Together with p2p_peer_fetch_total{outcome} this isolates dial vs. body vs. commit-time-digest-verification slowness.",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 12),
		}, []string{"outcome"}),
		peerDialSuccess: reg.NewCounter("mirror", prometheus.CounterOpts{
			Name: "p2p_peer_dial_success_total",
			Help: "Successful peer dials from the mirror miss path.",
		}),
		peerDialFailure: reg.NewCounter("mirror", prometheus.CounterOpts{
			Name: "p2p_peer_dial_failure_total",
			Help: "Failed peer dials from the mirror miss path.",
		}),
		dhtProvide: reg.NewCounter("discovery", prometheus.CounterOpts{
			Name: "p2p_dht_provide_total",
			Help: "DHT Provide calls that succeeded.",
		}),
		dhtProvideErr: reg.NewCounterVec("discovery", prometheus.CounterOpts{
			Name: "p2p_dht_provide_error_total",
			Help: "DHT Provide calls that errored, labeled by call site (advertise, peer_fetch_readvertise, cache_reannounce). Without the label a hung kad-dht is indistinguishable from a bad call site.",
		}, []string{"op"}),
		dhtReconcile: reg.NewCounter("discovery", prometheus.CounterOpts{
			Name: "p2p_dht_reconcile_total",
			Help: "cdsub reconciliation cycles completed.",
		}),
		dhtLookup: reg.NewCounterVec("discovery", prometheus.CounterOpts{
			Name: "p2p_dht_lookup_total",
			Help: "DHT FindProviders calls, labeled by outcome.",
		}, []string{"outcome"}),
		dhtLookupDur: reg.NewHistogramVec("discovery", prometheus.HistogramOpts{
			Name:    "p2p_dht_lookup_duration_seconds",
			Help:    "DHT FindProviders call latency in seconds.",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 10),
		}, []string{"outcome"}),
		dhtAdvertise: reg.NewCounter("discovery", prometheus.CounterOpts{
			Name: "p2p_dht_advertise_total",
			Help: "Successful DHT Provide calls issued by the advertiser.",
		}),
		cdsubReconnect: reg.NewCounter("discovery", prometheus.CounterOpts{
			Name: "p2p_cdsub_reconnect_total",
			Help: "cdsub subscriber reconnect attempts.",
		}),
	}

	for _, kind := range []string{"manifest", "config", "layer"} {
		p.peerServeBytes.WithLabelValues(kind).Add(0)
		p.peerFetchBytes.WithLabelValues(kind).Add(0)

		for _, source := range []string{"cache", "peer", "origin"} {
			p.mirrorServeBytes.WithLabelValues(kind, source).Add(0)
			p.mirrorCompletedAt.WithLabelValues(kind, source).Set(0)
		}
	}

	for _, outcome := range []string{
		"hit",
		"notfound",
		"unavailable",
		"digest_mismatch",
		"auth_or_config",
		"server_error",
		"protocol_error",
		"stall",
		"local_error",
		"busy",
	} {
		p.peerFetch.WithLabelValues(outcome).Add(0)
		p.peerFetchDur.WithLabelValues(outcome)
	}

	for _, outcome := range []string{"busy", "stall"} {
		p.peerFetchLastAt.WithLabelValues(outcome).Set(0)
	}

	for _, outcome := range []string{"hit", "miss", "error", "timeout"} {
		p.dhtLookup.WithLabelValues(outcome).Add(0)
		p.dhtLookupDur.WithLabelValues(outcome)
	}

	return p
}

type layerProgressTracker struct {
	mu              sync.Mutex
	gauge           *prometheus.GaugeVec
	node            string
	now             func() time.Time
	manifest        digest.Digest
	layers          map[digest.Digest]string
	completedLayers map[digest.Digest]struct{}
	earlyCompleted  map[digest.Digest]time.Time
	oldLabels       [][]string
}

const maxEarlyLayerCompletions = 256

func newLayerProgressTracker(gauge *prometheus.GaugeVec, node string, now func() time.Time) *layerProgressTracker {
	return &layerProgressTracker{
		gauge:           gauge,
		node:            node,
		now:             now,
		layers:          map[digest.Digest]string{},
		completedLayers: map[digest.Digest]struct{}{},
		earlyCompleted:  map[digest.Digest]time.Time{},
	}
}

func (t *layerProgressTracker) observeManifest(manifestDigest digest.Digest, children []manifest.TypedChild) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.manifest == manifestDigest {
		return
	}

	for _, labels := range t.oldLabels {
		t.gauge.DeleteLabelValues(labels...)
	}

	t.manifest = manifestDigest
	t.layers = make(map[digest.Digest]string, len(children))
	t.completedLayers = make(map[digest.Digest]struct{}, len(children))
	t.oldLabels = t.oldLabels[:0]

	layerIndex := 0

	for _, child := range children {
		if child.Kind != ifaces.KindBlob {
			continue
		}

		index := strconv.Itoa(layerIndex)
		labels := []string{t.node, manifestDigest.String(), child.Digest.String(), index}
		t.layers[child.Digest] = index
		t.oldLabels = append(t.oldLabels, labels)

		completedAt, completed := t.earlyCompleted[child.Digest]
		if completed {
			t.gauge.WithLabelValues(labels...).Set(float64(completedAt.UnixNano()) / float64(time.Second))
			t.completedLayers[child.Digest] = struct{}{}
		} else {
			t.gauge.WithLabelValues(labels...).Set(0)
		}

		layerIndex++
	}

	t.earlyCompleted = map[digest.Digest]time.Time{}
}

func (t *layerProgressTracker) completed(d digest.Digest) {
	t.mu.Lock()
	defer t.mu.Unlock()

	index, ok := t.layers[d]
	if !ok {
		if _, exists := t.earlyCompleted[d]; !exists && len(t.earlyCompleted) < maxEarlyLayerCompletions {
			t.earlyCompleted[d] = t.now()
		}

		return
	}

	if _, ok := t.completedLayers[d]; ok {
		return
	}

	t.gauge.WithLabelValues(t.node, t.manifest.String(), d.String(), index).
		Set(float64(t.now().UnixNano()) / float64(time.Second))

	t.completedLayers[d] = struct{}{}
}

// phase3Metrics groups the instruments owned by // HRW-rank-mismatch detection, DHT-false-empty observability, top-K
// probe hit rate, in-flight pull gauge, cold-start latency, and coord
// stream counters.
type phase3Metrics struct {
	hrwRankMismatch                   *prometheus.CounterVec
	dhtFalseEmpty                     prometheus.Counter
	topkProbeHit                      prometheus.Counter
	coldStartDuration                 *prometheus.HistogramVec
	coordPullIntentServed             prometheus.Counter
	coordPullIntentStorageUnavailable prometheus.Counter
	coordPleasePullServed             prometheus.Counter
	coordPleasePullStarted            prometheus.Counter
	coordPleasePullDeclined           prometheus.Counter
	coordStreamError                  prometheus.Counter
	coordUnauthorizedPeer             *prometheus.CounterVec
	prefetchBatchesTotal              prometheus.Counter
	prefetchDigestsTotal              prometheus.Counter
	prefetchPullersPerBatch           prometheus.Histogram
	prefetchGroupsTotal               *prometheus.CounterVec
}

func newPhase3Metrics(reg *metrics.Registry, infl *inflight.Map) *phase3Metrics {
	// in_flight_pulls is a GaugeFunc that polls inflightMap.Len on
	// every scrape - no separate counter update path needed.
	_ = reg.NewGaugeFunc("coord", prometheus.GaugeOpts{ //nolint:errcheck // best-effort
		Name: "p2p_in_flight_pulls",
		Help: "Current count of in-flight digest pulls on this node.",
	}, func() float64 { return float64(infl.Len()) })

	prefetchGroupsTotal := reg.NewCounterVec("coord", prometheus.CounterOpts{
		Name: "p2p_prefetch_groups_total",
		Help: "Prefetch dispatch groups by local or remote target and success or error outcome.",
	}, []string{"target", "outcome"})

	for _, target := range []string{"local", "remote"} {
		for _, outcome := range []string{"success", "error"} {
			prefetchGroupsTotal.WithLabelValues(target, outcome).Add(0)
		}
	}

	return &phase3Metrics{
		hrwRankMismatch: reg.NewCounterVec("coord", prometheus.CounterOpts{
			Name: "p2p_hrw_rank_mismatch_total",
			Help: "pull_intent_query responses where the responder's reported HRW rank disagrees with the requester's view (informer divergence,).",
		}, []string{"digest_kind"}),
		dhtFalseEmpty: reg.NewCounter("coord", prometheus.CounterOpts{
			Name: "p2p_dht_false_empty_total",
			Help: "Cases where DHT FindProviders returned 0 but a peer's pull_intent_query reported has_cached=true (DHT degradation indicator,).",
		}),
		topkProbeHit: reg.NewCounter("coord", prometheus.CounterOpts{
			Name: "p2p_topk_probe_hit_total",
			Help: "Cold-start cascade resolutions before reaching rule 7 (i.e., the top-K probe avoided an origin pull).",
		}),
		coldStartDuration: reg.NewHistogramVec("coord", prometheus.HistogramOpts{
			Name:    "p2p_cold_start_duration_seconds",
			Help:    "Wall-clock time spent in the cold-start orchestrator per Resolve call.",
			Buckets: prometheus.ExponentialBuckets(0.05, 2, 10),
		}, []string{"digest_kind", "outcome"}),
		coordPullIntentServed: reg.NewCounter("coord", prometheus.CounterOpts{
			Name: "p2p_coord_pull_intent_served_total",
			Help: "pull_intent_query RPCs answered by this node's coord server.",
		}),
		coordPullIntentStorageUnavailable: reg.NewCounter("coord", prometheus.CounterOpts{
			Name: "p2p_coord_pull_intent_storage_unavailable_total",
			Help: "pull_intent_query responses whose has_cached=false answer was caused by the local storage backend (typically containerd) returning ErrUnavailable rather than a definitive miss. Distinguishes \"we genuinely lack the blob\" from \"containerd is unreachable\" so transient storage flaps are observable independently of /readyz (PullIntent path).",
		}),
		coordPleasePullServed: reg.NewCounter("coord", prometheus.CounterOpts{
			Name: "p2p_coord_please_pull_served_total",
			Help: "please_pull RPCs answered by this node's coord server.",
		}),
		coordPleasePullStarted: reg.NewCounter("coord", prometheus.CounterOpts{
			Name: "p2p_coord_please_pull_started_total",
			Help: "Digests transitioned to in_flight via please_pull on this node.",
		}),
		coordPleasePullDeclined: reg.NewCounter("coord", prometheus.CounterOpts{
			Name: "p2p_coord_please_pull_declined_total",
			Help: "Digests this node declined to start via please_pull because the puller-pump was at its concurrent-pull ceiling or shutting down (reported as OUTCOME_UNSPECIFIED). A sustained nonzero rate means designated pullers are saturated and requesters are falling through to direct-origin fallback (NF5).",
		}),
		coordStreamError: reg.NewCounter("coord", prometheus.CounterOpts{
			Name: "p2p_coord_stream_error_total",
			Help: "Inbound coord streams dropped without a normal reply: malformed or oversized envelopes, read/decode/deadline failures, concurrent-stream-limit drops, dispatch or serve errors, and response marshal/write failures. Enforce-mode peer-authz rejections are NOT counted here (they are tracked, by reason, in p2p_coord_unauthorized_peer_total), so enabling peer authz does not inflate this protocol-error signal.",
		}),
		coordUnauthorizedPeer: reg.NewCounterVec("coord", prometheus.CounterOpts{
			Name: "p2p_coord_unauthorized_peer_total",
			Help: "Inbound coord requests whose libp2p peer ID was not authorized against the membership view, labeled by reason: \"unrecognized\" (membership has published peer IDs but none match the dialing peer) or \"unevaluable\" (no member has published a peer ID yet, only reported in enforce mode). Fires in observe-only for recognized misses and in enforce mode for both. Verify peer-id annotations are published before using zero as an enforcement-readiness signal.",
		}, []string{"reason"}),
		prefetchBatchesTotal: reg.NewCounter("coord", prometheus.CounterOpts{
			Name: "p2p_prefetch_batches_total",
			Help: "Speculative manifest-pre-fan PleasePull batches dispatched (one per distinct HRW rank-0 puller per manifest serve,).",
		}),
		prefetchDigestsTotal: reg.NewCounter("coord", prometheus.CounterOpts{
			Name: "p2p_prefetch_digests_total",
			Help: "Layer/config digests carried in speculative manifest-pre-fan batches (cumulative sum across batches,).",
		}),
		prefetchPullersPerBatch: reg.NewHistogram("coord", prometheus.HistogramOpts{
			Name:    "p2p_prefetch_pullers_per_manifest",
			Help:    "Distribution of distinct HRW rank-0 pullers contacted per manifest pre-fan call.",
			Buckets: prometheus.ExponentialBuckets(1, 2, 11),
		}),
		prefetchGroupsTotal: prefetchGroupsTotal,
	}
}

// phase4Metrics groups the instruments owned by the
// negative-cache entry gauge + hit counters, and the designated-
// puller takeover counter. The takeover counter is incremented from
// the cold-start orchestrator (requester side); the cache metrics
// come from negcache.Cache callbacks (puller side).
type phase4Metrics struct {
	size                          atomic.Int64
	hits                          *prometheus.CounterVec
	enters                        *prometheus.CounterVec
	designatedPullerTakeoverTotal *prometheus.CounterVec
}

func newPhase4Metrics(reg *metrics.Registry) *phase4Metrics {
	p := &phase4Metrics{}
	_ = reg.NewGaugeFunc("coord", prometheus.GaugeOpts{ //nolint:errcheck // best-effort
		Name: "p2p_negative_cache_entries",
		Help: "Active negative-cache entries on this puller (per-digest cooldowns).",
	}, func() float64 { return float64(p.size.Load()) })
	p.hits = reg.NewCounterVec("coord", prometheus.CounterOpts{
		Name: "p2p_negative_cache_hit_total",
		Help: "Lookups against the negative cache that returned an active cooldown, by failure class.",
	}, []string{"class"})
	p.enters = reg.NewCounterVec("coord", prometheus.CounterOpts{
		Name: "p2p_negative_cache_enter_total",
		Help: "New or extended negative-cache entries by failure class.",
	}, []string{"class"})
	p.designatedPullerTakeoverTotal = reg.NewCounterVec("coord", prometheus.CounterOpts{
		Name: "p2p_designated_puller_takeover_total",
		Help: "Cold-start observations where the rank-0 puller's in-flight pull was older than the stall threshold, triggering a takeover by the next-ranked node.",
	}, []string{"digest_kind"})

	return p
}

func (p *phase4Metrics) observeEnter(class ifaces.FailureClass) {
	p.enters.WithLabelValues(failureClassLabel(class)).Inc()
}

func (p *phase4Metrics) observeHit(class ifaces.FailureClass) {
	p.hits.WithLabelValues(failureClassLabel(class)).Inc()
}

func (p *phase4Metrics) setSize(n int) { p.size.Store(int64(n)) }

func failureClassLabel(c ifaces.FailureClass) string {
	if c == ifaces.FailureUnspecified {
		return "unspecified"
	}

	return string(c)
}

// phase5Metrics groups the instruments owned by the
// DHT health gauge, direct-origin-fallback direct-origin fallback counter, and top-K
// expansion counter.
type phase5Metrics struct {
	originFallbackTotal        prometheus.Counter
	originFallbackDeclineTotal *prometheus.CounterVec
	topkExpansionTotal         *prometheus.CounterVec
}

func newPhase5Metrics(reg *metrics.Registry, healthScore func() float64) *phase5Metrics {
	p := &phase5Metrics{}
	_ = reg.NewGaugeFunc("discovery", prometheus.GaugeOpts{ //nolint:errcheck // best-effort
		Name: "p2p_dht_health_score",
		Help: " geometric-mean DHT health score in [0, 1] (routing-table coverage × p95 lookup latency score × self-test success rate).",
	}, healthScore)
	p.originFallbackTotal = reg.NewCounter("mirror", prometheus.CounterOpts{
		Name: "p2p_origin_fallback_total",
		Help: " NF5 direct-origin fallback pulls (last-resort path after cold-start exhaustion).",
	})
	p.originFallbackDeclineTotal = reg.NewCounterVec("mirror", prometheus.CounterOpts{
		Name: "p2p_origin_fallback_decline_total",
		Help: " NF5 gating-sequence declines by reason. Without this counter a never-firing NF5 looks identical in metrics to a never-eligible NF5.",
	}, []string{"reason"})
	p.topkExpansionTotal = reg.NewCounterVec("coord", prometheus.CounterOpts{
		Name: "p2p_topk_expansion_total",
		Help: "Cold-start cascade expansions from top-K to top-(K × factor) by reason (degraded DHT, all top-K unreachable).",
	}, []string{"reason"})

	return p
}

// phase6Metrics is REMOVED in its sole instrument was
// the forced-eviction counter that tracked Gantry's hostPath cache
// LRU eviction, and that cache no longer exists. containerd's own GC
// is now responsible for blob lifetime and exposes its own metrics
// via the containerd Prometheus endpoint.

// phase9Metrics groups the new instruments added in // to surface the containerd-as-truth model on the wire:
//
// - storage_mode_info: a per-mode gauge fixed at 1 so dashboards
// can group panels by which backend is in use without scraping
// a fleet-wide label. Dimension is "mode" (currently always
// "containerd"; the label exists so a future remix can flip
// between modes without breaking the recording rules).
// - advertise_reconcile_*: the advertiser's reconcile
// loop instrumentation. Pairs (duration histogram + digest
// count gauge + reconcile counters) describe one full pass.
// - withdraw_*: counter pair around DHT.Withdraw, the // equivalent of dht_provide_*. With kad-dht's 24h TTL, the
// primary signal is the rate of attempted withdrawals (it
// should track the rate of container deletions on the node).
// - containerd_lease_*: lease lifecycle counters. Active
// gauge is not maintained here (would require a List call on
// scrape); created + cleanup_error counters are bumped by the
// hooks wired into pre-ingest lease creation and CleanupExpiredLeases.
// - containerd_ingest_*: paired counters around ingest paths so
// an operator can see "of N pulls, how many committed to
// containerd". Failures here are downstream/cache-side, not
// origin-side; that's why this is separate from
// origin_failure_total.
// - origin_stream_* + containerd_commit_*: live mirror
// stream-through observability. These counters deliberately split
// "we proxied the bytes" from "containerd later showed the digest
// in its content inventory" so the agent does not pretend a local
// commit happened just because the HTTP stream completed.
type phase9Metrics struct {
	storageMode                *prometheus.GaugeVec
	advReconcileTotal          prometheus.Counter
	advReconcileError          prometheus.Counter
	advReconcileUnavailable    prometheus.Counter
	advReconcileDur            prometheus.Histogram
	advReconcileDigestCount    prometheus.Gauge
	advReconcileAdded          prometheus.Counter
	advReconcileRemoved        prometheus.Counter
	withdrawTotal              prometheus.Counter
	withdrawError              prometheus.Counter
	containerdLeaseCreated     prometheus.Counter
	containerdLeaseReleased    prometheus.Counter
	containerdLeaseActive      prometheus.Gauge
	containerdLeaseCleanupErr  prometheus.Counter
	containerdIngestTotal      prometheus.Counter
	containerdIngestFailure    prometheus.Counter
	containerdHit              prometheus.Counter
	containerdMiss             prometheus.Counter
	containerdUnavailable      prometheus.Counter
	containerdOpenError        prometheus.Counter
	originStreamStarted        *prometheus.CounterVec
	originStreamCompleted      *prometheus.CounterVec
	originStreamFailed         *prometheus.CounterVec
	containerdCommitObserved   prometheus.Counter
	containerdCommitObservedAt prometheus.Gauge
	containerdCommitObserveDur prometheus.Histogram
	containerdCommitLatestDur  prometheus.Gauge
	dhtStaleOnly               prometheus.Counter
	staleProviderFiltered      prometheus.Counter
	commitMissingAfterStream   prometheus.Counter
	advertiseTotal             prometheus.Counter
	advertiseError             prometheus.Counter
}

func newPhase9Metrics(reg *metrics.Registry) *phase9Metrics {
	return &phase9Metrics{
		storageMode: reg.NewGaugeVec("storage", prometheus.GaugeOpts{
			Name: "gantry_storage_mode_info",
			Help: "Storage backend in use. Fixed at 1 with the active mode in the \"mode\" label; absent series means that mode is not active.",
		}, []string{"mode"}),
		advReconcileTotal: reg.NewCounter("discovery", prometheus.CounterOpts{
			Name: "gantry_advertise_reconcile_total",
			Help: "Total reconcile passes the advertiser has completed (successful or not).",
		}),
		advReconcileError: reg.NewCounter("discovery", prometheus.CounterOpts{
			Name: "gantry_advertise_reconcile_error_total",
			Help: "Reconcile passes that aborted on an Inventory error (no diff applied; previous announced set preserved).",
		}),
		advReconcileUnavailable: reg.NewCounter("discovery", prometheus.CounterOpts{
			Name: "gantry_advertise_reconcile_unavailable_total",
			Help: "Reconcile passes that aborted because the inventory backend (containerd) was unavailable. Distinct from generic errors so dashboards can separate transient backend hiccups from real misconfigurations. Per plan : the announced set is preserved across these - no spurious Withdraws.",
		}),
		advReconcileDur: reg.NewHistogram("discovery", prometheus.HistogramOpts{
			Name:    "gantry_advertise_reconcile_duration_seconds",
			Help:    "End-to-end advertiser reconcile pass duration. Includes inventory snapshot + diff + every Provide + every Withdraw call.",
			Buckets: prometheus.ExponentialBuckets(0.05, 2, 10),
		}),
		advReconcileDigestCount: reg.NewGauge("discovery", prometheus.GaugeOpts{
			Name: "gantry_advertise_reconcile_digest_count",
			Help: "Size of the inventory snapshot at the last reconcile pass. Drift between this and containerd's actual content store indicates Inventory misses .",
		}),
		advReconcileAdded: reg.NewCounter("discovery", prometheus.CounterOpts{
			Name: "gantry_advertise_reconcile_added_total",
			Help: "Digests Provide'd because they appeared in the inventory since the last pass.",
		}),
		advReconcileRemoved: reg.NewCounter("discovery", prometheus.CounterOpts{
			Name: "gantry_advertise_reconcile_removed_total",
			Help: "Digests Withdraw'n because they disappeared from the inventory since the last pass.",
		}),
		withdrawTotal: reg.NewCounter("discovery", prometheus.CounterOpts{
			Name: "gantry_withdraw_total",
			Help: "Successful DHT.Withdraw calls (currently a no-op at the libp2p layer; we count the intent so a future protocol-level withdraw is observable without dashboard changes).",
		}),
		withdrawError: reg.NewCounter("discovery", prometheus.CounterOpts{
			Name: "gantry_withdraw_error_total",
			Help: "DHT.Withdraw calls that errored.",
		}),
		containerdLeaseCreated: reg.NewCounter("storage", prometheus.CounterOpts{
			Name: "gantry_containerd_lease_created_total",
			Help: "Containerd content-store leases attached by Gantry on ingest (Plan).",
		}),
		containerdLeaseReleased: reg.NewCounter("storage", prometheus.CounterOpts{
			Name: "gantry_containerd_lease_released_total",
			Help: "Containerd leases deleted by Gantry - either by the periodic cleanup loop after their TTL expired, or by the startup sweep, or by an explicit per-ingest abort. Pair with gantry_containerd_lease_created_total to spot leases that outlive their TTL.",
		}),
		containerdLeaseActive: reg.NewGauge("storage", prometheus.GaugeOpts{
			Name: "gantry_containerd_lease_active",
			Help: "Best-effort estimate of Gantry-owned leases currently live in containerd. Sampled at every cleanup pass - between samples this gauge can drift; trust the counters for accurate rates.",
		}),
		containerdLeaseCleanupErr: reg.NewCounter("storage", prometheus.CounterOpts{
			Name: "gantry_containerd_lease_cleanup_error_total",
			Help: "Errors returned by the periodic expired-lease sweep loop.",
		}),
		containerdIngestTotal: reg.NewCounter("storage", prometheus.CounterOpts{
			Name: "gantry_containerd_ingest_total",
			Help: "Successful commits into the containerd content store via Gantry's runOriginPull path.",
		}),
		containerdIngestFailure: reg.NewCounter("storage", prometheus.CounterOpts{
			Name: "gantry_containerd_ingest_failure_total",
			Help: "runOriginPull commits that failed at the containerd layer (digest mismatch, ingest I/O error, lease conflict).",
		}),
		containerdHit: reg.NewCounter("storage", prometheus.CounterOpts{
			Name: "gantry_containerd_hit_total",
			Help: "Successful local containerd content-store Has/Open checks across all Gantry subsystems (mirror local-presence check, transfer peer serving, coord pull_intent_query, advertiser openability probe). Workload-facing mirror hits are also exposed separately as p2p_cache_hit_total.",
		}),
		containerdMiss: reg.NewCounter("storage", prometheus.CounterOpts{
			Name: "gantry_containerd_miss_total",
			Help: "Local containerd content-store Has/Open checks that returned no openable entry, across all Gantry subsystems (see gantry_containerd_hit_total for the call sites). Workload-facing mirror misses are also exposed separately as p2p_cache_miss_total.",
		}),
		containerdUnavailable: reg.NewCounter("storage", prometheus.CounterOpts{
			Name: "gantry_containerd_unavailable_total",
			Help: "Local containerd content-store Has/Open calls that surfaced ifaces.ErrUnavailable from the backend, across all subsystems. A sustained non-zero rate means containerd is sick; the mirror and transfer endpoints respond accordingly (503 from transfer; mirror does NOT treat as a miss).",
		}),
		containerdOpenError: reg.NewCounter("storage", prometheus.CounterOpts{
			Name: "gantry_containerd_open_error_total",
			Help: "Open/ReaderAt calls that returned an error other than ErrNotFound or ErrUnavailable, across all subsystems. Anything non-zero warrants log inspection.",
		}),
		originStreamStarted: reg.NewCounterVec("origin", prometheus.CounterOpts{
			Name: "gantry_origin_stream_started_total",
			Help: "Live mirror requests that entered the direct-origin stream-through path. Labeled by digest kind so manifest-vs-layer traffic stays distinguishable.",
		}, []string{"kind"}),
		originStreamCompleted: reg.NewCounterVec("origin", prometheus.CounterOpts{
			Name: "gantry_origin_stream_completed_total",
			Help: "Live direct-origin stream-through responses that fully completed and digest-verified in-process. This does NOT imply the requesting containerd committed the digest yet.",
		}, []string{"kind"}),
		originStreamFailed: reg.NewCounterVec("origin", prometheus.CounterOpts{
			Name: "gantry_origin_stream_failed_total",
			Help: "Live direct-origin stream-through attempts that failed before completion (origin-side error, truncated body, or digest mismatch). Labeled by digest kind.",
		}, []string{"kind"}),
		containerdCommitObserved: reg.NewCounter("storage", prometheus.CounterOpts{
			Name: "gantry_containerd_commit_observed_total",
			Help: "Completed live stream-through responses whose digest later appeared in the local containerd inventory within the verification window. This is the truthful post-stream commit signal for live mirror traffic.",
		}),
		containerdCommitObservedAt: reg.NewGauge("storage", prometheus.GaugeOpts{
			Name: "gantry_containerd_commit_observed_timestamp_seconds",
			Help: "Unix timestamp when containerd inventory most recently showed a digest from a completed live stream-through response.",
		}),
		containerdCommitObserveDur: reg.NewHistogram("storage", prometheus.HistogramOpts{
			Name:    "gantry_containerd_commit_observation_duration_seconds",
			Help:    "Time from a digest-verified live stream-through response completing to the digest appearing in containerd inventory. Resolution is bounded by the inventory probe interval.",
			Buckets: prometheus.ExponentialBuckets(0.25, 2, 9),
		}),
		containerdCommitLatestDur: reg.NewGauge("storage", prometheus.GaugeOpts{
			Name: "gantry_containerd_commit_latest_observation_duration_seconds",
			Help: "Most recent measured time from a digest-verified live stream-through response completing to the digest appearing in containerd inventory. Resolution is bounded by the inventory probe interval.",
		}),
		dhtStaleOnly: reg.NewCounter("discovery", prometheus.CounterOpts{
			Name: "gantry_dht_stale_only_total",
			Help: "Mirror cache-miss requests where the DHT returned candidate providers but every candidate was filtered (stale, suspicious, self, unavailable) before any peer fetch was attempted. Treated as if DHT returned empty - falls through to cold-start. Distinct from gantry_dht_lookup_total{outcome=\"miss\"} which counts true empty results.",
		}),
		staleProviderFiltered: reg.NewCounter("discovery", prometheus.CounterOpts{
			Name: "gantry_stale_provider_filtered_total",
			Help: "Total provider candidates removed from a DHT lookup result by the stale/suspicious/unavailable cache before fetch attempt. Per plan - measures how much DHT noise the local stale cache is absorbing.",
		}),
		commitMissingAfterStream: reg.NewCounter("storage", prometheus.CounterOpts{
			Name: "gantry_containerd_commit_missing_after_stream_total",
			Help: "Stream-through mirror responses that completed successfully but the digest did NOT appear in local containerd inventory within the verification window. Indicates either kubelet aborted the pull mid-stream or Gantry's response completed without a later containerd commit. Containerd-unavailable probe windows do NOT count as missing; correlation pauses until inventory is available again. \"Origin metric semantics\".",
		}),
		advertiseTotal: reg.NewCounter("discovery", prometheus.CounterOpts{
			Name: "gantry_advertise_total",
			Help: "Successful per-digest DHT.Provide calls issued by the advertiser (renamed sibling of p2p_dht_advertise_total - counts the same events; kept under both names so dashboards built before the rename keep working).",
		}),
		advertiseError: reg.NewCounter("discovery", prometheus.CounterOpts{
			Name: "gantry_advertise_error_total",
			Help: "DHT.Provide calls from the advertiser that returned an error. Pair with gantry_advertise_total for an error rate; nonzero in a healthy cluster means DHT routing-table churn.",
		}),
	}
}
