// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package mirror is the loopback OCI registry mirror containerd talks to
// via hosts.toml .
//
// endpoint contract (cited from architecture.md the API contract and
// the design doc):
//
//	GET /v2/ 200, {"api":"registry/2.0"}
//	GET /healthz 200, "ok"
//	GET /v2/<repo>/manifests/<tag> 503, empty body
//	GET /v2/<repo>/manifests/sha256:<hex> cache or origin
//	GET /v2/<repo>/blobs/sha256:<hex> cache or origin
//
// The tag-manifests 503 is the "tag fallthrough" - hosts.toml lists
// origin as the next entry, so containerd retries against origin directly.
// Returning 503 (NOT 404) is load-bearing: hosts.toml only falls through
// on 5xx, NOT on 4xx. Returning the wrong code breaks tag-resolution.
//
// ?ns=<registry> routing (the design doc): containerd adds ?ns=<host> to every
// request when hosts.toml specifies `server=<origin>`. If exactly one
// upstream is configured, ?ns= is optional (and ignored if present). When
// more than one upstream is configured, ?ns= MUST match one of them or
// the request returns 404 - there is no safe default.
package mirror

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/containerd/errdefs"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/digestpipe"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/oci"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
)

const providerFailureSweepInterval = time.Minute

const authenticationChallengeTimeout = 2 * time.Second

type AuthenticationChallenger interface {
	AuthenticationChallenge(ctx context.Context, registry string) (challenge string, required bool, err error)
}

// Server is the mirror HTTP handler.
type Server struct {
	cfg     *config.Config
	store   ifaces.LocalContentStore
	origin  ifaces.OriginPuller
	auth    AuthenticationChallenger
	logger  *slog.Logger
	metrics metricsHooks

	// dependencies - nil-safe. When both dht and peer are set,
	// the cache miss path tries DHT-discovered providers before origin.
	dht  ifaces.DHT
	peer ifaces.PeerDialer

	// cold-start orchestrator (the design doc 7-rule cascade). When set,
	// it is consulted when DHT.FindProviders returns an empty provider
	// set, before the request falls through to origin.
	coldStart ColdStartResolver

	// direct-origin-fallback direct-origin fallback controller (the design doc). When set,
	// the mirror is permitted to do a controlled direct origin pull
	// after the cold-start cascade reports ErrColdStartExhausted (and
	// the direct-origin-fallback gating sequence passes). When nil, cold-start exhaustion
	// always returns 5xx.
	nf5 *DirectOriginFallbackController

	// Speculative layer prefetcher (the design doc detailed-design L332 / architecture
	// L180). When set, every successful manifest serve fires a
	// fire-and-forget OnManifestServed callback so the prefetcher can
	// parse the body, group child digests by HRW rank-0 puller, and
	// issue batched please_pull RPCs before containerd asks for the
	// layers. Nil-safe.
	prefetcher LayerPrefetcher

	// tunables (zero values fall back to package defaults).
	peerLookupBudget time.Duration
	peerFetchBudget  time.Duration
	maxPeerAttempts  int
	// peerRediscoverBudget, when > 0, enables the re-discovery loop: the
	// mirror keeps re-running FindProviders and retrying peer fetches for up
	// to this total wall-clock budget so it picks up finisher-seeds that
	// advertise mid-swarm before falling to origin. Zero keeps the historical
	// single-shot provider attempt.
	peerRediscoverBudget  time.Duration
	peerRediscoverBackoff time.Duration
	selfNodeID            ifaces.NodeID
	selfPeerID            ifaces.NodeID

	staleProviderTTL         time.Duration
	unavailablePeerTTL       time.Duration
	suspiciousPeerTTL        time.Duration
	providerFailureMu        sync.Mutex
	staleProviders           map[providerDigestKey]time.Time
	suspiciousProviders      map[providerDigestKey]time.Time
	unavailableProviders     map[string]time.Time
	nextProviderFailureSweep time.Time

	// defaultUpstream is the upstream to use when exactly one is
	// configured and ?ns= is absent.
	defaultUpstream string

	// negCache is the negative-cache integration for the
	// direct-origin path. Optional (nil-safe). See
	// WithNegativeCacheRecorder for the contract and the
	// rationale.
	negCache NegativeCacheRecorder

	// liveStreamThrough switches live GET cache-miss handling from
	// Gantry-owned ingest to direct stream-through. When true, peer and
	// origin responses are proxied straight to the requesting
	// containerd/kubelet client and hash-checked as they pass through,
	// but Gantry does NOT write the bytes into s.cache itself. If the
	// final digest check fails, some bytes have already reached
	// containerd; containerd remains the final verifier and should reject
	// the commit. That avoids same-digest
	// writer races now that the active store in production is the
	// containerd content store itself. Background please_pull / direct-origin-fallback
	// ingest continues to land via runOriginPull in cmd/gantry/main.go.
	liveStreamThrough bool

	// draining is set to true via Drain when the agent is shutting
	// down. Once true, every /v2/ request returns 503 immediately so
	// containerd's hosts.toml falls through to origin (// graceful-shutdown contract). The check is layered ON TOP of
	// http.Server.Shutdown so that even keep-alive connections that
	// the kernel has already accepted get a 503 instead of normal
	// handling once Drain has fired.
	draining atomic.Bool

	// startupGated, together with `ready`, implements the // startup mirror gate. The mirror's TCP listener accepts traffic
	// from containerd's hostPort plumbing the moment ListenAndServe
	// returns - well before /readyz can pass (members informer sync,
	// DHT routing-table convergence, self-announce patch, cache
	// scan). Without a handler-level gate, image pulls during the
	// startup window would race the agent's own bootstrap: the
	// DHT-empty branch would route to origin instead of to the
	// coordinated cold-start path, and every restarting pod would
	// add its own direct origin pulls to the cluster's total. That
	// silently shreds the cache-hit invariant for the duration of the
	// startup window.
	//
	// startupGated is set by WithStartupReadinessGate; when set, the
	// /v2/ handler returns 503 (containerd hosts.toml falls through
	// to origin for THAT request, exactly the same as the shutdown
	// drain) until MarkReady is called. Default-false so existing
	// test fixtures (which build Server without the option) continue
	// to serve immediately.
	startupGated bool

	// ready is a sticky atomic flag: false until MarkReady is
	// called once, then true forever. Sticky so a /readyz blip
	// (e.g. DHT routing table briefly empty during informer churn)
	// does NOT take the mirror out of service mid-rollout - the
	// startup gate is a one-shot 'wait for first ready' and Drain
	// handles graceful shutdown separately.
	ready atomic.Bool
}

// Drain flips the mirror into shutdown mode: new /v2/ requests return
// 503 immediately. Idempotent. Safe to call from a signal handler.
func (s *Server) Drain() { s.draining.Store(true) }

// MarkReady flips the startup gate from "not yet ready" to "serving"
// for production deployments that opted into WithStartupReadinessGate.
// Sticky: subsequent /readyz flaps do NOT take the mirror back out of
// service - once we have decided to serve we stay serving until Drain.
// Safe to call multiple times; safe to call from any goroutine. No-op
// for Servers that did not opt into the startup gate.
func (s *Server) MarkReady() { s.ready.Store(true) }

type metricsHooks struct {
	onCacheHit                func()
	onCacheMiss               func()
	onOriginSuccess           func(kind string, bytes int64)
	onOriginDownstreamFailure func(kind, class string)
	onOriginStreamStarted     func(kind string)
	onOriginStreamCompleted   func(kind string)
	onOriginStreamFailed      func(kind string)
	onLiveStreamCompleted     func(d digest.Digest)
	onPeerFetch               func(outcome string)
	onMirrorBytesServed       func(kind, source string, bytes int64)
	onMirrorResponseCompleted func(d digest.Digest, kind, source string)
	onPeerFetchLatency        func(outcome string, d time.Duration)
	onPeerDialResult          func(success bool)
	onDhtLookup               func(outcome string, dur time.Duration)
	onProvideError            func(op string)
	onDhtStaleOnly            func()
	onStaleProviderFiltered   func(n int)
}

// Option configures Server construction.
type Option func(*Server)

// WithLogger plumbs a structured logger into the mirror handler.
func WithLogger(l *slog.Logger) Option {
	return func(s *Server) {
		if l != nil {
			s.logger = l.With(slog.String("subsystem", "mirror"))
		}
	}
}

// WithMetrics registers metric callbacks for cache hit and cache
// miss observed by the mirror. The origin pull-family counters are
// intentionally NOT plumbed here - they're split across origin and
// mirror to keep one source of truth per counter:
//
// - p2p_origin_pull_total{kind} and p2p_origin_failure_total{class}
// belong to origin.WithMetrics in the origin Client. Origin is
// the single chokepoint that both the mirror direct-origin path
// and the coordinated please_pull / runOriginPull goroutine
// route through, so counting there means dashboards see one
// source of truth and the operator-facing "is origin sick?"
// alert (p2p_origin_failure_total) stays consistent across both
// paths and free of false positives from downstream failures.
// - p2p_origin_pull_success_total{kind} belongs to the mirror
// (WithOriginSuccessMetric) because origin can't know whether
// the caller actually committed bytes - see that option's doc.
// - p2p_origin_pull_failure_total{kind,class} is fed from BOTH
// halves: origin's failure hook bumps it on true origin-side
// failures (with double-bump of p2p_origin_failure_total), and
// the mirror's WithDownstreamFailureMetric bumps it on
// downstream failures (with class=transient, NO double-bump of
// p2p_origin_failure_total).
//
// Counting any of these at the mirror's WithMetrics hook would
// silently undercount the please_pull-coordinated path (the bulk of
// pulls on a hot cluster) and break the started == success + failure
// + in-flight arithmetic identity that all three counters rely on.
func WithMetrics(cacheHit, cacheMiss func()) Option {
	return func(s *Server) {
		s.metrics.onCacheHit = cacheHit
		s.metrics.onCacheMiss = cacheMiss
	}
}

// WithByteMetrics registers a callback for bytes written to the local
// containerd caller, split by cache, peer, or origin source path. Peer receive
// bytes are measured separately at transfer.Client's response-body boundary.
func WithByteMetrics(mirrorBytesServed func(kind, source string, bytes int64)) Option {
	return func(s *Server) {
		s.metrics.onMirrorBytesServed = mirrorBytesServed
	}
}

// WithOriginSuccessMetric registers a callback fired by the mirror's
// direct-origin path AFTER it has streamed the response body to
// completion AND committed the bytes to cache (or, when cache is
// unavailable, AFTER the direct-stream digest verifier confirms the
// served bytes match the requested digest). The kind label uses the
// design-doc Prometheus vocabulary (see ifaces.OriginRefKind.MetricLabel).
//
// This hook is the mirror-side half of the origin-success contract:
// origin.Client.Pull no longer reports success itself because it has
// no way to know whether the caller actually drained and verified the
// stream. HEAD requests (which by design never read the body),
// io.Copy interruptions, and cache-commit failures all leave the
// response body Closed without a real success - so reporting success
// on Close inside origin.Client inflated p2p_origin_pull_success_total
// against operations that never produced a usable byte. The puller
// pump's runOriginPull owns the equivalent hook on the
// please_pull-coordinated path; together they're the two places that
// know what "the origin pull actually succeeded" means.
func WithOriginSuccessMetric(originSuccess func(kind string, bytes int64)) Option {
	return func(s *Server) {
		s.metrics.onOriginSuccess = originSuccess
	}
}

