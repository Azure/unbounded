// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package coldstart implements the / the design doc rule cascade that
// decides how an agent resolves a digest when its local cache misses
// and the DHT lookup did not return enough providers.
//
// The orchestrator is invoked by the mirror miss path after a
// FindProviders call returns empty. It runs the following pipeline:
//
// 1. Compute HRW top-K from the local membership snapshot.
// 2. Dial all K in parallel with `pull_intent_query`, collecting
// responses up to a 2 s timeout (the step 4).
// 3. Apply the 7-rule cascade in priority order (the step 5):
// 1. failure short-circuit -> return ErrFailureShortCircuit (5xx)
// 2. cache hit -> return the responder's transfer addr
// 3. in-flight piggyback -> DHT-poll until provider appears
// 4. transient cooldown -> return ErrCooldownActive (5xx)
// 5. all-unreachable expand -> re-run step 2 with top-2K
// 6. degraded eager expand -> re-run step 2 with top-2K
// 7. cold-start -> please_pull to lowest-rank reachable,
// then DHT-poll for the provider
// 4. While DHT-polling (rules 3 and 7), bound by the per-digest
// timeout from the design doc (manifest/config 5 s; layer
// max(10 s, size/50 MB/s) × 3) and the per-kind poll interval
// (200 ms manifest/config; 1 s layer).
//
// The Resolver is stateless across calls; concurrent Resolve invocations
// for the same digest are safe and will independently arrive at the
// same outcome (the inflight map at the puller side dedupes the
// origin pull itself).
package coldstart

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/hrw"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/inflight"
)

// Sentinel errors. Mirror layer maps all of these to 5xx; tests
// distinguish to validate which rule fired.
var (
	// ErrFailureShortCircuit fires rule 1.
	ErrFailureShortCircuit = errors.New("coldstart: failure short-circuit")
	// ErrCooldownActive fires rule 4.
	ErrCooldownActive = errors.New("coldstart: transient cooldown active")
	// ErrExhausted fires when the cascade reaches its terminal state
	// without producing a provider (e.g., expanded-2K exhausted, or
	// please_pull completed but the DHT poll timed out).
	ErrExhausted = errors.New("coldstart: cascade exhausted")
)

const defaultHonorSweepInterval = time.Minute

// Discovery is the subset of the libp2p discovery host that the
// orchestrator needs. Kept narrow for ease of mocking.
type Discovery interface {
	FindProviders(ctx context.Context, d digest.Digest) ([]ifaces.Provider, error)
	Health() float64
}

// MetricsHooks lets the metrics package wire Prometheus counters
// without coupling the orchestrator to client_golang. All hooks are
// nil-safe.
type MetricsHooks struct {
	// OnRankMismatch fires once per pull_intent response whose
	// reported hrw_rank disagrees with the requester's computed
	// rank for that responder. kindLabel is "manifest", "config",
	// or "layer".
	OnRankMismatch func(kindLabel string, responder ifaces.NodeID)
	// OnDhtFalseEmpty fires when the orchestrator observes the
	// false-empty case: DHT had returned 0 providers, but a
	// pull_intent_query reports has_cached=true.
	OnDhtFalseEmpty func()
	// OnTopKProbeHit fires when any rule before rule 7 (cold-start)
	// resolves the request. Used to track how often the probe saves
	// an origin pull.
	OnTopKProbeHit func()
	// OnColdStartDuration is called once per Resolve with the total
	// elapsed time and the outcome rule that fired ("rule1".."rule7"
	// or "expanded_rule_N").
	OnColdStartDuration func(kindLabel, outcome string, d time.Duration)
	// OnDesignatedPullerTakeover fires when a pull_intent_query
	// responder reports in_flight=true but its started_at is older
	// than the per-the design doc stall threshold, so the requester excludes
	// it from rule-3 piggyback and routes via the next-ranked node
	// (rule 6 / rule 7). kindLabel is "manifest", "config", or
	// "layer". Maps to the design doc metric
	// `p2p_designated_puller_takeover_total`.
	OnDesignatedPullerTakeover func(kindLabel string)
	// OnTopKExpansion fires once per expansion pass to top-2K (or
	// top-(K × TopKExpansionFactor) when the factor is configured).
	// reason is "degraded" (rule-6 DHT-degraded expand) or
	// "all_unreachable" (rule-5 expansion). Maps to the design doc metric
	// `p2p_topk_expansion_total{reason=}`.
	OnTopKExpansion func(reason string)
	// OnPrefetchBatch fires once per PrefetchLayers call with the
	// number of distinct pullers contacted and the number of layer
	// digests grouped into those batches (after self / unreachable
	// filtering). Maps to the design doc / the design doc batched-please_pull metrics:
	// `p2p_prefetch_batches_total` (count) and
	// `p2p_prefetch_digests_batched_total` (sum).
	OnPrefetchBatch func(pullers, digests int)
}

