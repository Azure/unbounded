// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package coldstart

// Speculative wire-level batching described in detailed-design.md §5.2
// (L332) and architecture.md L180:
//
//	"when multiple cold-start layers all HRW to the same designated
//	 puller (which happens often when K is small relative to the
//	 cluster), the agent may send a single
//	 please_pull([digest1, digest2, …]) carrying all such digests"
//
// Prefetch is intentionally NOT the full §5.2 cascade. It is a
// best-effort warm-up fired by the mirror's manifest serve path: when
// the mirror serves a manifest it knows the layer digests up-front,
// and pre-emptively asking each layer's HRW rank-0 reachable peer to
// start pulling means the cluster is already warm by the time
// containerd issues per-layer GETs. If the rank-0 puller turns out
// to be unreachable or already failed, no harm done — when containerd
// asks for that layer, the full Resolve cascade runs and recovers via
// rules 5/6/7 expansion.
//
// Batching is the entire point: 10 layers landing on 3 pullers
// produces 3 PleasePull RPCs, not 10. The puller's per-digest
// in-flight dedupe is unchanged.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/hrw"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

// PrefetchLayers groups digests by their HRW rank-0 reachable
// designated puller and issues one PleasePull RPC per puller. Digests
// HRW'ing to self are diverted to the local LocalPullStarter (if
// configured) and batched as a single StartLocalPull call; if no
// LocalPullStarter is configured, the self-bucket is skipped (the
// per-digest Resolve cascade will still recover via rule 7 when
// containerd actually asks for the layer).
// Already-cached or otherwise filtered digests should be removed by
// the caller before invoking.
//
// PrefetchLayers blocks until every per-puller RPC has completed or
// errored, but each RPC is bounded by QueryTimeout. Callers run it in
// a goroutine for fire-and-forget semantics. A returned error means
// at least one puller's RPC failed; callers can log it and otherwise
// ignore — the next per-digest Resolve call from containerd will fall
// back to the full §5.2 cascade.
//
// registry and repository identify the upstream and OCI repo. They
// MUST be non-empty (§4.4 single-repo-per-batch invariant); an empty
// value returns ErrPrefetchInvalid without issuing any RPC.
//
// All digests passed via PrefetchLayers are sent as KindBlob; this
// is a back-compat shim that pre-dates per-kind labelling. New
// callers should use PrefetchChildren which preserves the
// config-vs-layer kind end-to-end through the wire so per-kind
// metrics ("manifest | config | layer") remain honest.
func (r *Resolver) PrefetchLayers(ctx context.Context, digests []digest.Digest, registry, repository string) error {
	if registry == "" || repository == "" {
		return fmt.Errorf("%w: registry=%q repository=%q",
			ErrPrefetchInvalid, registry, repository)
	}
	if len(digests) == 0 {
		return nil
	}
	children := make([]ChildDigest, 0, len(digests))
	for _, d := range digests {
		children = append(children, ChildDigest{Digest: d, Kind: ifaces.KindBlob})
	}
	return r.PrefetchChildren(ctx, children, registry, repository)
}

// ChildDigest pairs a child digest with the OCI URL-family kind the
// puller MUST target. Kind is one of ifaces.KindConfig (the manifest's
// image-config blob) or ifaces.KindBlob (every layer descriptor); both
// are pulled from /v2/<repo>/blobs/<digest> at the registry level but
// are carried separately on the wire so per-kind metrics agree
// end-to-end across the please_pull boundary. internal/manifest's
// TypedChildren is the canonical producer of these values.
type ChildDigest struct {
	Digest digest.Digest
	Kind   ifaces.OriginRefKind
}