// WithDownstreamFailureMetric registers a callback fired by the
// mirror's direct-origin path when the body has been received from
// origin but a DOWNSTREAM step (io.Copy stall, cw.Commit digest
// mismatch / cache I/O error, directVerifier mismatch) fails before
// the cluster has produced a usable artifact.
//
// Why this is separate from the origin failure-hook
// (origin.WithMetrics' failure closure in cmd/gantry/main.go):
// - origin.WithMetrics' failure closure is the origin-side
// terminal counter - it bumps BOTH p2p_origin_pull_failure_total
// (operator dashboards) AND p2p_origin_failure_total (the
// "is origin sick?" alert). Origin-side failures are the
// ones where the origin pull never started, never returned
// 2xx, or returned a non-2xx body. Counting downstream
// failures (where origin DID return 2xx but the body
// stalled / corrupted en route to the cache) against the
// same closure would falsely accuse origin of being sick.
// - This hook bumps ONLY p2p_origin_pull_failure_total
// (per-(kind,class) detail) with class="transient", leaving
// p2p_origin_failure_total reserved for true origin-side
// failures. Operators see the failure detail without the
// alert false-positive.
//
// Together with onOriginSuccess and the origin-side failure
// closure, this restores the per-pull arithmetic identity
// for the GET path:
//
//	p2p_origin_pull_total{kind} == p2p_origin_pull_success_total{kind}
//	 + p2p_origin_pull_failure_total{kind,class=any}
//	 + (in-flight at scrape time)
//
// This constraint ensures the missing terminal counter
// for downstream failures as the second of the two reasons that
// identity drifted positive in production traces. (The first
// was HEAD, fixed by adding origin.Head.)
func WithDownstreamFailureMetric(downstreamFailure func(kind, class string)) Option {
	return func(s *Server) {
		s.metrics.onOriginDownstreamFailure = downstreamFailure
	}
}

// WithLiveStreamThrough enables the "Mode A: live mirror requests
// - stream-through" contract for cache misses handled on behalf of the
// local containerd mirror client. When enabled, the mirror no longer
// writes live peer/origin responses into the active store; it proxies
// them directly to the caller and relies on the caller's containerd to
// perform the final commit.
func WithLiveStreamThrough() Option {
	return func(s *Server) {
		s.liveStreamThrough = true
	}
}

// WithOriginStreamMetrics wires the the live-stream-through origin
// counters. Hooks fire only from the direct-origin stream-through path:
// start at the moment the mirror commits to the origin path, completed
// after the full body has been proxied and the final digest check passes,
// and failed on any terminal error before that completion point.
func WithOriginStreamMetrics(started, completed, failed func(kind string)) Option {
	return func(s *Server) {
		s.metrics.onOriginStreamStarted = started
		s.metrics.onOriginStreamCompleted = completed
		s.metrics.onOriginStreamFailed = failed
	}
}

// WithLiveStreamCompletedHook registers a callback fired after any live
// stream-through response (peer or origin) fully completes and passes the
// final digest check.
// Callers use this to correlate the response with a later containerd
// inventory observation without forcing the mirror to ingest the bytes
// itself.
func WithLiveStreamCompletedHook(onCompleted func(d digest.Digest)) Option {
	return func(s *Server) {
		s.metrics.onLiveStreamCompleted = onCompleted
	}
}

// WithMirrorResponseCompletedHook registers a callback after a complete GET
// response body has been written successfully to the local containerd client.
// It is not fired for HEAD requests, partial streams, or failed copies.
func WithMirrorResponseCompletedHook(onCompleted func(d digest.Digest, kind, source string)) Option {
	return func(s *Server) {
		s.metrics.onMirrorResponseCompleted = onCompleted
	}
}

// NegativeCacheRecorder is the negative-cache integration the
// mirror's direct-origin path uses to mirror what the coordinated
// puller-pump path (cmd/gantry/main.go's runOriginPull) already does:
// classify a terminal origin / downstream failure into an
// ifaces.FailureClass and seed the per-puller cooldown ladder so the
// next request for the same digest short-circuits via the same
// `recently_failed` propagation the please_pull path uses.
//
// Why this exists (a prior review): before this hook, the mirror's
// direct-origin path - including the direct-origin-fallback fallback that fires
// after the cold-start cascade reports ErrColdStartExhausted -
// recorded the failure metric but did NOT enter a negative-cache
// cooldown. The next direct-origin-fallback-eligible request for the same digest could
// re-fire the direct-origin pull at the bottom of the next jitter
// window, even though the previous attempt had stalled mid-stream or
// digest-mismatched at commit. The puller-pump path correctly drops
// such retries on the recently_failed cooldown; the mirror direct
// path did not. That gap is a retry-amplification hardening hole, not
// a metrics bug - fireOriginDownstreamFailure was already wired by
// fixes.
//
// Contract:
//
// - RecordFailure is invoked once per terminal mirror-direct origin
// failure, BEFORE the response is finalized. The class is taken
// from *ifaces.OriginError when origin returns one; downstream
// failures (io.Copy / cw.Commit / directVerifier.Verify) are
// recorded as FailureTransient - the same classification the
// puller-pump path uses for those exact paths.
// - RecordSuccess is invoked exactly once per successful
// mirror-direct origin pull, AFTER cw.Commit (or the direct-
// stream digest verifier) passes. It clears any prior cooldown
// so the next failure restarts the ladder from Initial (the design doc
// "Self-healing"). Symmetric with the puller-pump path's
// neg.RecordSuccess(d) call after a successful Commit.
//
// HEAD requests deliberately do NOT touch the negative cache: the
// coordinated path never issues HEAD, and HEAD does not warm the
// cache, so recording HEAD failures would diverge the two paths'
// cooldown semantics with no observability win.
type NegativeCacheRecorder interface {
	RecordFailure(d digest.Digest, class ifaces.FailureClass)
	RecordSuccess(d digest.Digest)
}

// WithNegativeCacheRecorder wires the design doc negative-cache integration
// into the mirror's direct-origin path. See NegativeCacheRecorder
// for the contract. Nil-safe: passing nil leaves the mirror behaving
// exactly as it did before this option existed (metric-only failure
// reporting; no cooldown propagation to subsequent direct-origin
// attempts on the same node).
func WithNegativeCacheRecorder(rec NegativeCacheRecorder) Option {
	return func(s *Server) { s.negCache = rec }
}

// WithPeerMetrics registers peer-fallback metric callbacks.
// peerFetchOutcome labels include: "hit", "notfound", "unavailable",
// "auth_or_config", "server_error", "protocol_error",
// "digest_mismatch", "stall", and "local_error".
// peerDialResult is invoked per attempted dial.
func WithPeerMetrics(peerFetchOutcome func(outcome string), peerDialResult func(success bool)) Option {
	return func(s *Server) {
		s.metrics.onPeerFetch = peerFetchOutcome
		s.metrics.onPeerDialResult = peerDialResult
	}
}

// WithPeerFetchLatencyMetric registers a hook that fires once per
// fetchOneProvider call with the terminal outcome label and the
// wall-clock time from the FetchFromPeer dial to either the cache
// commit (hit) or the failing-branch return. Used for the
// p2p_peer_fetch_duration_seconds{outcome} histogram so operators can
// see whether peer fetches are slow because of dial latency, body
// streaming, or commit-time digest verification.
func WithPeerFetchLatencyMetric(onPeerFetchLatency func(outcome string, d time.Duration)) Option {
	return func(s *Server) {
		s.metrics.onPeerFetchLatency = onPeerFetchLatency
	}
}

// WithDhtLookupMetric registers a hook that fires once per FindProviders
// call with the outcome label ("hit", "miss", "timeout", "error") and the
// observed lookup duration. Used to populate p2p_dht_lookup_total and
// p2p_dht_lookup_duration_seconds (the design doc).
func WithDhtLookupMetric(onLookup func(outcome string, dur time.Duration)) Option {
	return func(s *Server) {
		s.metrics.onDhtLookup = onLookup
	}
}

// WithProvideErrorMetric registers a hook that fires when the mirror's
// post-peer-fetch dht.Provide call fails. The hook receives a stable
// label string identifying the call site so a CounterVec keyed by `op`
// can distinguish mirror-internal Provide failures from other sites.
func WithProvideErrorMetric(onProvideErr func(op string)) Option {
	return func(s *Server) {
		s.metrics.onProvideError = onProvideErr
	}
}

// WithDhtStaleOnlyMetric registers a hook that fires when a DHT
// lookup returned candidate providers but the local stale/suspicious/
// unavailable/self filters removed every one before any peer fetch
// was attempted - treated as a "stale-only" outcome so dashboards
// can separate true empty DHT responses (counted under
// gantry_dht_lookup_total{outcome="miss"}) from "DHT had providers
// but they were all dead". Per "DHT stale-only" mitigation.
func WithDhtStaleOnlyMetric(onStaleOnly func()) Option {
	return func(s *Server) {
		s.metrics.onDhtStaleOnly = onStaleOnly
	}
}

// WithStaleProviderFilteredMetric registers a hook that fires once
// per DHT lookup, reporting the total number of provider candidates
// removed by the local filter caches (stale + unavailable +
// suspicious + self). n may be zero. Used to size the local
// false-positive surface from the DHT layer per .
func WithStaleProviderFilteredMetric(onFiltered func(n int)) Option {
	return func(s *Server) {
		s.metrics.onStaleProviderFiltered = onFiltered
	}
}

// WithDiscovery wires P2P fetch: cache miss -> DHT FindProviders ->
// PeerDialer.FetchFromPeer (across up to 3 providers) -> origin fallback.
// Either argument nil disables P2P fallback entirely (behavior).
func WithDiscovery(d ifaces.DHT, peer ifaces.PeerDialer) Option {
	return func(s *Server) {
		s.dht = d
		s.peer = peer
	}
}

// WithPeerBudgets overrides the default peer-path budgets.
// lookup ≤ 0 means "use default 2s"; fetch ≤ 0 means "use default 1h";
// maxAttempts ≤ 0 means "use default 3".
func WithPeerBudgets(lookup, fetch time.Duration, maxAttempts int) Option {
	return func(s *Server) {
		s.peerLookupBudget = lookup
		s.peerFetchBudget = fetch
		s.maxPeerAttempts = maxAttempts
	}
}

// WithPeerRediscover enables the peer re-discovery loop. budget is the total
// wall-clock time the mirror keeps re-running DHT FindProviders and retrying
// peer fetches on a cache miss before falling to origin; it lets a node pick
// up finisher-seeds that advertise mid-swarm instead of exhausting a fixed
// provider set. backoff is the pause between rounds (<= 0 uses a built-in 1s
// default). budget <= 0 disables re-discovery, restoring the single-shot
// provider attempt.
func WithPeerRediscover(budget, backoff time.Duration) Option {
	return func(s *Server) {
		s.peerRediscoverBudget = budget
		s.peerRediscoverBackoff = backoff
	}
}

// WithSelfNodeID configures the local Kubernetes node identity used to filter
// stale self provider records after a local cache miss. Membership/cold-start
// providers use Kubernetes node names as NodeID values.
func WithSelfNodeID(id ifaces.NodeID) Option {
	return func(s *Server) { s.selfNodeID = id }
}

// WithSelfPeerID configures the local libp2p peer identity used to filter stale
// self provider records from DHT lookup results after a local cache miss. DHT
// providers use libp2p peer IDs as NodeID values.
func WithSelfPeerID(id ifaces.NodeID) Option {
	return func(s *Server) { s.selfPeerID = id }
}

// WithProviderFailureCacheTTL configures TTLs used to suppress immediate
// retries against recently-failed providers.
func WithProviderFailureCacheTTL(staleTTL, unavailableTTL, suspiciousTTL time.Duration) Option {
	return func(s *Server) {
		s.staleProviderTTL = staleTTL
		s.unavailablePeerTTL = unavailableTTL
		s.suspiciousPeerTTL = suspiciousTTL
	}
}

// ColdStartResolver is the subset of *coldstart.Resolver that mirror
// needs. Kept narrow for testability - production wires the concrete
// resolver via WithColdStart.
type ColdStartResolver interface {
	Resolve(ctx context.Context, d digest.Digest, kind ifaces.OriginRefKind, registry, repository string, expectedSize int64) (*ColdStartResolution, error)
}

// ColdStartResolution mirrors *coldstart.Resolution at this boundary
// so the mirror package does not import internal/coldstart (which
// would import internal/mirror by transitivity through wiring).
type ColdStartResolution struct {
	Providers []ifaces.Provider
	Outcome   string
}

// WithColdStart wires cold-start orchestration. When set, the
// orchestrator is consulted on the DHT-empty branch of the cache-miss
// path before falling through to origin.
func WithColdStart(c ColdStartResolver) Option {
	return func(s *Server) { s.coldStart = c }
}