// Options configures a Resolver.
type Options struct {
	Members   ifaces.Members
	Discovery Discovery
	Coord     ifaces.Coordinator
	Inflight  *inflight.Map
	Logger    *slog.Logger
	Metrics   MetricsHooks
	Now       func() time.Time
	HrwK      int       // default 3
	HrwScope  hrw.Scope // default ScopeCluster
	SelfZone  string    // required when HrwScope == ScopeZone

	// LocalIntent computes self's PullIntent synchronously, without
	// the libp2p coord round-trip. When non-nil, the cold-start
	// orchestrator includes self as a first-class participant in the
	// the design doc rule cascade - rule 2 (cache hit on self), rule 3 (self
	// in-flight), rule 4 (self in cooldown), and rule 7 (self picked
	// as designated puller) all behave the same as for any peer.
	//
	// Without LocalIntent, the resolver excludes self from
	// queryTargets and from `reachable`, which means a self-as-HRW-
	// rank-0 case routes please_pull to rank 1 - two nodes both
	// trying to delegate to each other can each origin-pull the same
	// digest, violating the cache-hit "one origin pull per digest"
	// invariant. New deployments MUST wire LocalIntent.
	LocalIntent ifaces.LocalIntentProvider
	// LocalPull starts an origin pull on self without the libp2p
	// please_pull RPC. Used when rule 7's lowest-rank-reachable puller
	// is self. nil + LocalIntent non-nil + rule 7 picks self ->
	// resolver falls back to Coord.PleasePull(self, ...), which will
	// either fail to dial or burn a stream slot to no benefit; tests
	// should set both together.
	LocalPull ifaces.LocalPullStarter

	// Tunables (defaults applied if zero).
	QueryTimeout         time.Duration // default 2s - step 5 wait window
	PollManifest         time.Duration // default 200ms -
	PollLayer            time.Duration // default 1s -
	TransientCooldownCap time.Duration // default 30s - rule 4
	HonorSweepInterval   time.Duration // default 1m, negative disables full sweeps

	// TopKExpansionFactor is the multiplier applied to HrwK on the
	// expansion pass under rule 5 / rule 6 (the step 5; the design doc
	// `topk_expansion_factor_degraded`). Defaults to 2 when ≤1.
	TopKExpansionFactor int

	// TrustedFailureClasses is the set of the design doc origin-error classes
	// the requester accepts as a cluster-wide 5xx-immediate signal
	// when a top-K responder reports recently_failed in rule 1.
	// Classes outside this set (notably `transient`) are honored
	// only locally by the reporting puller. Empty defaults to
	// {auth, not_found, rate_limited} per design the design doc / config
	// `origin_failure_classes_trusted_cluster_wide`.
	TrustedFailureClasses []ifaces.FailureClass
}

// Resolver runs the cascade.
type Resolver struct {
	opts Options

	// honorMu guards honorUntil. honorUntil records the requester-
	// side local honor window per digest: when a transient cooldown
	// was last observed, the requester suppresses please_pull (and
	// in fact the whole probe pass) for `min(cooldown_until - now,
	// TransientCooldownCap)`. Evicted on access once the deadline
	// passes; see suppressedByHonorWindow.
	honorMu        sync.Mutex
	honorUntil     map[digest.Digest]time.Time
	nextHonorSweep time.Time
}

// New builds a Resolver. Required fields: Members, Discovery, Coord,
// Inflight.
func New(opts Options) *Resolver {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	opts.Logger = opts.Logger.With(slog.String("subsystem", "coldstart"))

	if opts.Members == nil {
		panic("coldstart.New: Members is required")
	}

	if opts.Discovery == nil {
		panic("coldstart.New: Discovery is required")
	}

	if opts.Coord == nil {
		panic("coldstart.New: Coord is required")
	}

	if opts.Inflight == nil {
		panic("coldstart.New: Inflight is required")
	}

	if opts.Now == nil {
		opts.Now = time.Now
	}

	if opts.HrwK <= 0 {
		opts.HrwK = 3
	}

	if opts.QueryTimeout <= 0 {
		opts.QueryTimeout = 2 * time.Second
	}

	if opts.PollManifest <= 0 {
		opts.PollManifest = 200 * time.Millisecond
	}

	if opts.PollLayer <= 0 {
		opts.PollLayer = 1 * time.Second
	}

	if opts.TransientCooldownCap <= 0 {
		opts.TransientCooldownCap = 30 * time.Second
	}

	if opts.HonorSweepInterval == 0 {
		opts.HonorSweepInterval = defaultHonorSweepInterval
	}

	if opts.TopKExpansionFactor < 2 {
		opts.TopKExpansionFactor = 2
	}

	if len(opts.TrustedFailureClasses) == 0 {
		opts.TrustedFailureClasses = []ifaces.FailureClass{
			ifaces.FailureAuth,
			ifaces.FailureNotFound,
			ifaces.FailureRateLimited,
		}
	}

	return &Resolver{
		opts:       opts,
		honorUntil: make(map[digest.Digest]time.Time),
	}
}