// PrefetchChildren is the kind-preserving sibling of PrefetchLayers.
// It groups children by (HRW puller, kind) and emits one PleasePull
// (or StartLocalPull) RPC per group, so the §5.2a single-repo-per-
// batch invariant ("all digests in a batch MUST share kind") is
// honored while still preserving the per-kind metric label all the
// way through the wire.
//
// A manifest typically yields one KindConfig digest and N KindBlob
// digests. If all N+1 children HRW to the same puller, PrefetchChildren
// issues TWO RPCs (one per kind) rather than one mixed RPC — that's
// the trade for keeping the kind label honest. The CPU/RPC overhead
// is negligible (one extra round-trip per manifest serve, dwarfed by
// the layer pull itself), and the alternative (collapsing config
// into blob on the wire) leaves
// p2p_origin_pull_total{kind="config"} permanently zero.
func (r *Resolver) PrefetchChildren(ctx context.Context, children []ChildDigest, registry, repository string) error {
	if registry == "" || repository == "" {
		return fmt.Errorf("%w: registry=%q repository=%q",
			ErrPrefetchInvalid, registry, repository)
	}
	if len(children) == 0 {
		return nil
	}

	cluster := r.opts.Members.Snapshot()
	self := r.opts.Members.Self()
	candidates := hrw.Candidates(cluster, r.opts.HrwScope, r.opts.SelfZone)
	if r.opts.HrwScope == hrw.ScopeZone && len(candidates) == 0 {
		// Zone empty → fall back to cluster mode (mirrors Resolve's
		// behaviour, §4.3).
		candidates = cluster
	}
	if len(candidates) == 0 {
		return nil
	}

	// Group children by (HRW rank-0 puller, kind). Self is a valid
	// puller key when LocalPull is configured: we route its digests
	// through StartLocalPull as one batch per kind, matching the
	// rule-7 self path in probe(). When LocalPull is nil the self-
	// buckets are dropped before the dispatch fan-out (preserving the
	// original "skip-self" semantics). Deterministic digest order
	// inside each group: input order, which is also the order
	// containerd will pull the layers.
	type groupKey struct {
		node ifaces.NodeID
		kind ifaces.OriginRefKind
	}
	byGroup := make(map[groupKey][]digest.Digest)
	selfByKind := make(map[ifaces.OriginRefKind][]digest.Digest)
	skippedSelf := 0
	skippedNoTop := 0
	// Suppress duplicate digests within a single call so a malformed
	// manifest with repeated layer references doesn't inflate the RPC.
	// Dedupe is per-digest (not per (digest, kind)) because a single
	// content blob has exactly one kind — if a manifest somehow
	// referenced the same digest as both config and layer, the
	// first-seen kind wins.
	seen := make(map[string]struct{}, len(children))
	for _, c := range children {
		key := c.Digest.String()
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		top := hrw.TopK(candidates, c.Digest, 1)
		if len(top) == 0 {
			skippedNoTop++
			continue
		}
		puller := top[0].Node.ID
		if puller == self {
			if r.opts.LocalPull != nil {
				selfByKind[c.Kind] = append(selfByKind[c.Kind], c.Digest)
			} else {
				skippedSelf++
			}
			continue
		}
		gk := groupKey{node: puller, kind: c.Kind}
		byGroup[gk] = append(byGroup[gk], c.Digest)
	}
	if len(byGroup) == 0 && len(selfByKind) == 0 {
		r.opts.Logger.Debug("coldstart: prefetch had no remote pullers",
			slog.Int("children", len(children)),
			slog.Int("skipped_self", skippedSelf),
			slog.Int("skipped_no_top", skippedNoTop),
		)
		return nil
	}

	// Sort group keys for deterministic logging / test order. We sort
	// by (puller, kind) so a given puller's two-kind pair is
	// adjacent in the log.
	groupKeys := make([]groupKey, 0, len(byGroup))
	for gk := range byGroup {
		groupKeys = append(groupKeys, gk)
	}
	sort.Slice(groupKeys, func(i, j int) bool {
		if groupKeys[i].node != groupKeys[j].node {
			return groupKeys[i].node < groupKeys[j].node
		}
		return groupKeys[i].kind < groupKeys[j].kind
	})
	// Sort self-kinds too so the goroutine launch is deterministic.
	selfKinds := make([]ifaces.OriginRefKind, 0, len(selfByKind))
	for k := range selfByKind {
		selfKinds = append(selfKinds, k)
	}
	sort.Slice(selfKinds, func(i, j int) bool { return selfKinds[i] < selfKinds[j] })

	// Total puller count for the OnPrefetchBatch metric: distinct
	// remote pullers + 1 for self when self has digests to pull. We
	// count distinct PULLERS (not groups) because the puller is the
	// load-bearing unit for the histogram's "pullers per batch"
	// semantics — splitting one puller's children into two RPCs by
	// kind doesn't change the puller count.
	distinctPullers := make(map[ifaces.NodeID]struct{}, len(byGroup))
	for gk := range byGroup {
		distinctPullers[gk.node] = struct{}{}
	}
	totalPullers := len(distinctPullers)
	if len(selfByKind) > 0 {
		totalPullers++
	}
	totalDigests := len(children) - skippedSelf - skippedNoTop

	r.opts.Logger.Debug("coldstart: prefetch batching",
		slog.Int("children", len(children)),
		slog.Int("pullers", totalPullers),
		slog.Int("rpc_groups", len(byGroup)+len(selfByKind)),
		slog.Int("skipped_self", skippedSelf),
	)
	if r.opts.Metrics.OnPrefetchBatch != nil {
		r.opts.Metrics.OnPrefetchBatch(totalPullers, totalDigests)
	}

	var wg sync.WaitGroup
	var failures atomic.Int32
	totalGroups := len(byGroup) + len(selfByKind)
	// Self batches (one per kind, if any) run concurrently with peer
	// batches so a slow LocalPull doesn't gate remote dispatch — and
	// so a slow peer doesn't gate the local pull. All branches are
	// bounded by QueryTimeout so PrefetchChildren's overall latency
	// matches the pre-split PrefetchLayers behaviour.
	for _, k := range selfKinds {
		digests := selfByKind[k]
		if len(digests) == 0 {
			continue
		}
		wg.Add(1)
		go func(kind ifaces.OriginRefKind, ds []digest.Digest) {
			defer wg.Done()
			callCtx, cancel := context.WithTimeout(ctx, r.opts.QueryTimeout)
			defer cancel()
			_, err := r.opts.LocalPull.StartLocalPull(callCtx, registry, repository, kind, ds)
			if err != nil {
				failures.Add(1)
				r.opts.Logger.Debug("coldstart: prefetch local pull failed",
					slog.String("kind", kind.String()),
					slog.Int("batch_size", len(ds)),
					slog.Any("err", err),
				)
			}
		}(k, digests)
	}
	for _, gk := range groupKeys {
		ds := byGroup[gk]
		wg.Add(1)
		go func(node ifaces.NodeID, kind ifaces.OriginRefKind, digests []digest.Digest) {
			defer wg.Done()
			callCtx, cancel := context.WithTimeout(ctx, r.opts.QueryTimeout)
			defer cancel()
			_, err := r.opts.Coord.PleasePull(callCtx, node, registry, repository, kind, digests)
			if err != nil {
				failures.Add(1)
				r.opts.Logger.Debug("coldstart: prefetch please_pull failed",
					slog.String("puller", string(node)),
					slog.String("kind", kind.String()),
					slog.Int("batch_size", len(digests)),
					slog.Any("err", err),
				)
			}
		}(gk.node, gk.kind, ds)
	}
	wg.Wait()
	if n := failures.Load(); n > 0 {
		return fmt.Errorf("%w: %d/%d groups errored", ErrPrefetchPartial, n, totalGroups)
	}
	return nil
}

// ErrPrefetchInvalid signals a programmer error in the prefetch
// call: registry or repository was empty, which would produce a
// malformed PleasePull RPC.
var ErrPrefetchInvalid = errors.New("coldstart: prefetch invalid arguments")

// ErrPrefetchPartial signals at least one puller's PleasePull RPC
// failed. The other pullers were still asked to pull. Callers can
// errors.Is against this for retry / logging logic; the prefetch is
// best-effort so this is informational rather than fatal.
var ErrPrefetchPartial = errors.New("coldstart: prefetch had per-puller failures")