// WithNF5 wires the direct-origin fallback controller.
// When non-nil and cold-start exits via ErrColdStartExhausted, the
// mirror runs the direct-origin-fallback gating sequence (jitter, token bucket, dedup,
// re-check) before falling through to a direct origin pull. When nil,
// cold-start exhaustion always returns 5xx.
func WithNF5(c *DirectOriginFallbackController) Option {
	return func(s *Server) { s.nf5 = c }
}

// LayerPrefetcher is the speculative wire-level optimisation hook
// (the design doc detailed-design.md L332 / architecture.md L180). After the
// mirror serves a manifest successfully the mirror invokes
// OnManifestServed in a goroutine so an implementation can fetch
// the just-cached manifest body, parse it, identify child
// layer/config digests, group them by HRW rank-0 puller, and issue
// batched please_pull RPCs to warm the cluster before containerd
// asks for the layers. The mirror never waits for the callback to
// return; failures are the prefetcher's to log.
type LayerPrefetcher interface {
	OnManifestServed(ctx context.Context, registry, repository string, manifestDigest digest.Digest)
}

// WithLayerPrefetcher wires a speculative layer prefetcher. Nil-safe.
func WithLayerPrefetcher(p LayerPrefetcher) Option {
	return func(s *Server) { s.prefetcher = p }
}

// WithStartupReadinessGate opts the mirror into the the startup
// gate: until MarkReady is called, every /v2/ request returns 503
// with reason "agent starting up". Production callers should pair
// this with a goroutine that polls the same conditions /readyz uses
// and calls MarkReady once they converge - see cmd/gantry/main.go's
// readyCheck-poller for the canonical wiring.
//
// Without this option the Server is "ready immediately" so unit-test
// fixtures (which never call MarkReady) continue to behave as before.
// The shutdown drain (Drain / drainGuard) is independent of this
// gate and always installed.
func WithStartupReadinessGate() Option {
	return func(s *Server) { s.startupGated = true }
}

// New builds a Server bound to the given local content store and origin.
func New(cfg *config.Config, store ifaces.LocalContentStore, origin ifaces.OriginPuller, opts ...Option) *Server {
	s := &Server{
		cfg:                  cfg,
		store:                store,
		origin:               origin,
		logger:               slog.Default().With(slog.String("subsystem", "mirror")),
		staleProviders:       map[providerDigestKey]time.Time{},
		suspiciousProviders:  map[providerDigestKey]time.Time{},
		unavailableProviders: map[string]time.Time{},
		staleProviderTTL:     3 * time.Minute,
		unavailablePeerTTL:   30 * time.Second,
		suspiciousPeerTTL:    5 * time.Minute,
	}
	if auth, ok := origin.(AuthenticationChallenger); ok {
		s.auth = auth
	}

	for _, opt := range opts {
		opt(s)
	}

	if len(cfg.UpstreamRegistries) == 1 {
		s.defaultUpstream = cfg.UpstreamRegistries[0].Name
	}
	// Default ready=true so unit-test fixtures, which never call
	// MarkReady, continue to serve immediately. Production callers
	// flip startupGated via WithStartupReadinessGate which forces
	// ready=false at construction and gates the /v2/ handler until
	// MarkReady fires.
	if !s.startupGated {
		s.ready.Store(true)
	}

	return s
}

// Handler returns an http.Handler suitable for serving on cfg.MirrorListen.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	// Order matters: drainGuard runs FIRST so shutdown wins over a
	// concurrent startup transition (we never want to flip from
	// "starting up" back to serving via a stale MarkReady). The
	// startupGate runs INSIDE drainGuard so a still-not-ready agent
	// also returns 503.
	mux.HandleFunc("/v2/", s.drainGuard(s.startupGate(s.handleV2)))
	mux.HandleFunc("/v2", s.drainGuard(s.startupGate(s.handleV2))) // some clients omit trailing slash

	return mux
}

// drainGuard wraps a /v2/ handler so that once Drain has been called,
// every new request gets a 503 instead of normal handling. // "stops accepting new mirror requests with 503". The 503 (not 404)
// is load-bearing - hosts.toml only falls through on 5xx.
func (s *Server) drainGuard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.draining.Load() {
			w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
			http.Error(w, "agent shutting down", http.StatusServiceUnavailable)

			return
		}

		h(w, r)
	}
}

// startupGate returns 503 until MarkReady is called, but only for
// Servers that opted in via WithStartupReadinessGate. The 503 is
// load-bearing in exactly the same way Drain's 503 is: containerd's
// hosts.toml falls through to origin for the un-served request.
// Without this gate the mirror serves /v2/ traffic the moment
// ListenAndServe returns, racing the agent's own DHT/members/coord
// bootstrap and routing every startup-window pull straight to origin
// outside the coordinated cold-start path.
func (s *Server) startupGate(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.ready.Load() {
			w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
			w.Header().Set("Retry-After", "5")
			http.Error(w, "agent starting up", http.StatusServiceUnavailable)

			return
		}

		h(w, r)
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok") //nolint:errcheck // best-effort write
}