// Resolution carries the orchestrator's verdict.
type Resolution struct {
	// Providers are transfer endpoints (host:port) the caller should
	// fetch from, in priority order. Non-empty on success.
	Providers []ifaces.Provider
	// Outcome names which rule fired. Useful for tests and metrics.
	Outcome string
}

// Resolve runs the cascade for d. The returned error is one of the
// sentinel errors above, or a context cancellation, or a transport
// error. expectedSize is 0 if unknown (e.g., manifest digest before
// parsing); it is used to compute the per-the design doc stall threshold for
// rule 3 (in_flight) and the DHT-polling deadline overall.
//
// registry+repository identify the upstream and OCI repo for the
// please_pull RPC (the design doc single-repo-per-batch invariant). Both must be
// non-empty for rule 7 to fire; the orchestrator otherwise falls
// through to ErrExhausted.
func (r *Resolver) Resolve(ctx context.Context, d digest.Digest, kind ifaces.OriginRefKind, registry, repository string, expectedSize int64) (*Resolution, error) {
	start := r.opts.Now()
	kindLabel := kindLabel(kind)

	defer func() {
		// outcome is set by the named return wrapper below.
	}()

	// the design doc requester-side honor window: if we recently observed a
	// transient cooldown for this digest and the capped window has
	// not yet elapsed, short-circuit without probing the top-K. This
	// matches the design's "apply a local honor window" rule and
	// suppresses redundant probe traffic across kubelet retries.
	if r.suppressedByHonorWindow(d) {
		r.bumpDuration(kindLabel, "rule4_cooldown_honored", start)
		return nil, ErrCooldownActive
	}

	// Step 1 + 2: top-K + parallel pull_intent_query.
	cluster := r.opts.Members.Snapshot()
	scope := r.opts.HrwScope
	scopedZone := r.opts.SelfZone

	candidates := hrw.Candidates(cluster, scope, scopedZone)
	if scope == hrw.ScopeZone && len(candidates) == 0 {
		// Zone-mode degrades to cluster-mode when the zone is empty
		// (e.g., this node has no zone label). Matches the design's
		// the design doc fallback expectation.
		candidates = cluster
	}

	// Pass 1: top-K.
	// Capture DHT health ONCE per Resolve invocation. probe consults
	// health for rule-6 / rule-7 decisions and Resolve consults it
	// again below for rule-6 expansion - reading twice can flip a
	// healthy cluster into degraded mode mid-decision (TOCTOU).
	health := r.opts.Discovery.Health()

	res, outcome, err := r.probe(ctx, d, kind, registry, repository, expectedSize, candidates, r.opts.HrwK, "", health)
	if err == nil {
		r.bumpDuration(kindLabel, outcome, start)
		return res, nil
	}

	// Rules 5/6: expansion to 2K. These apply when:
	// - rule 5: probe returned ErrNoReachable (no reachable top-K).
	// - rule 6: probe returned ErrAllNeitherCachedNorInFlight AND
	// DHT health is below the Healthy threshold (0.7).
	// Below 0.3 the health is "Unhealthy" per the design doc and the
	// expansion reason is distinguished from "Degraded".
	expand := false
	expandReason := ""
	expandMetricReason := ""

	switch {
	case errors.Is(err, errNoReachable):
		expand = true
		expandReason = "rule5_all_unreachable"
		expandMetricReason = "all_unreachable"
	case errors.Is(err, errAllNeither):
		if health < 0.7 {
			expand = true
			expandReason = "rule6_degraded_expand"

			if health < 0.3 {
				expandMetricReason = "unhealthy_health"
			} else {
				expandMetricReason = "degraded_health"
			}
		}
	}

	if expand {
		factor := r.opts.TopKExpansionFactor
		if factor < 2 {
			factor = 2
		}

		if r.opts.Metrics.OnTopKExpansion != nil {
			r.opts.Metrics.OnTopKExpansion(expandMetricReason)
		}

		res, outcome, err = r.probe(ctx, d, kind, registry, repository, expectedSize, candidates, r.opts.HrwK*factor, expandReason, health)
		if err == nil {
			r.bumpDuration(kindLabel, outcome, start)
			return res, nil
		}
	}

	// Translate internal errors into the public sentinel set.
	mappedOutcome, out := mapTerminalErr(err)
	r.bumpDuration(kindLabel, mappedOutcome, start)

	return nil, out
}

// errNoReachable / errAllNeither are internal - see probe and
// mapTerminalErr.
var (
	errNoReachable = errors.New("coldstart: no reachable top-K")
	errAllNeither  = errors.New("coldstart: all reachable report neither cached nor in-flight (rule 7 path)")
)

func mapTerminalErr(err error) (string, error) {
	switch {
	case err == nil:
		return "ok", nil
	case errors.Is(err, ErrFailureShortCircuit):
		return "rule1_failure", ErrFailureShortCircuit
	case errors.Is(err, ErrCooldownActive):
		return "rule4_cooldown", ErrCooldownActive
	case errors.Is(err, errNoReachable), errors.Is(err, errAllNeither), errors.Is(err, ErrExhausted):
		return "exhausted", ErrExhausted
	default:
		return "error", err
	}
}

// probe runs one pass of pull_intent_query against the top-N candidates
// and evaluates the rule cascade. expandLabel is non-empty when
// this is the expansion pass (top-2K) and identifies which rule fired
// the expansion (used as the outcome label).
//
// Returns (Resolution, outcomeLabel, nil) on success; or
// (nil, "", err) where err is one of the internal/public sentinels.
func (r *Resolver) probe(ctx context.Context, d digest.Digest, kind ifaces.OriginRefKind, registry, repository string, expectedSize int64, candidates []ifaces.Node, k int, expandLabel string, health float64) (*Resolution, string, error) {
	top := hrw.TopK(candidates, d, k)
	if len(top) == 0 {
		return nil, "", errNoReachable
	}

	self := r.opts.Members.Self()
	// Find self's rank in top (if present) so we can synthesize a
	// self-response below. selfIdx == -1 means self isn't in the
	// current top-N - typically because the cluster has at least K
	// other nodes that all rank higher for this digest, in which
	// case self is irrelevant to the cascade.
	selfIdx := -1

	for i, s := range top {
		if s.Node.ID == self {
			selfIdx = i
			break
		}
	}
	// Don't pull_intent_query ourselves - we already know our state.
	// Each target carries its requester-computed HRW rank (its index
	// in top); fanOut threads that rank into the response so puller
	// selection in lowestRankReachable runs against the requester's
	// own ranking and is independent of whatever rank the responder
	// reports (the design doc: responder rank is a *debug* signal, not the
	// authoritative selector - see Batch-43 rationale below).
	queryTargets := make([]rankedTarget, 0, len(top))
	for i, s := range top {
		if s.Node.ID != self {
			queryTargets = append(queryTargets, rankedTarget{Scored: s, requesterRank: int32(i)})
		}
	}

	probeCtx, cancel := context.WithTimeout(ctx, r.opts.QueryTimeout)
	defer cancel()

	responses := r.fanOut(probeCtx, queryTargets, d)

	// Synthesize self's response from LocalIntent so the rule cascade
	// can pick self when self is rank 0 (or for cache-hit / in-flight
	// piggyback). Without this, an excluded-self set means
	// lowestRankReachable never returns self even though self IS
	// the HRW-designated puller - two nodes both ranking each other
	// as the puller can each issue a please_pull and both
	// origin-pull, violating the cache-hit invariant.
	//
	// Uses probeCtx (not the outer ctx) so self-intent obeys the
	// same per-query deadline as the peer fan-out. LocalPullIntent
	// may consult the secondary blob source (containerd content
	// store) on cache miss - that lookup is fast in steady state
	// but can stall a containerd that's busy compacting its content
	// store, and without the timeout a slow self-intent would block
	// the whole probe past the per-digest budget while peers have
	// already returned.
	if r.opts.LocalIntent != nil && selfIdx >= 0 {
		selfIntent := r.opts.LocalIntent.LocalPullIntent(probeCtx, d)
		// Defensive: the synthetic responder MUST report the rank we
		// expect for self so the cascade's lowest-rank-reachable
		// picks correctly. The server-side computeLocalIntent uses
		// the same membership view we did, but if the snapshot
		// changed mid-flight, fall back to our index.
		if selfIntent.RecipientRank != int32(selfIdx) {
			selfIntent.RecipientRank = int32(selfIdx)
		}

		responses = append(responses, response{
			node:          top[selfIdx].Node,
			ok:            true,
			intent:        selfIntent,
			requesterRank: int32(selfIdx),
		})
	}

	// the design doc: emit hrw_rank_mismatch when responder's reported rank
	// disagrees with our locally computed rank.
	r.checkRankMismatches(top, responses, d, kindLabel(kind))

	// Apply rules in strict priority order.
	reachable := reachableResponses(responses)

	// Rule 1: failure short-circuit.
	if v := findFailureShortCircuit(reachable, r.opts.TrustedFailureClasses); v != nil {
		return nil, prefix(expandLabel, "rule1_failure"), ErrFailureShortCircuit
	}

	// Rule 2: cache hit.
	if v := findCacheHit(reachable); v != nil {
		// DHT-false-empty marker: we got here because FindProviders
		// returned 0, yet a peer claims has_cached. Emit metric.
		if r.opts.Metrics.OnDhtFalseEmpty != nil {
			r.opts.Metrics.OnDhtFalseEmpty()
		}

		if r.opts.Metrics.OnTopKProbeHit != nil {
			r.opts.Metrics.OnTopKProbeHit()
		}

		return r.providersFor(v, top), prefix(expandLabel, "rule2_cache_hit"), nil
	}

	// Rule 3: in-flight piggyback.
	if v := findInFlight(reachable, r.opts.Inflight, kind, expectedSize, r.opts.Now(), r.opts.Metrics.OnDesignatedPullerTakeover); v != nil {
		if r.opts.Metrics.OnTopKProbeHit != nil {
			r.opts.Metrics.OnTopKProbeHit()
		}

		providers, err := r.pollDHT(ctx, d, kind, expectedSize)
		if err != nil {
			return nil, prefix(expandLabel, "rule3_inflight_poll_exhausted"), ErrExhausted
		}

		return &Resolution{Providers: providers, Outcome: prefix(expandLabel, "rule3_inflight")}, prefix(expandLabel, "rule3_inflight"), nil
	}

	// Rule 4: transient cooldown. Capture the latest cooldown_until so
	// the requester's local honor window can be set, bounded by
	// TransientCooldownCap (the design doc "apply a local honor window of
	// min(cooldown_until - now, cap)").
	if hit, cooldownUntil := findTransientCooldown(reachable); hit {
		r.recordHonorWindow(d, cooldownUntil)
		return nil, prefix(expandLabel, "rule4_cooldown"), ErrCooldownActive
	}

	// Rule 5/6 are decided one frame up in Resolve once probe
	// reports its own diagnosis here:
	// - 0 reachable -> errNoReachable (rule 5 expand)
	// - all reachable report neither cached nor in-flight -> errAllNeither
	// (rule 6 expand or rule 7 cold-start depending on DHT health)
	if len(reachable) == 0 {
		return nil, "", errNoReachable
	}

	// Rule 7: cold-start. Lowest hrw_rank reachable wins.
	puller := lowestRankReachable(reachable)
	if puller == nil {
		// Defensive: should not be possible - reachable is non-empty
		// but no entry had a valid rank. Treat as exhausted.
		return nil, prefix(expandLabel, "rule7_no_puller"), ErrExhausted
	}

	// If the orchestrator hasn't expanded yet AND DHT health is below
	// the Healthy threshold, surface errAllNeither so Resolve
	// can expand to top-2K *before* cold-starting (the design doc rule 6). Under
	// Healthy DHT we proceed straight to rule 7 at top-K - expansion
	// is the degraded-mode safety net, not a prerequisite for
	// cold-start. health is captured once per Resolve invocation to
	// avoid a TOCTOU between rule-6 evaluation here and in Resolve.
	if expandLabel == "" && health < 0.7 {
		return nil, "", errAllNeither
	}

	// Cold-start: please_pull, then DHT-poll for completion.
	outcome, err := r.sendPleasePull(ctx, *puller, d, kind, registry, repository)
	if err != nil {
		return nil, prefix(expandLabel, "rule7_please_pull_failed"), ErrExhausted
	}
	// Interpret the per-digest outcome BEFORE polling. The puller's
	// state can change between pull_intent_query (rule 1 evaluation)
	// and please_pull (rule 7 dispatch) - e.g. an in-flight pull may
	// have hit a 401 and entered the negative cache in between. Acting
	// on the stale rule-1 view would silently waste a per-digest stall
	// budget on a DHT poll that can never succeed; worse, on
	// RECENTLY_FAILED we MUST surface the same cluster-wide
	// short-circuit / honor-window semantics as rule 1 / rule 4.
	switch outcome.Outcome {
	case ifaces.PleasePullStarted, ifaces.PleasePullAlreadyPulling:
		// Pull is committed (or already running) - falls through
		// to the DHT poll below.
	case ifaces.PleasePullRecentlyFailed:
		if isTrustedFailureClass(outcome.FailureClass, r.opts.TrustedFailureClasses) {
			// auth / not_found / rate_limited: the puller's local
			// negative cache reports a class for which retry-now
			// is provably useless cluster-wide. Identical handling
			// to rule 1, just observed one RPC later because the
			// transition happened mid-cascade.
			return nil, prefix(expandLabel, "rule7_failure_short_circuit"), ErrFailureShortCircuit
		}
		// Transient: apply the same local honor window that rule 4
		// uses, bounded by TransientCooldownCap. Return
		// ErrCooldownActive so the next Resolve within the window
		// short-circuits without re-probing.
		r.recordHonorWindow(d, outcome.CooldownUntil)

		return nil, prefix(expandLabel, "rule7_cooldown"), ErrCooldownActive
	default:
		// PleasePullUnspecified or an unrecognised enum value: the
		// puller did not commit to starting a pull, so the DHT
		// provider record may never appear. Polling for the full
		// stall budget would be a multi-second wait for nothing;
		// surface exhaustion immediately so the caller can fail or
		// back off.
		return nil, prefix(expandLabel, "rule7_please_pull_unspecified"), ErrExhausted
	}

	providers, err := r.pollDHT(ctx, d, kind, expectedSize)
	if err != nil {
		return nil, prefix(expandLabel, "rule7_poll_exhausted"), ErrExhausted
	}

	return &Resolution{Providers: providers, Outcome: prefix(expandLabel, "rule7_cold_start")}, prefix(expandLabel, "rule7_cold_start"), nil
}