// handleV2 is the OCI Distribution v2 entry point.
func (s *Server) handleV2(w http.ResponseWriter, r *http.Request) {
	// Common headers.
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	// Keep the requester's request-scoped registry identity attached to this
	// request as it moves through peer lookup, please_pull, and origin. Basic
	// and Bearer auth are accepted for delegation. An unsupported or malformed
	// credential falls through to containerd's next host rather than silently
	// switching to this agent's configured fallback identity.
	rawAuthorization := strings.TrimSpace(r.Header.Get("Authorization"))

	authorization := registryauth.Normalize(rawAuthorization)
	if rawAuthorization != "" && authorization == "" {
		http.Error(w, "unsupported registry authorization", http.StatusServiceUnavailable)

		return
	}

	r = r.WithContext(registryauth.WithAuthorization(r.Context(), authorization))

	path := r.URL.Path
	if path == "/v2/" || path == "/v2" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`) //nolint:errcheck // best-effort write

		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	repo, kind, ref, ok := oci.ParseV2Path(path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	upstream, err := s.resolveUpstream(r)
	if err != nil {
		// The ?ns= value is not in upstream_registries. Log at Warn so
		// operators notice misconfigured registry lists - this causes
		// containerd to bypass the mirror entirely for that registry,
		// defeating P2P distribution silently at Info log level.
		s.logger.Warn("mirror: ignoring request for unconfigured registry - add it to upstream_registries",
			slog.String("ns", r.URL.Query().Get("ns")),
			slog.String("path", path),
		)
		http.NotFound(w, r)

		return
	}

	if !isDigestRef(ref) {
		// Tag request (the design doc) - fall through to origin via hosts.toml.
		// 503 (not 404) so containerd retries against the next mirror.
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	d, err := digest.Parse(ref)
	if err != nil {
		http.Error(w, "invalid digest", http.StatusBadRequest)
		return
	}

	if authorization == "" && s.auth != nil {
		challengeCtx, cancel := context.WithTimeout(r.Context(), authenticationChallengeTimeout)
		challenge, required, challengeErr := s.auth.AuthenticationChallenge(challengeCtx, upstream)

		cancel()

		if challengeErr != nil {
			s.logger.Warn("mirror: registry authentication challenge unavailable",
				slog.String("registry", upstream),
				slog.Any("err", challengeErr),
			)
			http.Error(w, "registry authentication unavailable", http.StatusServiceUnavailable)

			return
		}

		if required {
			w.Header().Set("WWW-Authenticate", challenge)
			http.Error(w, "authentication required", http.StatusUnauthorized)

			return
		}
	}

	s.serveDigest(w, r, upstream, repo, d, kind)
}

func (s *Server) resolveUpstream(r *http.Request) (string, error) {
	ns := r.URL.Query().Get("ns")
	if ns == "" {
		if s.defaultUpstream != "" {
			return s.defaultUpstream, nil
		}

		return "", errors.New("mirror: ?ns= is required when multiple upstreams are configured")
	}

	if ur, ok := s.cfg.ResolveUpstream(ns); ok {
		return ur.Name, nil
	}

	return "", fmt.Errorf("mirror: unknown ns=%q", ns)
}

// serveDigest serves a digest-addressed manifest or blob: cache hit, then
// origin pull with stream-and-cache fallback.
func (s *Server) serveDigest(w http.ResponseWriter, r *http.Request, upstream, repo string, d digest.Digest, kind ifaces.OriginRefKind) {
	ctx := r.Context()
	logger := s.logger.With(
		slog.String("registry", upstream),
		slog.String("repo", repo),
		slog.String("digest", d.String()),
		slog.String("kind", kind.String()),
	)

	// 1. Local content-store lookup.
	if handled := s.serveLocalHit(ctx, w, r, d, kind, upstream, repo, logger); handled {
		return
	}

	s.bumpCacheMiss()

	// 1a. HEAD short-circuit (fourteenth-review fix). See serveHeadMiss
	// for the rationale (metadata-only requests MUST NOT please_pull,
	// peer-GET, or bump p2p_origin_pull_total).
	if r.Method == http.MethodHead {
		s.serveHeadMiss(ctx, w, d, kind, upstream, repo, logger)
		return
	}

	// 2. Peer fallback . If both DHT and PeerDialer are wired,
	// try up to maxPeerAttempts providers from FindProviders. The result
	// is tri-state per design the design doc's "v1 transfer policy":
	// - served: bytes already written from a peer; we're done.
	// - exhausted: DHT had providers but all maxAttempts failed (stall
	// or error). Return 5xx so containerd's hosts.toml mirror chain
	// promotes the request to origin directly. The agent does *not*
	// do a direct origin pull here (direct-origin-fallback owns the controlled
	// direct-origin path).
	// - unused: DHT not wired, errored, or returned empty providers.
	// Fall through to origin path; HRW probe
	// replaces this leg for the cold-start case.
	if s.dht != nil && s.peer != nil {
		switch s.tryPeerFallback(ctx, w, r, d, kind, upstream, repo, logger) {
		case peerFallbackLocalHit:
			return
		case peerFallbackServed:
			// Live stream-through proxies the body straight to containerd, so a
			// served manifest reaches the shared content store on containerd's
			// commit rather than ours. The prefetcher waits for it there, so this
			// must fire in both modes or cold-start seeding never runs.
			s.firePrefetch(ctx, kind, upstream, repo, d)

			return
		case peerFallbackPartial:
			// Response headers and a verified prefix were already written. Let
			// the truncated response close so containerd can retry; writing a
			// 503 body here would corrupt the content stream.
			return
		case peerFallbackExhausted:
			http.Error(w, "warm path exhausted", http.StatusServiceUnavailable)
			return
		case peerFallbackColdExhausted:
			// the design doc direct-origin-fallback last-resort: only attempt a direct origin
			// pull when the controller passes its gating sequence
			// (bootstrap done, DHT healthy enough, no dedup
			// collision, token budget, jitter elapsed without
			// recheck finding a provider).
			if s.nf5 == nil {
				http.Error(w, "warm path exhausted", http.StatusServiceUnavailable)
				return
			}

			proceed, release, err := s.nf5.Allow(ctx, d, kind, 0)
			if release != nil {
				defer release()
			}

			if err != nil || !proceed {
				http.Error(w, "warm path exhausted", http.StatusServiceUnavailable)
				return
			}
			// Fall through to the origin pull below.
		}
	}

	// 3. Origin pull, stream-and-cache (or live stream-through).
	s.serveFromOrigin(ctx, w, d, kind, upstream, repo, logger)
}

// serveLocalHit serves d from the local content store when present.
// Returns true when the response has been fully written (cache hit or
// non-NotFound store error -> 5xx); false means the digest is
// definitively absent and the caller should continue with the
// miss-path logic.
func (s *Server) serveLocalHit(ctx context.Context, w http.ResponseWriter, r *http.Request, d digest.Digest, kind ifaces.OriginRefKind, upstream, repo string, logger *slog.Logger) bool {
	rc, size, err := s.store.Open(ctx, d)
	if err == nil {
		defer func() { _ = rc.Close() }() //nolint:errcheck // best-effort close

		s.bumpCacheHit()
		// Sniff the first bytes so writeBlobHeaders can label content
		// with its real mediaType for two cases (see
		// writeBlobHeadersWithPrefix for the full story):
		// - kind == KindBlob: a manifest returned for a
		// /blobs/<digest> request (via origin's /blobs/->/manifests/
		// fallback) would otherwise be octet-stream and containerd
		// CRI fails with "Target.MediaType must be set".
		// - kind == KindManifest: a manifest LIST/index body would
		// otherwise be labelled as a single OCI manifest and
		// containerd fails the unpack with "expected manifest but
		// found index".
		br := bufio.NewReader(rc)

		var sniff []byte

		if kind == ifaces.KindBlob || kind == ifaces.KindManifest {
			if peek, _ := br.Peek(512); len(peek) > 0 { //nolint:errcheck // peek best-effort for logging
				sniff = peek
			}
		}

		writeBlobHeadersWithPrefix(w, d, size, kind, sniff)

		if r.Method == http.MethodHead {
			return true
		}

		s.firePrefetch(ctx, kind, upstream, repo, d)

		written, err := io.Copy(w, br)
		s.fireMirrorBytesServed(kind, "cache", written)

		if err != nil {
			logger.Debug("mirror: copy from cache failed", slog.Any("err", err))
		} else {
			s.fireMirrorResponseCompleted(d, kind, "cache")
		}

		return true
	}

	var enf *ifaces.ErrNotFound
	if errors.As(err, &enf) {
		return false
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		logger.Debug("mirror: cache open canceled", slog.Any("err", err))
		return true
	}

	var eun *ifaces.ErrUnavailable
	if errors.As(err, &eun) {
		logger.Warn("mirror: storage unavailable", slog.Any("err", err))
		http.Error(w, "storage unavailable", http.StatusServiceUnavailable)

		return true
	}

	logger.Warn("mirror: cache open error", slog.Any("err", err))
	http.Error(w, "cache error", http.StatusInternalServerError)

	return true
}

// serveHeadMiss satisfies a HEAD request for a digest the local store
// does not have. HEAD is purely metadata: containerd uses it to learn
// the blob's Content-Length / existence before issuing a GET. It MUST
// NOT:
// - send please_pull RPCs (would commit cluster-wide work on a
// metadata probe),
// - body-GET from a peer (would cache-warm and burn peer fetch
// budget for a request that never reads the body),
// - bump p2p_origin_pull_total (HEAD is the origin.Head path, not
// the origin.Pull path; success and downstream-failure can never
// fire here, so counting starts breaks the arithmetic).
//
// Before the fourteenth-review fix the HEAD branch lived AFTER the
// peer/cold-start cascade, so a HEAD cache-miss with DHT providers
// would still trigger a full peer GET into local cache, and a HEAD
// cache-miss with an empty DHT would still consult cold-start
// (potentially please_pull-ing). Branching here keeps the metric
// contract simple: HEAD is metadata-only, GET is byte-producing and
// cache-warming. A subsequent GET for the same digest takes the
// normal cache-miss path and warms the cache then.
func (s *Server) serveHeadMiss(ctx context.Context, w http.ResponseWriter, d digest.Digest, kind ifaces.OriginRefKind, upstream, repo string, logger *slog.Logger) {
	pRef := ifaces.OriginRef{Registry: upstream, Repository: repo, Digest: d, Kind: kind}

	hsize, hct, herr := s.origin.Head(ctx, pRef)
	if herr != nil {
		writeOriginError(w, herr, logger)
		return
	}
	// Propagate the upstream Content-Type so containerd builds a
	// descriptor with the right media type at HEAD time. Without
	// it a HEAD on a manifest-list digest later fails the unpack
	// with "expected manifest but found index" because containerd
	// trusts the HEAD-time type over the body bytes.
	if hct != "" {
		w.Header().Set("Content-Type", hct)
	}

	writeBlobHeaders(w, d, hsize, kind)
}

// serveFromOrigin runs the origin-pull + cache-write path (or, when
// WithLiveStreamThrough is set, the proxy-and-correlate path). Called
// after the local-store miss + peer/cold-start cascade have decided
// that this node is the designated origin puller for d.
//
// Metric placement (correction):
// - p2p_origin_pull_total{kind} bumps inside origin.Pull at
// entry (via origin.WithMetrics' onPullStart hook).
// - p2p_origin_pull_failure_total{kind,class} +
// p2p_origin_failure_total{class} bump inside
// origin.recordFailure on origin-side terminal failures
// (same WithMetrics closure double-bumps both).
// - p2p_origin_pull_success_total{kind} bumps HERE after
// cw.Commit succeeds (and analogously in runOriginPull after
// that path's Commit). Success cannot live in origin because
// origin has no way to know whether the caller actually
// committed the bytes to cache.
//
// HEAD takes the separate s.origin.Head path explicitly so it does
// NOT bump p2p_origin_pull_total: HEAD is metadata-only, it never
// warms the cache, so counting it as a pull-attempt inflated the
// started counter against operations that could fire neither success
// (no commit) nor downstream-failure (no body copy). See
// origin.Client.Head's comment for the full rationale. HEAD is
// short-circuited in serveHeadMiss above so it never reaches this
// section.
func (s *Server) serveFromOrigin(ctx context.Context, w http.ResponseWriter, d digest.Digest, kind ifaces.OriginRefKind, upstream, repo string, logger *slog.Logger) {
	pRef := ifaces.OriginRef{Registry: upstream, Repository: repo, Digest: d, Kind: kind}

	if s.liveStreamThrough {
		s.fireOriginStreamStarted(kind)
	}

	pr, psize, perr := s.origin.Pull(ctx, pRef)
	if perr != nil {
		// the design doc negative-cache: classify and record the origin-side
		// failure so the next direct-origin attempt for the same
		// digest on this node short-circuits on the recently_failed
		// cooldown. Symmetric with the puller-pump path's
		// recordOriginFailure(neg, d, err, "origin pull failed", ...)
		// in cmd/gantry/main.go. Without this, direct-origin-fallback direct fallback
		// could fire again on the next request through this node at
		// the bottom of the next jitter window, retry-amplifying
		// against an origin that just returned 4xx/5xx.
		// Requester-specific origin failures cannot be shared safely in a
		// cache keyed only by digest. A later request may carry another token.
		if registryauth.Authorization(ctx) == "" {
			s.recordNegCacheFailure(d, perr)
		}

		if s.liveStreamThrough {
			s.fireOriginStreamFailed(kind)
		}

		writeOriginError(w, perr, logger)

		return
	}

	defer func() { _ = pr.Close() }() //nolint:errcheck // best-effort close

	if s.liveStreamThrough {
		written, streamErr := streamDigestToClient(w, pr, d, psize, kind)
		s.fireMirrorBytesServed(kind, "origin", written)

		if streamErr != nil {
			logger.Debug("mirror: live origin stream failed",
				slog.Int64("written", written),
				slog.Any("err", streamErr),
			)
			s.fireOriginStreamFailed(kind)
			s.recordNegCacheFailure(d, streamErr)

			return
		}

		s.fireOriginStreamCompleted(kind)
		s.fireMirrorResponseCompleted(d, kind, "origin")
		s.fireLiveStreamCompleted(d)
		s.recordNegCacheSuccess(d)

		return
	}

	cw, cwerr := s.store.Writer(ctx, d)

	var dest io.Writer

	var directVerifier *digestpipe.Writer // non-nil only when caching is unavailable

	if cwerr == nil {
		defer func() { _ = cw.Abort(ctx) }() //nolint:errcheck // no-op after Commit

		dest = io.MultiWriter(w, cw)
	} else {
		logger.Warn("mirror: cache writer unavailable; serving without caching", slog.Any("err", cwerr))
		// digest-verification says the cache layer is what enforces digest verification
		// on origin pulls - and cache.Writer wraps the stream in a
		// digestpipe internally before Commit. When that path is
		// unavailable we still need to verify, otherwise an origin
		// returning corrupted bytes (truncation, content-injection
		// proxy, etc.) leaks straight to the client with no detection.
		// We can't unsend the bytes, but logging a digest mismatch
		// here is the only signal operators get that the origin lied.
		directVerifier = digestpipe.New(w)
		dest = directVerifier
	}

	// Peek the origin body so writeBlobHeaders can label content with
	// the right Content-Type for two cases:
	// - kind == KindBlob: a manifest that arrived via origin's
	// /blobs/->/manifests/ fallback would otherwise be labelled
	// octet-stream, and containerd CRI rejects the unpacked
	// content as "Target.MediaType must be set".
	// - kind == KindManifest: a manifest LIST/index body must be
	// labelled with the matching list/index media type, otherwise
	// containerd fails the unpack with "expected manifest but
	// found index" when it later resolves children.
	// The peek consumes nothing (bufio buffers the bytes).
	prBuf := bufio.NewReader(pr)

	var sniff []byte

	if kind == ifaces.KindBlob || kind == ifaces.KindManifest {
		if peek, _ := prBuf.Peek(512); len(peek) > 0 { //nolint:errcheck // peek best-effort for logging
			sniff = peek
		}
	}

	writeBlobHeadersWithPrefix(w, d, psize, kind, sniff)

	written, err := io.Copy(dest, prBuf)
	s.fireMirrorBytesServed(kind, "origin", written)

	if err != nil {
		// Bytes already sent; we can't undo. Cache will be aborted by defer.
		// This is a terminal downstream failure: origin returned 2xx
		// and we drained part of the body, but the stream stalled
		// before EOF. Count it against p2p_origin_pull_failure_total
		// (class=transient) so the per-pull arithmetic
		// (started == success + failure + in_flight) holds. We do
		// NOT also bump p2p_origin_failure_total - origin gave us
		// 2xx, the failure is downstream.
		logger.Debug("mirror: copy stalled", slog.Int64("written", written), slog.Any("err", err))
		s.fireOriginDownstreamFailure(kind, ifaces.FailureTransient)
		// the design doc cooldown: io.Copy stalls are the canonical mid-stream
		// truncation a prior review flagged for direct-origin-fallback direct
		// fallback. Classify as transient (matches the puller-pump
		// path) so the cooldown ladder grows on repeated truncations
		// of the same digest.
		s.recordNegCacheFailure(d, err)

		return
	}

	if directVerifier != nil {
		if verr := directVerifier.Verify(d); verr != nil {
			logger.Error("mirror: origin direct-stream digest mismatch - corrupted bytes were already served to client",
				slog.String("digest", d.String()),
				slog.Int64("written", written),
				slog.Any("err", verr),
			)
			// Corrupted bytes: do NOT count as origin success.
			// The client already got them, but the cluster did
			// not produce a usable cached/verifiable copy. This
			// is a downstream-detected failure (origin returned
			// 2xx; we caught the mismatch via the in-process
			// digestpipe verifier) so it goes to the downstream
			// counter, not to origin's failure family.
			s.fireOriginDownstreamFailure(kind, ifaces.FailureTransient)
			// the design doc cooldown: a direct-stream digest mismatch is the
			// strongest "this origin is lying" signal we have on the
			// no-cache path. Treat the failure as transient (same
			// class the puller-pump path uses for cw.Commit
			// mismatches) so repeated mismatches grow the cooldown.
			s.recordNegCacheFailure(d, verr)

			return
		}
		// Direct-stream verifier passed: bytes were delivered to
		// the client AND digest-matched the requested ref. The
		// cluster did not gain a cache entry (cache was
		// unavailable), but the origin pull itself succeeded
		// end-to-end. Count it.
		s.fireOriginSuccess(kind, written)
		// the design doc "Self-healing": a successful end-to-end pull clears
		// any prior cooldown entry. Symmetric with the puller-pump
		// path's neg.RecordSuccess(d) after cw.Commit.
		s.recordNegCacheSuccess(d)

		return
	}

	if cwerr == nil {
		if err := cw.Commit(ctx); err != nil {
			// The client already got the bytes; cache just doesn't keep them.
			// cw.Commit is where the cache's internal digestpipe runs;
			// a non-nil error here means EITHER cache I/O failed OR
			// the stream's digest didn't match d. Either way it's a
			// terminal downstream failure of THIS pull (no usable
			// cached copy produced) and must move the arithmetic
			// off in-flight. Origin returned 2xx, so we route this
			// to the downstream counter, NOT to the origin failure
			// family.
			logger.Warn("mirror: cache commit failed", slog.Any("err", err))
			s.fireOriginDownstreamFailure(kind, ifaces.FailureTransient)
			// the design doc cooldown: cw.Commit is where the cache's digestpipe
			// fires; this branch means EITHER cache I/O failed OR
			// origin's bytes didn't hash to d. Both are transient by
			// the puller-pump path's classification - record so the
			// next direct-origin attempt for the same digest waits
			// out the cooldown.
			s.recordNegCacheFailure(d, err)

			return
		}
		// Re-advertise into the DHT now that we hold a byte-identical
		// copy in our cache. Without this, an direct-origin-fallback-eligible direct
		// origin pull leaves the cluster's only known provider record
		// pointing at the origin instead of at this node - defeating
		// the deduplication promise of the step 7 specifically for
		// the cold-start-exhausted path that just escalated to origin.
		s.reAdvertiseDigest(d, "mirror_origin_announce", logger)
		s.firePrefetch(ctx, kind, upstream, repo, d)
		// Bytes streamed AND committed: this is the canonical
		// mirror-direct origin-pull success. Fire AFTER commit
		// (not after Copy) so a commit failure correctly leaves
		// the operation classified as not-yet-successful even
		// though the client already got the bytes.
		s.fireOriginSuccess(kind, written)
		// the design doc "Self-healing": clear any prior cooldown entry so
		// the next failure restarts the ladder from Initial.
		// Symmetric with the puller-pump path's neg.RecordSuccess(d)
		// after its cw.Commit.
		s.recordNegCacheSuccess(d)
	}
}

// peerFallbackResult is the outcome of tryPeerFallback.
type peerFallbackResult int

const (
	// peerFallbackUnused means the DHT layer was bypassed: no DHT call
	// fired (caller-gated), or it errored, or it returned no providers.
	// The caller may fall through to origin (behavior).
	peerFallbackUnused peerFallbackResult = iota
	// peerFallbackLocalHit means cold-start populated the local cache after
	// serveDigest's initial miss and the response has already been written.
	peerFallbackLocalHit
	// peerFallbackServed means a peer's bytes were fully delivered to the
	// client. In default mode they were verified+committed to cache first;
	// in live-stream-through mode they were proxied directly and the final
	// digest check passed after proxying. Caller must not write further bytes.
	peerFallbackServed
	// peerFallbackPartial means live stream-through delivered a prefix but
	// exhausted its re-discovery budget before completing the digest. The
	// caller must close the response without writing an HTTP error body.
	peerFallbackPartial
	// peerFallbackExhausted means the DHT returned providers but all
	// maxAttempts of them failed (stall or error), OR the cold-start
	// cascade short-circuited with an error other than
	// ErrColdStartExhausted (failure short-circuit, transient
	// cooldown, etc.). Per the design doc's v1 transfer policy and the design doc's
	// trusted-cluster-wide failure propagation, the mirror must
	// return 5xx - direct-origin-fallback must NOT fire here.
	peerFallbackExhausted
	// peerFallbackColdExhausted means the cold-start cascade ran to
	// its final ErrColdStartExhausted exit (no cache, no in-flight,
	// no provider returned by HRW + DHT, both top-K and top-2K
	// already tried). direct-origin-fallback direct-origin fallback is eligible to fire
	// - and only here.
	peerFallbackColdExhausted
)

type peerFetchOutcomeKind int

const (
	peerFetchOutcomeHit peerFetchOutcomeKind = iota
	peerFetchOutcomeStaleProvider
	peerFetchOutcomeUnavailable
	peerFetchOutcomeDigestMismatch
	peerFetchOutcomeAuthOrConfigError
	peerFetchOutcomePeerServerError
	peerFetchOutcomeProtocolError
	peerFetchOutcomeStall
	peerFetchOutcomeLocalError
	// peerFetchOutcomeBusy is a peer that answered 429: it is alive and
	// healthy but at its serve cap. It is deliberately NOT a hard failure and
	// the provider is NOT quarantined; the re-discovery loop retries it (or a
	// finisher) on the next round.
	peerFetchOutcomeBusy
)

type peerAttemptResult struct {
	outcome peerFetchOutcomeKind
	served  bool
}

type livePeerStream struct {
	verifier  *digestpipe.Writer
	totalSize int64
	started   bool
}

func (s *livePeerStream) offset() int64 {
	if s == nil || s.verifier == nil {
		return 0
	}

	return s.verifier.Written()
}

func (s *livePeerStream) append(w http.ResponseWriter, src io.Reader, d digest.Digest, size int64, kind ifaces.OriginRefKind) (int64, bool, error) {
	reader := src

	if !s.started {
		br := bufio.NewReader(src)

		var sniff []byte

		if kind == ifaces.KindBlob || kind == ifaces.KindManifest {
			if peek, _ := br.Peek(512); len(peek) > 0 { //nolint:errcheck // best-effort media sniff
				sniff = peek
			}
		}

		writeBlobHeadersWithPrefix(w, d, size, kind, sniff)
		s.verifier = digestpipe.New(w)
		s.totalSize = size
		s.started = true
		reader = br
	} else if size != s.totalSize {
		return 0, false, fmt.Errorf("peer resume size changed from %d to %d", s.totalSize, size)
	}

	written, err := io.Copy(s.verifier, reader)
	switch offset := s.offset(); {
	case offset > s.totalSize:
		return written, false, fmt.Errorf("peer stream exceeded content size: wrote %d, want %d", offset, s.totalSize)
	case offset < s.totalSize && err != nil:
		return written, false, err
	case offset < s.totalSize:
		return written, false, io.ErrUnexpectedEOF
	}

	if err := s.verifier.Verify(d); err != nil {
		return written, false, err
	}

	return written, true, nil
}

type providerDigestKey struct {
	digest digest.Digest
	nodeID ifaces.NodeID
	addr   string
}

type peerAttemptSummary struct {
	attempted           int
	stale               int
	unavailable         int
	digestMismatch      int
	authOrConfig        int
	peerServerError     int
	protocolError       int
	stall               int
	localError          int
	busy                int
	staleFiltered       int
	unavailableFiltered int
	suspiciousFiltered  int
	selfFiltered        int
}

func (s peerAttemptSummary) allStaleOrFiltered() bool {
	if s.attempted == 0 {
		return false
	}

	return s.unavailable == 0 &&
		s.digestMismatch == 0 &&
		s.authOrConfig == 0 &&
		s.peerServerError == 0 &&
		s.protocolError == 0 &&
		s.stall == 0 &&
		s.localError == 0 &&
		s.busy == 0
}

// tryPeerFallback attempts to satisfy a cache miss via DHT-discovered peers.
// When re-discovery is enabled (peerRediscoverBudget > 0) it repeatedly
// re-runs a discovery round so it can pick up finisher-seeds that advertise
// mid-swarm, turning a "fall to origin" cohort into a real peer cascade.
//
// Round 0 runs the full path including cold-start (please_pull), which
// designates the HRW puller; its terminal result is the authoritative
// origin-fallback decision returned if the swarm never delivers. Later rounds
// suppress cold-start (so please_pull is not re-issued every iteration) and
// exist only to catch a newly-advertised finisher. The result semantics
// (peerFallbackUnused -> direct origin, peerFallbackExhausted -> 503,
// peerFallbackColdExhausted -> NF5 gating) are preserved exactly.
func (s *Server) tryPeerFallback(ctx context.Context, w http.ResponseWriter, r *http.Request, d digest.Digest, kind ifaces.OriginRefKind, upstream, repo string, logger *slog.Logger) peerFallbackResult {
	var stream *livePeerStream
	if s.liveStreamThrough {
		stream = &livePeerStream{}
	}

	budget := s.peerRediscoverBudget
	if budget <= 0 {
		// Re-discovery disabled: a single round with cold-start allowed,
		// identical to the historical behavior.
		result := s.tryPeerFallbackRound(ctx, w, r, d, kind, upstream, repo, true, stream, logger)
		if stream != nil && stream.started && result != peerFallbackServed {
			return peerFallbackPartial
		}

		return result
	}

	backoff := s.peerRediscoverBackoff
	if backoff <= 0 {
		backoff = time.Second
	}

	deadline := time.Now().Add(budget)

	firstResult := s.tryPeerFallbackRound(ctx, w, r, d, kind, upstream, repo, true, stream, logger)
	switch firstResult {
	case peerFallbackServed, peerFallbackLocalHit:
		return firstResult
	}

	// Keep re-discovering. A seed that just finished advertises into the DHT,
	// so a later FindProviders can hand us a provider even though round 0 fell
	// through. We only look for a served/local-hit outcome here; if the swarm
	// never delivers within the budget we return round 0's authoritative
	// origin-fallback decision unchanged.
	for time.Now().Before(deadline) {
		// A concurrent request or the local puller may have populated the
		// cache since the last round.
		if (stream == nil || !stream.started) && s.serveLocalHit(ctx, w, r, d, kind, upstream, repo, logger) {
			return peerFallbackLocalHit
		}

		select {
		case <-ctx.Done():
			if stream != nil && stream.started {
				return peerFallbackPartial
			}

			return firstResult
		case <-time.After(jitteredBackoff(backoff)):
		}

		switch s.tryPeerFallbackRound(ctx, w, r, d, kind, upstream, repo, false, stream, logger) {
		case peerFallbackServed:
			return peerFallbackServed
		case peerFallbackLocalHit:
			return peerFallbackLocalHit
		}
	}

	if stream != nil && stream.started {
		return peerFallbackPartial
	}

	return firstResult
}

// jitteredBackoff returns base +/- 25% so a cohort of nodes that missed
// together does not re-discover in lockstep.
func jitteredBackoff(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}

	delta := base / 4
	if delta <= 0 {
		return base
	}

	return base - delta + time.Duration(rand.Int64N(int64(2*delta)+1))
}

// tryPeerFallbackRound runs one discovery-and-fetch pass. Returns one of
// peerFallbackResult above. In default mode, no bytes are written to w until a
// peer's body is digest-verified and committed to the local cache. In
// live-stream-through mode, a successful peer fetch streams directly to the
// caller and later inventory observation is used to confirm the local
// containerd commit. When allowColdStart is false, the cold-start
// (please_pull) legs are skipped so the re-discovery loop does not re-issue
// please_pull on every round.
func (s *Server) tryPeerFallbackRound(ctx context.Context, w http.ResponseWriter, r *http.Request, d digest.Digest, kind ifaces.OriginRefKind, upstream, repo string, allowColdStart bool, stream *livePeerStream, logger *slog.Logger) peerFallbackResult {
	// Cold-start may designate this process as the puller, populating the
	// local store after serveDigest's initial cache miss.
	recheckLocalAfterColdStart := func() bool {
		if stream != nil && stream.started {
			return false
		}

		return s.serveLocalHit(ctx, w, r, d, kind, upstream, repo, logger)
	}

	lookupBudget := s.peerLookupBudget
	if lookupBudget <= 0 {
		lookupBudget = 2 * time.Second
	}

	fetchBudget := s.peerFetchBudget
	if fetchBudget <= 0 {
		fetchBudget = time.Hour
	}

	maxAttempts := s.maxPeerAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}

	lookupCtx, cancel := context.WithTimeout(ctx, lookupBudget)
	lookupStart := time.Now()
	providers, err := s.dht.FindProviders(lookupCtx, d)
	lookupDur := time.Since(lookupStart)
	lookupCtxErr := lookupCtx.Err()

	cancel()

	switch {
	case err != nil:
		outcome := "error"
		if errors.Is(lookupCtxErr, context.DeadlineExceeded) {
			outcome = "timeout"
		}

		s.bumpDhtLookup(outcome, lookupDur)
		logger.Debug("mirror: FindProviders error", slog.Any("err", err))
	case len(providers) == 0:
		s.bumpDhtLookup("miss", lookupDur)
	default:
		s.bumpDhtLookup("hit", lookupDur)
	}

	if err != nil || len(providers) == 0 {
		if !allowColdStart {
			// Re-discovery round: cold-start is suppressed. No providers
			// this round; a finisher may advertise before the next round.
			if recheckLocalAfterColdStart() {
				return peerFallbackLocalHit
			}

			return peerFallbackUnused
		}

		csProviders, res := s.resolveViaColdStart(ctx, d, kind, upstream, repo, err != nil, false, peerAttemptSummary{}, logger)

		if recheckLocalAfterColdStart() {
			return peerFallbackLocalHit
		}

		if res != peerFallbackUnused {
			return res
		}

		if len(csProviders) == 0 {
			return peerFallbackUnused
		}

		providers = csProviders
	}

	providers, summary := s.filterProvidersForDigest(d, providers)
	if s.metrics.onStaleProviderFiltered != nil {
		totalFiltered := summary.staleFiltered + summary.unavailableFiltered + summary.suspiciousFiltered + summary.selfFiltered
		if totalFiltered > 0 {
			s.metrics.onStaleProviderFiltered(totalFiltered)
		}
	}

	if len(providers) == 0 {
		// All DHT-returned providers were filtered out before we
		// could try any of them. Distinct from "DHT returned zero"
		// (counted as dht_lookup miss above) - this is "DHT had
		// candidates but they were all dead". Falls through to
		// cold-start the same as a true miss, but operators need to
		// see the difference on the dashboard.
		if s.metrics.onDhtStaleOnly != nil {
			s.metrics.onDhtStaleOnly()
		}

		logger.Debug("mirror: DHT providers filtered",
			slog.Int("stale_filtered", summary.staleFiltered),
			slog.Int("unavailable_filtered", summary.unavailableFiltered),
			slog.Int("suspicious_filtered", summary.suspiciousFiltered),
			slog.Int("self_filtered", summary.selfFiltered),
		)

		if !allowColdStart {
			// Re-discovery round: cold-start is suppressed.
			if recheckLocalAfterColdStart() {
				return peerFallbackLocalHit
			}

			return peerFallbackUnused
		}

		csProviders, res := s.resolveViaColdStart(ctx, d, kind, upstream, repo, false, true, summary, logger)

		if recheckLocalAfterColdStart() {
			return peerFallbackLocalHit
		}

		if res != peerFallbackUnused {
			return res
		}

		if len(csProviders) == 0 {
			return peerFallbackUnused
		}

		filtered, _ := s.filterProvidersForDigest(d, csProviders)
		providers = filtered
	}

	tried := 0
	for _, p := range providers {
		if tried >= maxAttempts {
			break
		}

		tried++
		summary.attempted++
		res := s.fetchOneProvider(ctx, w, r, d, kind, upstream, repo, p, fetchBudget, stream, logger)

		summary = updatePeerSummary(summary, res.outcome)
		if res.served {
			return peerFallbackServed
		}
	}

	if stream != nil && stream.started {
		return peerFallbackExhausted
	}

	if tried > 0 && allowColdStart {
		allStale := summary.allStaleOrFiltered()
		logger.Debug("mirror: peer providers exhausted, consulting cold-start",
			slog.Int("attempted", summary.attempted),
			slog.Int("stale", summary.stale),
			slog.Int("unavailable", summary.unavailable),
			slog.Int("digest_mismatch", summary.digestMismatch),
			slog.Int("auth_or_config", summary.authOrConfig),
			slog.Int("peer_server_error", summary.peerServerError),
			slog.Int("protocol_error", summary.protocolError),
			slog.Int("stall", summary.stall),
			slog.Int("local_error", summary.localError),
			slog.Bool("all_stale_or_filtered", allStale),
		)

		csProviders, csResult := s.resolveViaColdStart(ctx, d, kind, upstream, repo, false, allStale, summary, logger)

		if recheckLocalAfterColdStart() {
			return peerFallbackLocalHit
		}

		if csResult != peerFallbackUnused {
			return csResult
		}

		filteredCS, _ := s.filterProvidersForDigest(d, csProviders)

		for _, p := range filteredCS {
			if tried >= maxAttempts*2 {
				break
			}

			tried++
			summary.attempted++
			res := s.fetchOneProvider(ctx, w, r, d, kind, upstream, repo, p, fetchBudget, stream, logger)

			summary = updatePeerSummary(summary, res.outcome)
			if res.served {
				return peerFallbackServed
			}
		}
	}

	return peerFallbackExhausted
}

// fetchOneProvider streams one peer candidate. In default mode it writes
// into the local cache first (digest verifies on Commit) and then serves
// from cache. In live-stream-through mode it proxies directly to the
// caller and performs the final digest check after the streamed body ends;
// containerd is still responsible for rejecting any bad bytes it received.
func (s *Server) fetchOneProvider(ctx context.Context, w http.ResponseWriter, r *http.Request, d digest.Digest, kind ifaces.OriginRefKind, upstream, repo string, p ifaces.Provider, fetchBudget time.Duration, stream *livePeerStream, logger *slog.Logger) peerAttemptResult {
	pCtx, cancel := context.WithTimeout(ctx, fetchBudget)
	defer cancel()

	fetchStart := time.Now()

	pRef := ifaces.OriginRef{Registry: upstream, Repository: repo, Digest: d, Kind: kind}
	if stream != nil {
		pRef.Offset = stream.offset()
	}

	rc, psize, err := s.peer.FetchFromPeer(pCtx, p.Addr, pRef)
	if err != nil {
		outcome, label := classifyPeerFetchError(err)

		switch outcome {
		case peerFetchOutcomeBusy:
			// The peer answered 429: it is alive (dial succeeded) but at
			// its serve cap. Do not quarantine it - it will serve once its
			// load drops, and the re-discovery loop retries it or a
			// finisher on the next round.
			s.bumpPeerDial(true)
		case peerFetchOutcomeStaleProvider:
			s.bumpPeerDial(true)
			s.markProviderStale(d, p)
		default:
			s.bumpPeerDial(false)

			if outcome == peerFetchOutcomeUnavailable {
				s.markProviderUnavailable(p)
			}

			if outcome == peerFetchOutcomeDigestMismatch {
				s.markProviderSuspicious(d, p)
			}
		}

		s.bumpPeerFetch(label)
		s.bumpPeerFetchLatency(label, fetchStart)
		logger.Debug("mirror: peer fetch failed",
			slog.String("peer", p.Addr),
			slog.String("node", string(p.NodeID)),
			slog.String("outcome", label),
			slog.Any("err", err),
		)

		return peerAttemptResult{outcome: outcome}
	}

	defer func() { _ = rc.Close() }() //nolint:errcheck // best-effort close

	s.bumpPeerDial(true)

	if s.liveStreamThrough {
		written, complete, streamErr := stream.append(w, rc, d, psize, kind)
		s.fireMirrorBytesServed(kind, "peer", written)

		if streamErr != nil {
			if isDigestMismatchErr(streamErr) {
				s.markProviderSuspicious(d, p)
				s.bumpPeerFetch("digest_mismatch")
				s.bumpPeerFetchLatency("digest_mismatch", fetchStart)
				logger.Warn("mirror: peer digest mismatch after live stream-through",
					slog.String("peer", p.Addr),
					slog.Int64("written", written),
					slog.Any("err", streamErr),
				)

				return peerAttemptResult{outcome: peerFetchOutcomeDigestMismatch, served: true}
			}

			s.bumpPeerFetch("stall")
			s.bumpPeerFetchLatency("stall", fetchStart)
			logger.Debug("mirror: peer live stream-through failed",
				slog.String("peer", p.Addr),
				slog.Int64("resume_offset", stream.offset()),
				slog.Int64("written", written),
				slog.Any("err", streamErr),
			)

			return peerAttemptResult{outcome: peerFetchOutcomeStall, served: ctx.Err() != nil}
		}

		if !complete {
			return peerAttemptResult{outcome: peerFetchOutcomeStall}
		}

		s.bumpPeerFetch("hit")
		s.bumpPeerFetchLatency("hit", fetchStart)
		s.fireMirrorResponseCompleted(d, kind, "peer")
		s.fireLiveStreamCompleted(d)

		return peerAttemptResult{outcome: peerFetchOutcomeHit, served: true}
	}

	cw, cwerr := s.store.Writer(pCtx, d)
	if cwerr != nil {
		s.bumpPeerFetch("local_error")
		s.bumpPeerFetchLatency("local_error", fetchStart)
		logger.Warn("mirror: cache writer unavailable for peer fetch", slog.Any("err", cwerr))

		return peerAttemptResult{outcome: peerFetchOutcomeLocalError}
	}

	defer func() { _ = cw.Abort(pCtx) }() //nolint:errcheck // best-effort abort

	_, err = io.Copy(cw, rc)
	if err != nil {
		s.bumpPeerFetch("stall")
		s.bumpPeerFetchLatency("stall", fetchStart)
		logger.Debug("mirror: peer copy stalled",
			slog.String("peer", p.Addr),
			slog.Any("err", err),
		)

		return peerAttemptResult{outcome: peerFetchOutcomeStall}
	}

	if err := cw.Commit(pCtx); err != nil {
		if isDigestMismatchErr(err) {
			s.markProviderSuspicious(d, p)
			s.bumpPeerFetch("digest_mismatch")
			s.bumpPeerFetchLatency("digest_mismatch", fetchStart)
			logger.Warn("mirror: peer digest mismatch",
				slog.String("peer", p.Addr),
				slog.Any("err", err),
			)

			return peerAttemptResult{outcome: peerFetchOutcomeDigestMismatch}
		}

		s.bumpPeerFetch("local_error")
		s.bumpPeerFetchLatency("local_error", fetchStart)
		logger.Warn("mirror: peer commit failed",
			slog.String("peer", p.Addr),
			slog.Any("err", err),
		)

		return peerAttemptResult{outcome: peerFetchOutcomeLocalError}
	}

	// Re-advertise this digest into the DHT now that we've cached a
	// byte-identical copy. Without this, peer-fetched blobs were
	// discoverable only via the source peer's announcements, so the
	// provider set never grew - defeating the deduplication promise
	// of the design (detailed-design the step 7). Fire-and-forget
	// with a 30s budget; bg ctx so client cancellation can't abort
	// the announcement.
	s.reAdvertiseDigest(d, "peer_fetch_readvertise", logger)

	// Re-open from cache and stream verified bytes to the client.
	rcLocal, size, err := s.store.Open(ctx, d)
	if err != nil {
		s.bumpPeerFetch("local_error")
		s.bumpPeerFetchLatency("local_error", fetchStart)
		logger.Warn("mirror: post-commit cache open failed", slog.Any("err", err))

		return peerAttemptResult{outcome: peerFetchOutcomeLocalError}
	}

	defer func() { _ = rcLocal.Close() }() //nolint:errcheck // best-effort close

	s.bumpPeerFetch("hit")
	s.bumpPeerFetchLatency("hit", fetchStart)
	// Sniff the cached body's prefix so writeBlobHeaders can label
	// content with the right Content-Type for blobs that hold manifest
	// bytes AND for manifests that hold a manifest list/index body
	// (see writeBlobHeadersWithPrefix for the rationale).
	rcLocalBuf := bufio.NewReader(rcLocal)

	var sniff []byte

	if kind == ifaces.KindBlob || kind == ifaces.KindManifest {
		if peek, _ := rcLocalBuf.Peek(512); len(peek) > 0 { //nolint:errcheck // peek best-effort for logging
			sniff = peek
		}
	}

	writeBlobHeadersWithPrefix(w, d, size, kind, sniff)

	if r.Method == http.MethodHead {
		return peerAttemptResult{outcome: peerFetchOutcomeHit, served: true}
	}

	written, err := io.Copy(w, rcLocalBuf)
	s.fireMirrorBytesServed(kind, "peer", written)

	if err != nil {
		logger.Debug("mirror: copy from cache (post-peer) failed", slog.Any("err", err))
	}

	return peerAttemptResult{outcome: peerFetchOutcomeHit, served: true}
}

func (s *Server) resolveViaColdStart(ctx context.Context, d digest.Digest, kind ifaces.OriginRefKind, upstream, repo string, afterDHTErr, staleOnly bool, summary peerAttemptSummary, logger *slog.Logger) ([]ifaces.Provider, peerFallbackResult) {
	if s.coldStart == nil {
		return nil, peerFallbackUnused
	}

	res, csErr := s.coldStart.Resolve(ctx, d, kind, upstream, repo, 0)
	if csErr != nil {
		logger.Debug("mirror: cold-start exhausted",
			slog.Bool("after_dht_error", afterDHTErr),
			slog.Bool("stale_only", staleOnly),
			slog.Int("attempted", summary.attempted),
			slog.Any("err", csErr),
		)

		if errors.Is(csErr, ErrColdStartExhausted) {
			return nil, peerFallbackColdExhausted
		}

		return nil, peerFallbackExhausted
	}

	return res.Providers, peerFallbackUnused
}

func (s *Server) filterProvidersForDigest(d digest.Digest, providers []ifaces.Provider) ([]ifaces.Provider, peerAttemptSummary) {
	now := time.Now()
	s.sweepProviderFailures(now)

	filtered := make([]ifaces.Provider, 0, len(providers))

	var summary peerAttemptSummary

	for _, p := range providers {
		if s.isSelfProvider(p) {
			summary.selfFiltered++
			continue
		}

		key := providerDigestKey{digest: d, nodeID: p.NodeID, addr: p.Addr}
		if s.isProviderInWindow(s.staleProviders, key, now) {
			summary.staleFiltered++
			continue
		}

		if s.isProviderInWindow(s.suspiciousProviders, key, now) {
			summary.suspiciousFiltered++
			continue
		}

		if s.isUnavailablePeerInWindow(p.Addr, now) {
			summary.unavailableFiltered++
			continue
		}

		filtered = append(filtered, p)
	}

	// Shuffle so each requester tries the surviving providers in a random
	// order. Without this, every node walks the DHT/cold-start provider list
	// in the same order and piles onto whichever seed sorts first, re-creating
	// a single-seed hotspot even when the layer is seeded on N pullers
	// (prefetch_puller_replicas) and finishers have joined the provider set.
	// Randomizing spreads the maxPeerAttempts fetches across the available
	// seeds, which is what turns the N initial seeds into an even fan-out.
	rand.Shuffle(len(filtered), func(i, j int) {
		filtered[i], filtered[j] = filtered[j], filtered[i]
	})

	return filtered, summary
}

func (s *Server) isSelfProvider(p ifaces.Provider) bool {
	return (s.selfNodeID != "" && p.NodeID == s.selfNodeID) ||
		(s.selfPeerID != "" && p.NodeID == s.selfPeerID)
}

func updatePeerSummary(summary peerAttemptSummary, outcome peerFetchOutcomeKind) peerAttemptSummary {
	switch outcome {
	case peerFetchOutcomeStaleProvider:
		summary.stale++
	case peerFetchOutcomeUnavailable:
		summary.unavailable++
	case peerFetchOutcomeDigestMismatch:
		summary.digestMismatch++
	case peerFetchOutcomeAuthOrConfigError:
		summary.authOrConfig++
	case peerFetchOutcomePeerServerError:
		summary.peerServerError++
	case peerFetchOutcomeProtocolError:
		summary.protocolError++
	case peerFetchOutcomeStall:
		summary.stall++
	case peerFetchOutcomeLocalError:
		summary.localError++
	case peerFetchOutcomeBusy:
		summary.busy++
	}

	return summary
}

func classifyPeerFetchError(err error) (peerFetchOutcomeKind, string) {
	var enf *ifaces.ErrNotFound
	if errors.As(err, &enf) {
		return peerFetchOutcomeStaleProvider, "notfound"
	}

	var statusErr *ifaces.ErrPeerHTTPStatus
	if errors.As(err, &statusErr) {
		switch {
		case statusErr.StatusCode == http.StatusTooManyRequests:
			return peerFetchOutcomeBusy, "busy"
		case statusErr.StatusCode == http.StatusUnauthorized || statusErr.StatusCode == http.StatusForbidden:
			return peerFetchOutcomeAuthOrConfigError, "auth_or_config"
		case statusErr.StatusCode >= 500 && statusErr.StatusCode <= 599:
			return peerFetchOutcomePeerServerError, "server_error"
		default:
			return peerFetchOutcomeProtocolError, "protocol_error"
		}
	}

	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || isDialUnavailable(err) {
		return peerFetchOutcomeUnavailable, "unavailable"
	}

	return peerFetchOutcomeProtocolError, "protocol_error"
}

func isDialUnavailable(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}

	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH) {
		return true
	}

	errText := strings.ToLower(err.Error())

	return strings.Contains(errText, "connection refused") || strings.Contains(errText, "no route to host")
}

// isDigestMismatchErr reports whether err signals that a peer served
// bytes that failed content verification. Two typed sources are
// recognized, one per fetch path:
//
//   - digestpipe.ErrDigestMismatch from the live stream-through verifier
//     (streamDigestToClient).
//   - errdefs.ErrFailedPrecondition from a containerd content-store
//     Commit, which wraps it for both "unexpected commit digest" and
//     "unexpected commit size" - either way the peer's bytes did not
//     match what we asked for.
//
// Substring matching on the message is deliberately avoided: the real
// containerd commit error says "unexpected commit digest", not "digest
// mismatch", so the old text match silently misclassified a corrupt
// peer as a generic local error and never quarantined it.
func isDigestMismatchErr(err error) bool {
	return errors.Is(err, digestpipe.ErrDigestMismatch) ||
		errors.Is(err, errdefs.ErrFailedPrecondition)
}

func (s *Server) markProviderStale(d digest.Digest, p ifaces.Provider) {
	if s.staleProviderTTL <= 0 {
		return
	}

	s.providerFailureMu.Lock()
	defer s.providerFailureMu.Unlock()

	now := time.Now()
	s.sweepProviderFailuresLocked(now)
	s.staleProviders[providerDigestKey{digest: d, nodeID: p.NodeID, addr: p.Addr}] = now.Add(s.staleProviderTTL)
}

func (s *Server) markProviderSuspicious(d digest.Digest, p ifaces.Provider) {
	if s.suspiciousPeerTTL <= 0 {
		return
	}

	s.providerFailureMu.Lock()
	defer s.providerFailureMu.Unlock()

	now := time.Now()
	s.sweepProviderFailuresLocked(now)
	s.suspiciousProviders[providerDigestKey{digest: d, nodeID: p.NodeID, addr: p.Addr}] = now.Add(s.suspiciousPeerTTL)
}

func (s *Server) markProviderUnavailable(p ifaces.Provider) {
	if s.unavailablePeerTTL <= 0 {
		return
	}

	s.providerFailureMu.Lock()
	defer s.providerFailureMu.Unlock()

	now := time.Now()
	s.sweepProviderFailuresLocked(now)
	s.unavailableProviders[p.Addr] = now.Add(s.unavailablePeerTTL)
}

func (s *Server) sweepProviderFailures(now time.Time) {
	s.providerFailureMu.Lock()
	defer s.providerFailureMu.Unlock()

	s.sweepProviderFailuresLocked(now)
}

func (s *Server) sweepProviderFailuresLocked(now time.Time) {
	if !s.nextProviderFailureSweep.IsZero() && now.Before(s.nextProviderFailureSweep) {
		return
	}

	s.nextProviderFailureSweep = now.Add(providerFailureSweepInterval)
	s.sweepProviderDigestMapLocked(s.staleProviders, now)
	s.sweepProviderDigestMapLocked(s.suspiciousProviders, now)

	for addr, until := range s.unavailableProviders {
		if !now.After(until) {
			continue
		}

		delete(s.unavailableProviders, addr)
	}
}

func (s *Server) sweepProviderDigestMapLocked(m map[providerDigestKey]time.Time, now time.Time) {
	for key, until := range m {
		if !now.After(until) {
			continue
		}

		delete(m, key)
	}
}

func (s *Server) isProviderInWindow(m map[providerDigestKey]time.Time, key providerDigestKey, now time.Time) bool {
	s.providerFailureMu.Lock()
	defer s.providerFailureMu.Unlock()

	until, ok := m[key]
	if !ok {
		return false
	}

	if now.After(until) {
		delete(m, key)
		return false
	}

	return true
}

func (s *Server) isUnavailablePeerInWindow(addr string, now time.Time) bool {
	s.providerFailureMu.Lock()
	defer s.providerFailureMu.Unlock()

	until, ok := s.unavailableProviders[addr]
	if !ok {
		return false
	}

	if now.After(until) {
		delete(s.unavailableProviders, addr)
		return false
	}

	return true
}

// firePrefetch invokes the LayerPrefetcher (if any) in a goroutine
// when kind is a manifest. The mirror does NOT wait for the
// callback; the prefetcher's job is to read the manifest body from
// cache and dispatch batched please_pull RPCs entirely in the
// background.
func (s *Server) firePrefetch(ctx context.Context, kind ifaces.OriginRefKind, registry, repository string, d digest.Digest) {
	if s.prefetcher == nil || kind != ifaces.KindManifest {
		return
	}

	// The callback outlives the HTTP request, so detach cancellation while
	// retaining only the delegated registry credential.
	go s.prefetcher.OnManifestServed(registryauth.Detach(ctx), registry, repository, d)
}

// reAdvertiseDigest does a fire-and-forget dht.Provide(d) in a
// goroutine with a 30s budget. This helper is retained for the legacy
// verify-before-serving mirror mode used by tests; production wiring
// enables WithLiveStreamThrough, where live mirror requests never call
// this path and advertisement is owned by the advertiser after
// containerd commit observation. The op label distinguishes the call
// site for the p2p_dht_provide_error_total{op} counter. The background
// context shields the announce from client cancellation.
func (s *Server) reAdvertiseDigest(d digest.Digest, op string, logger *slog.Logger) {
	if s.dht == nil {
		return
	}

	dHash := d

	go func() {
		provCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if perr := s.dht.Provide(provCtx, dHash); perr != nil {
			if s.metrics.onProvideError != nil {
				s.metrics.onProvideError(op)
			}

			logger.Debug("mirror: post-commit dht.Provide failed",
				slog.String("op", op),
				slog.String("digest", dHash.String()),
				slog.Any("err", perr),
			)
		}
	}()
}

// bumpCacheHit / bumpCacheMiss are no-ops if no metric hooks were
// registered. There is intentionally no bumpOriginPull,
// bumpOriginFailure, bumpOriginSuccess, or bumpOriginDownstreamFailure
// helper here. The origin pull-family counters are split deliberately
// across two packages (see the WithMetrics, WithOriginSuccessMetric,
// and WithDownstreamFailureMetric doc comments):
//
// - p2p_origin_pull_total{kind} : bumped by origin.Client
// at Pull entry (via origin.WithMetrics' onPullStart hook).
// origin.Client.Head deliberately does NOT bump this counter.
// - p2p_origin_pull_failure_total{kind,class} : bumped from two
// places - origin.Client.recordFailure (true origin-side
// failures: non-2xx, network errors) AND the mirror's
// fireOriginDownstreamFailure (downstream failures after
// origin returned 2xx: io.Copy stall, cw.Commit, directVerifier).
// - p2p_origin_failure_total{class} : bumped ONLY by
// origin.Client.recordFailure (true origin-side failures).
// Reserved for the "is the origin sick?" operator alert; the
// mirror's downstream-failure hook does NOT bump it.
// - p2p_origin_pull_success_total{kind} : bumped by the mirror
// (fireOriginSuccess) after cw.Commit succeeds or the direct-
// stream digest verifier passes, AND by the puller pump's
// runOriginPull after that path's cw.Commit. Origin cannot
// emit success because origin has no way to know whether the
// caller actually committed bytes.
//
// Together this layout preserves the per-pull arithmetic identity:
//
//	started == success + (origin_failure + downstream_failure) + in_flight
//
// while keeping the operator-facing "origin sick" alert
// (p2p_origin_failure_total) free of false positives from local
// cache I/O errors or origin truncations.
func (s *Server) bumpCacheHit() {
	if s.metrics.onCacheHit != nil {
		s.metrics.onCacheHit()
	}
}

func (s *Server) bumpCacheMiss() {
	if s.metrics.onCacheMiss != nil {
		s.metrics.onCacheMiss()
	}
}

func (s *Server) fireMirrorBytesServed(kind ifaces.OriginRefKind, source string, bytes int64) {
	if bytes <= 0 || s.metrics.onMirrorBytesServed == nil {
		return
	}

	s.metrics.onMirrorBytesServed(kind.MetricLabel(), source, bytes)
}

func (s *Server) fireMirrorResponseCompleted(d digest.Digest, kind ifaces.OriginRefKind, source string) {
	if s.metrics.onMirrorResponseCompleted == nil {
		return
	}

	s.metrics.onMirrorResponseCompleted(d, kind.MetricLabel(), source)
}

func (s *Server) fireOriginStreamStarted(kind ifaces.OriginRefKind) {
	if s.metrics.onOriginStreamStarted == nil {
		return
	}

	s.metrics.onOriginStreamStarted(kind.MetricLabel())
}

func (s *Server) fireOriginStreamCompleted(kind ifaces.OriginRefKind) {
	if s.metrics.onOriginStreamCompleted == nil {
		return
	}

	s.metrics.onOriginStreamCompleted(kind.MetricLabel())
}

func (s *Server) fireOriginStreamFailed(kind ifaces.OriginRefKind) {
	if s.metrics.onOriginStreamFailed == nil {
		return
	}

	s.metrics.onOriginStreamFailed(kind.MetricLabel())
}

func (s *Server) fireLiveStreamCompleted(d digest.Digest) {
	if s.metrics.onLiveStreamCompleted == nil {
		return
	}

	s.metrics.onLiveStreamCompleted(d)
}

// fireOriginSuccess emits p2p_origin_pull_success_total via the hook
// registered with WithOriginSuccessMetric. Call sites must invoke
// this AFTER the response body has been streamed AND the cluster has
// produced a useful artifact from it (cache commit OK, or
// direct-stream digest verifier passed when cache is unavailable).
// Calling it earlier - e.g. inside a deferred Close on the origin
// reader - inflates the success counter against HEAD requests, io.Copy
// interruptions, and cache-commit failures (the exact bug the
// a prior review flagged as "false positives on the success metric").
func (s *Server) fireOriginSuccess(kind ifaces.OriginRefKind, bytes int64) {
	if s.metrics.onOriginSuccess == nil {
		return
	}

	s.metrics.onOriginSuccess(kind.MetricLabel(), bytes)
}

// fireOriginDownstreamFailure emits the per-(kind,class)
// p2p_origin_pull_failure_total via the hook registered with
// WithDownstreamFailureMetric. Call sites must invoke this on
// terminal failures of the downstream pipeline (io.Copy / cw.Commit
// / directVerifier.Verify) AFTER origin returned 2xx. Origin-side
// failures (origin.Pull returned an *ifaces.OriginError) are
// counted separately by origin.WithMetrics' failure closure and
// must NOT also fire this hook - see WithDownstreamFailureMetric's
// doc for the cleanup of the two counters.
func (s *Server) fireOriginDownstreamFailure(kind ifaces.OriginRefKind, class ifaces.FailureClass) {
	if s.metrics.onOriginDownstreamFailure == nil {
		return
	}

	s.metrics.onOriginDownstreamFailure(kind.MetricLabel(), string(class))
}

// classifyOriginFailureClass extracts the FailureClass from an
// origin-side error. Mirrors the same classification the puller-pump
// path's recordOriginFailure (cmd/gantry/main.go) uses: an
// *ifaces.OriginError carries a class, anything else (cache writer
// open errors, copy stalls, commit digest mismatches) maps to
// FailureTransient - not the origin's fault, but treating them as
// transient blocks the cluster from re-hammering the same puller on
// a flapping local disk or a content-injection proxy while still
// self-healing on the next cooldown elapse.
func classifyOriginFailureClass(err error) ifaces.FailureClass {
	var oe *ifaces.OriginError
	if errors.As(err, &oe) && oe.Class != ifaces.FailureUnspecified {
		return oe.Class
	}

	return ifaces.FailureTransient
}

// recordNegCacheFailure routes a terminal direct-origin failure into
// the optional the design doc negative-cache recorder. Nil-safe: leaves the
// pre-behaviour untouched when no recorder is
// wired. Symmetric with the puller-pump path's recordOriginFailure
// (cmd/gantry/main.go) which seeds the same cache for the
// please_pull-coordinated path.
func (s *Server) recordNegCacheFailure(d digest.Digest, err error) {
	if s.negCache == nil {
		return
	}

	s.negCache.RecordFailure(d, classifyOriginFailureClass(err))
}

// recordNegCacheSuccess clears any prior the design doc cooldown entry for d
// when the mirror's direct-origin path produces a committed (or
// direct-stream-verified) artifact. Nil-safe; symmetric with the
// puller-pump path's neg.RecordSuccess(d) call after cw.Commit.
func (s *Server) recordNegCacheSuccess(d digest.Digest) {
	if s.negCache == nil {
		return
	}

	s.negCache.RecordSuccess(d)
}

func streamDigestToClient(w http.ResponseWriter, src io.Reader, d digest.Digest, size int64, kind ifaces.OriginRefKind) (int64, error) {
	br := bufio.NewReader(src)

	var sniff []byte
	// Peek the first bytes for both blobs (which may carry manifest
	// bytes via the origin /blobs/->/manifests/ fallback) AND manifests
	// (which may carry a manifest LIST/index body that needs the
	// matching Content-Type - see writeBlobHeadersWithPrefix for the
	// "expected manifest but found index" failure mode this prevents).
	if kind == ifaces.KindBlob || kind == ifaces.KindManifest {
		if peek, _ := br.Peek(512); len(peek) > 0 { //nolint:errcheck // peek best-effort for logging
			sniff = peek
		}
	}

	writeBlobHeadersWithPrefix(w, d, size, kind, sniff)
	verifier := digestpipe.New(w)

	written, err := io.Copy(verifier, br)
	if err != nil {
		return written, err
	}

	if err := verifier.Verify(d); err != nil {
		return written, err
	}

	return written, nil
}

func (s *Server) bumpPeerFetch(outcome string) {
	if s.metrics.onPeerFetch != nil {
		s.metrics.onPeerFetch(outcome)
	}
}

// bumpPeerFetchLatency emits the peer-fetch duration observation with
// the terminal outcome label. Always paired with bumpPeerFetch; the
// two together describe one fetchOneProvider call.
func (s *Server) bumpPeerFetchLatency(outcome string, start time.Time) {
	if s.metrics.onPeerFetchLatency != nil {
		s.metrics.onPeerFetchLatency(outcome, time.Since(start))
	}
}

func (s *Server) bumpPeerDial(success bool) {
	if s.metrics.onPeerDialResult != nil {
		s.metrics.onPeerDialResult(success)
	}
}

func (s *Server) bumpDhtLookup(outcome string, dur time.Duration) {
	if s.metrics.onDhtLookup != nil {
		s.metrics.onDhtLookup(outcome, dur)
	}
}

// writeBlobHeaders sets the OCI distribution headers the client expects.
//
// When kind == KindBlob, sniffPrefix may carry the first bytes of the
// body so the function can detect manifest JSON (produced by origin's
// /blobs/->/manifests/ fallback or by a peer that happened to cache a
// manifest digest) and set the matching Content-Type. Containerd's CRI
// plugin rejects manifest bytes returned with Content-Type:
// application/octet-stream as "Target.MediaType must be set".
func writeBlobHeaders(w http.ResponseWriter, d digest.Digest, size int64, kind ifaces.OriginRefKind) {
	writeBlobHeadersWithPrefix(w, d, size, kind, nil)
}

func writeBlobHeadersWithPrefix(w http.ResponseWriter, d digest.Digest, size int64, kind ifaces.OriginRefKind, sniffPrefix []byte) {
	w.Header().Set("Docker-Content-Digest", d.String())

	if w.Header().Get("Content-Type") == "" {
		switch kind {
		case ifaces.KindManifest:
			// containerd's CRI plugin uses the HEAD /manifests/<digest>
			// response Content-Type to populate the in-memory image's
			// Target.MediaType. With an empty header it later rejects
			// the unpack with "Target.MediaType must be set: invalid
			// argument" - even though the manifest body carries its
			// own schemaVersion + mediaType.
			//
			// Critically, containerd ALSO uses this Content-Type to
			// decide whether the resolved descriptor is a single
			// manifest or a manifest list/index. If we serve a
			// manifest-list body but label it with the single-manifest
			// media type, containerd later fails the unpack with
			// "expected manifest but found index". So we sniff the
			// body prefix first to pick the matching content type;
			// only when sniffing yields nothing do we fall back to the
			// safe OCI manifest default.
			if ct := sniffManifestContentType(sniffPrefix); ct != "" {
				w.Header().Set("Content-Type", ct)
			} else {
				w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			}
		case ifaces.KindBlob:
			// /blobs/<digest> can carry either real layer/config bytes
			// (octet-stream) OR a manifest body served via origin's
			// /blobs/->/manifests/ fallback. Sniff the prefix to tell
			// the difference; if it looks like a manifest envelope use
			// the matching manifest content type, otherwise the
			// distribution-spec default.
			if ct := sniffManifestContentType(sniffPrefix); ct != "" {
				w.Header().Set("Content-Type", ct)
			} else {
				w.Header().Set("Content-Type", "application/octet-stream")
			}
		}
	}

	if size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
}

// sniffManifestContentType returns an OCI/Docker manifest Content-Type
// when the prefix bytes look like a manifest JSON envelope, otherwise
// an empty string. Used by writeBlobHeadersWithPrefix to label content
// retrieved via origin's /blobs/->/manifests/ fallback so containerd's
// CRI plugin can unpack it. We inspect mediaType because the same
// schemaVersion=2 envelope is used by both image-manifest, image-index,
// docker-manifest, and docker-manifest-list and the client needs the
// right one to dispatch unpacking.
func sniffManifestContentType(prefix []byte) string {
	if len(prefix) < 2 || prefix[0] != '{' {
		return ""
	}
	// Try to find a mediaType field. We don't fully parse JSON here
	// because the prefix may not contain a complete value; a substring
	// match against the well-known media types is sufficient for the
	// types Gantry ever sees on the /blobs/ fallback path.
	switch {
	case bytes.Contains(prefix, []byte("application/vnd.oci.image.index.v1+json")):
		return "application/vnd.oci.image.index.v1+json"
	case bytes.Contains(prefix, []byte("application/vnd.oci.image.manifest.v1+json")):
		return "application/vnd.oci.image.manifest.v1+json"
	case bytes.Contains(prefix, []byte("application/vnd.docker.distribution.manifest.list.v2+json")):
		return "application/vnd.docker.distribution.manifest.list.v2+json"
	case bytes.Contains(prefix, []byte("application/vnd.docker.distribution.manifest.v2+json")):
		return "application/vnd.docker.distribution.manifest.v2+json"
	}
	// Schema-version-2 envelope without a recognisable mediaType: use
	// the OCI manifest content type as a safe default (containerd's
	// unpacker will pick the right schema from the envelope itself).
	if bytes.Contains(prefix, []byte("\"schemaVersion\"")) || bytes.Contains(prefix, []byte("\"schemaVersion\":")) {
		return "application/vnd.oci.image.manifest.v1+json"
	}

	return ""
}

// writeOriginError maps an *ifaces.OriginError to an HTTP status code that
// matches what containerd expects from an OCI Distribution endpoint.
//
// mapping (refined by the design doc in):
//
//	FailureAuth 401
//	FailureNotFound 404
//	FailureRateLimited 429
//	FailureTransient 503 (← lets hosts.toml fall through to origin)
func writeOriginError(w http.ResponseWriter, err error, logger *slog.Logger) {
	var oe *ifaces.OriginError
	if !errors.As(err, &oe) {
		logger.Warn("mirror: non-classified origin error", slog.Any("err", err))
		http.Error(w, "origin error", http.StatusBadGateway)

		return
	}

	switch oe.Class {
	case ifaces.FailureAuth:
		if oe.Challenge != "" {
			w.Header().Set("WWW-Authenticate", oe.Challenge)
		}

		http.Error(w, "unauthorized", http.StatusUnauthorized)
	case ifaces.FailureNotFound:
		http.Error(w, "not found", http.StatusNotFound)
	case ifaces.FailureRateLimited:
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	default:
		http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
	}
}

func isDigestRef(ref string) bool { return strings.HasPrefix(ref, "sha256:") }

// ListenAndServe runs the mirror on the configured loopback address. The
// returned function stops the server gracefully.
func (s *Server) ListenAndServe(addr string) (func(context.Context) error, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("mirror: listen %s: %w", addr, err)
	}

	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Default().Error("mirror: serve error", slog.Any("err", err))
		}
	}()

	return func(ctx context.Context) error {
		return srv.Shutdown(ctx)
	}, nil
}