// fanOut issues pull_intent_query to every target in parallel. Each
// response (or error) is recorded in the returned slice, indexed by
// target. Targets with nil queryTimeout protection rely on ctx. The
// requester-computed HRW rank is threaded onto the response so
// downstream selection (lowestRankReachable) does not depend on the
// responder reporting its own rank correctly.
func (r *Resolver) fanOut(ctx context.Context, targets []rankedTarget, d digest.Digest) []response {
	out := make([]response, len(targets))

	var wg sync.WaitGroup
	wg.Add(len(targets))

	for i, s := range targets {
		i, s := i, s

		go func() {
			defer wg.Done()

			intent, err := r.opts.Coord.PullIntentQuery(ctx, s.Node.ID, d)
			out[i] = response{
				node:          s.Node,
				ok:            err == nil,
				err:           err,
				intent:        intent,
				requesterRank: s.requesterRank,
			}
		}()
	}

	wg.Wait()

	return out
}

// rankedTarget pairs an HRW scored node with the rank the requester
// (the caller of fanOut) has computed for it. Threading the rank
// onto the response ensures the puller-selection sort uses the
// requester's own ranking, not whatever rank the responder reports
// - which can be stale during informer lag or a rolling membership
// update and would otherwise let the cascade pick the wrong puller
// in exactly the convergence window where the algorithm is most
// fragile.
type rankedTarget struct {
	hrw.Scored
	requesterRank int32
}

type response struct {
	node          ifaces.Node
	ok            bool
	err           error
	intent        ifaces.PullIntent
	requesterRank int32 // requester-computed HRW rank for response.node
}

func reachableResponses(in []response) []response {
	out := make([]response, 0, len(in))
	for _, r := range in {
		if r.ok {
			out = append(out, r)
		}
	}

	return out
}

func findFailureShortCircuit(rs []response, trusted []ifaces.FailureClass) *response {
	for i := range rs {
		if !rs[i].intent.RecentlyFailed {
			continue
		}

		if isTrustedFailureClass(rs[i].intent.FailureClass, trusted) {
			return &rs[i]
		}
	}

	return nil
}

// isTrustedFailureClass reports whether class is in the operator's
// trusted-failure-class allow-list (the design doc). Trusted failure classes
// (auth / not_found / rate_limited) are those for which a single
// origin response is sufficient evidence to short-circuit cluster-
// wide: every node would hit the same wall on retry-now. Transient
// is intentionally excluded - it may resolve on the next attempt and
// a single bad response must not propagate.
//
// Used by both findFailureShortCircuit (rule 1, evaluating peer
// PullIntent responses) and the rule-7 please_pull dispatcher
// (evaluating PleasePullRecentlyFailed outcomes from the designated
// puller). Keeping the predicate in one place ensures the two
// rules' definitions of "trusted" cannot drift.
func isTrustedFailureClass(class ifaces.FailureClass, trusted []ifaces.FailureClass) bool {
	for _, t := range trusted {
		if class == t {
			return true
		}
	}

	return false
}

func findCacheHit(rs []response) *response {
	for i := range rs {
		if rs[i].intent.HasCached {
			return &rs[i]
		}
	}

	return nil
}

func findInFlight(rs []response, infl *inflight.Map, kind ifaces.OriginRefKind, expectedSize int64, now time.Time, onTakeover func(kindLabel string)) *response {
	for i := range rs {
		intent := rs[i].intent
		if !intent.InFlight {
			continue
		}
		// the design doc stall check: exclude the reporter if started_at is too
		// old for the per-digest timeout.
		if !intent.StartedAt.IsZero() && infl != nil {
			elapsed := now.Sub(intent.StartedAt)

			threshold := infl.Stalls().ResolveStall(kind, expectedSize)
			if elapsed > threshold {
				// Stale puller - emit the takeover metric and
				// keep searching. Rank-1 (next entry) may still serve.
				if onTakeover != nil {
					onTakeover(kindLabel(kind))
				}

				continue
			}
		}

		return &rs[i]
	}

	return nil
}

func findTransientCooldown(rs []response) (bool, time.Time) {
	// Returns whether any reachable response reports a transient
	// cooldown and, if so, the latest cooldown_until across all such
	// responses. Picking the latest is conservative: it gives the
	// requester the longest honor window any puller is asking for.
	hit := false

	var until time.Time

	for _, r := range rs {
		if !r.intent.RecentlyFailed || r.intent.FailureClass != ifaces.FailureTransient {
			continue
		}

		hit = true

		if r.intent.CooldownUntil.After(until) {
			until = r.intent.CooldownUntil
		}
	}

	return hit, until
}

func lowestRankReachable(rs []response) *response {
	// Sort by requester-computed hrw_rank ascending; the
	// lowest-numbered rank is the highest-priority puller
	// (the step 6).
	//
	// We deliberately use the *requester's* rank (response.requesterRank,
	// set in fanOut from this resolver's own HRW top-K) rather than
	// the responder-reported intent.RecipientRank. Otherwise a peer
	// that is mid-rollout, has stale membership informer cache, or
	// is simply misconfigured can mis-report its rank and steer the
	// requester to a non-canonical puller - duplicating origin
	// pulls in exactly the convergence windows the algorithm is
	// meant to protect. The responder-reported rank is kept around
	// (see checkRankMismatches) purely as a divergence signal.
	sorted := append([]response(nil), rs...)
	sort.SliceStable(sorted, func(i, j int) bool {
		ri, rj := sorted[i].requesterRank, sorted[j].requesterRank
		// Treat negative (unknown) ranks as +∞ so they're picked last.
		if ri < 0 {
			ri = int32(1<<31 - 1)
		}

		if rj < 0 {
			rj = int32(1<<31 - 1)
		}

		return ri < rj
	})

	if len(sorted) == 0 {
		return nil
	}

	return &sorted[0]
}

func (r *Resolver) checkRankMismatches(top []hrw.Scored, rs []response, d digest.Digest, kindLabel string) {
	if r.opts.Metrics.OnRankMismatch == nil {
		return
	}
	// Build a node-id -> expected-rank map from our own scoring.
	idToRank := make(map[ifaces.NodeID]int32, len(top))
	for i, s := range top {
		idToRank[s.Node.ID] = int32(i)
	}

	for _, resp := range rs {
		if !resp.ok {
			continue
		}

		want, known := idToRank[resp.node.ID]
		if !known {
			continue
		}

		if resp.intent.RecipientRank != want {
			r.opts.Logger.Warn("hrw_rank_mismatch",
				slog.String("digest", d.String()),
				slog.String("recipient_node", string(resp.node.ID)),
				slog.Int("our_rank", int(want)),
				slog.Int("their_rank", int(resp.intent.RecipientRank)),
			)
			r.opts.Metrics.OnRankMismatch(kindLabel, resp.node.ID)
		}
	}
}

func (r *Resolver) providersFor(v *response, top []hrw.Scored) *Resolution {
	// Build a list of providers in HRW rank order, with the cache-hit
	// responder first (so the warm-path peer fetch loop tries it
	// before falling back to other reachable top-K members).
	out := []ifaces.Provider{{NodeID: v.node.ID, Addr: v.node.Addr}}
	for _, s := range top {
		if s.Node.ID == v.node.ID {
			continue
		}

		out = append(out, ifaces.Provider{NodeID: s.Node.ID, Addr: s.Node.Addr})
	}

	return &Resolution{Providers: out, Outcome: "rule2_cache_hit"}
}

func (r *Resolver) sendPleasePull(ctx context.Context, puller response, d digest.Digest, kind ifaces.OriginRefKind, registry, repository string) (ifaces.PleasePullOutcome, error) {
	// the design doc invariant: please_pull is a single repo per batch. If the
	// caller didn't supply registry+repository, refuse to send a
	// malformed RPC (the server would reject it) and surface a
	// terminal error so the cascade reports rule7_please_pull_failed
	// instead of silently succeeding.
	if registry == "" || repository == "" {
		return ifaces.PleasePullOutcome{Digest: d}, fmt.Errorf("coldstart: please_pull requires non-empty registry+repository (got %q/%q)", registry, repository)
	}
	// Single-digest call; batching is the orchestrator-caller's job
	// (the mirror at the layer-fanout level groups by puller).
	ctx, cancel := context.WithTimeout(ctx, r.opts.QueryTimeout)
	defer cancel()

	var (
		outcomes []ifaces.PleasePullOutcome
		err      error
	)
	// When the designated puller is self, skip the libp2p stream and
	// drive the local pullerPump directly. Identity of "puller is
	// self" is whatever Members.Self reports - the synthetic
	// response added in probe uses the same ID, so a rule-7 match
	// on self is detectable by NodeID equality. Without this branch,
	// Coord.PleasePull(self, ...) would either fail to dial (peer.ID
	// = our own peer.ID does not resolve to a network address) or
	// round-trip through libp2p for no benefit.
	if r.opts.LocalPull != nil && puller.node.ID == r.opts.Members.Self() {
		outcomes, err = r.opts.LocalPull.StartLocalPull(ctx, registry, repository, kind, []digest.Digest{d})
	} else {
		outcomes, err = r.opts.Coord.PleasePull(ctx, puller.node.ID, registry, repository, kind, []digest.Digest{d})
	}

	if err != nil {
		return ifaces.PleasePullOutcome{Digest: d}, err
	}
	// The server SHOULD return exactly one outcome per requested
	// digest; be defensive in case the wire is desynced. Look for our
	// digest specifically rather than assuming index 0 - a buggy
	// implementation that returned outcomes in different order than
	// the request would silently drive the wrong cascade branch.
	for _, o := range outcomes {
		if o.Digest == d {
			return o, nil
		}
	}
	// Empty / mismatched response: caller maps PleasePullUnspecified
	// to ErrExhausted rather than polling the DHT (the puller never
	// committed to starting a pull, so the provider record may never
	// land).
	return ifaces.PleasePullOutcome{Digest: d}, nil
}

func (r *Resolver) pollDHT(ctx context.Context, d digest.Digest, kind ifaces.OriginRefKind, expectedSize int64) ([]ifaces.Provider, error) {
	// the design doc polling interval + per-digest stall threshold.
	interval := r.opts.PollLayer
	if kind == ifaces.KindManifest {
		interval = r.opts.PollManifest
	}

	threshold := r.opts.Inflight.Stalls().ResolveStall(kind, expectedSize)
	deadline := r.opts.Now().Add(threshold)

	pollCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	t := time.NewTicker(interval)
	defer t.Stop()

	// One immediate lookup before the first sleep - minimizes latency
	// when the provider record landed just before we started polling.
	if provs, err := r.opts.Discovery.FindProviders(pollCtx, d); err == nil && len(provs) > 0 {
		return provs, nil
	}

	for {
		select {
		case <-pollCtx.Done():
			return nil, ErrExhausted
		case <-t.C:
			provs, err := r.opts.Discovery.FindProviders(pollCtx, d)
			if err != nil {
				continue
			}

			if len(provs) > 0 {
				return provs, nil
			}
		}
	}
}

func (r *Resolver) bumpDuration(kindLabel, outcome string, start time.Time) {
	if r.opts.Metrics.OnColdStartDuration == nil {
		return
	}

	r.opts.Metrics.OnColdStartDuration(kindLabel, outcome, r.opts.Now().Sub(start))
}

// suppressedByHonorWindow reports whether d is currently in this
// requester's local the design doc transient honor window. Evicts the entry on
// access if the window has elapsed (lazy GC keeps the map bounded
// without a background goroutine).
func (r *Resolver) suppressedByHonorWindow(d digest.Digest) bool {
	r.honorMu.Lock()
	defer r.honorMu.Unlock()

	now := r.opts.Now()
	r.sweepHonorWindowsLocked(now)

	until, ok := r.honorUntil[d]
	if !ok {
		return false
	}

	if !now.Before(until) {
		delete(r.honorUntil, d)
		return false
	}

	return true
}

// recordHonorWindow stores a new honor-window deadline for d derived
// from the puller's advertised cooldown_until, capped at
// TransientCooldownCap. A non-positive remaining duration (the
// puller's cooldown already elapsed) is dropped so the next request
// re-probes immediately.
func (r *Resolver) recordHonorWindow(d digest.Digest, cooldownUntil time.Time) {
	maxWindow := r.opts.TransientCooldownCap
	if maxWindow <= 0 {
		return
	}

	now := r.opts.Now()

	remaining := cooldownUntil.Sub(now)
	if remaining <= 0 {
		return
	}

	if remaining > maxWindow {
		remaining = maxWindow
	}

	r.honorMu.Lock()
	r.sweepHonorWindowsLocked(now)
	r.honorUntil[d] = now.Add(remaining)
	r.honorMu.Unlock()
}

func (r *Resolver) sweepHonorWindowsLocked(now time.Time) {
	if r.opts.HonorSweepInterval < 0 {
		return
	}

	if !r.nextHonorSweep.IsZero() && now.Before(r.nextHonorSweep) {
		return
	}

	if r.opts.HonorSweepInterval > 0 {
		r.nextHonorSweep = now.Add(r.opts.HonorSweepInterval)
	}

	for d, until := range r.honorUntil {
		if now.Before(until) {
			continue
		}

		delete(r.honorUntil, d)
	}
}

func kindLabel(k ifaces.OriginRefKind) string {
	switch k {
	case ifaces.KindManifest:
		return "manifest"
	case ifaces.KindConfig:
		return "config"
	default:
		return "layer"
	}
}

func prefix(p, s string) string {
	if p == "" {
		return s
	}

	return fmt.Sprintf("%s/%s", p, s)
}
