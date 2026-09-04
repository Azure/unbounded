// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package coldstart

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/Azure/unbounded/internal/gantry/chairs"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/inflight"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
)

type ChairSnapshotCache interface {
	Snapshot(ctx context.Context, epoch int64) (chairs.Snapshot, error)
	RefreshChair(ctx context.Context, id chairs.ID) (chairs.Chair, error)
}

type ChairClaimer interface {
	ClaimFailed(ctx context.Context, chair chairs.Chair) (chairs.Chair, bool, error)
}

type ChairOptions struct {
	Chairs                ChairSnapshotCache
	Discovery             Discovery
	Coord                 ifaces.ChairCoordinator
	LocalPull             ifaces.LocalChairPullStarter
	Inflight              *inflight.Map
	SelfPeerID            ifaces.NodeID
	CurrentEpoch          func() int64
	InstallHolder         func(chairs.Holder) error
	Claimer               ChairClaimer
	Logger                *slog.Logger
	QueryTimeout          time.Duration
	PollManifest          time.Duration
	PollLayer             time.Duration
	APITimeout            time.Duration
	TrustedFailureClasses []ifaces.FailureClass
}

type ChairResolver struct {
	opts ChairOptions
}

func NewChairResolver(opts ChairOptions) *ChairResolver {
	if opts.Chairs == nil {
		panic("coldstart.NewChairResolver: Chairs is required")
	}

	if opts.Discovery == nil {
		panic("coldstart.NewChairResolver: Discovery is required")
	}

	if opts.Coord == nil {
		panic("coldstart.NewChairResolver: Coord is required")
	}

	if opts.Inflight == nil {
		panic("coldstart.NewChairResolver: Inflight is required")
	}

	if opts.CurrentEpoch == nil {
		panic("coldstart.NewChairResolver: CurrentEpoch is required")
	}

	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	if opts.QueryTimeout <= 0 {
		opts.QueryTimeout = 2 * time.Second
	}

	if opts.PollManifest <= 0 {
		opts.PollManifest = 200 * time.Millisecond
	}

	if opts.PollLayer <= 0 {
		opts.PollLayer = time.Second
	}

	if opts.APITimeout <= 0 {
		opts.APITimeout = 5 * time.Second
	}

	if len(opts.TrustedFailureClasses) == 0 {
		opts.TrustedFailureClasses = []ifaces.FailureClass{
			ifaces.FailureAuth,
			ifaces.FailureNotFound,
			ifaces.FailureRateLimited,
		}
	}

	opts.Logger = opts.Logger.With(slog.String("subsystem", "chair-coldstart"))

	return &ChairResolver{opts: opts}
}

func (r *ChairResolver) Resolve(ctx context.Context, d digest.Digest, kind ifaces.OriginRefKind, registry, repository string, expectedSize int64) (*Resolution, error) {
	apiCtx, cancel := context.WithTimeout(ctx, r.opts.APITimeout)
	snapshot, err := r.opts.Chairs.Snapshot(apiCtx, r.opts.CurrentEpoch())

	cancel()

	if err != nil {
		return nil, err
	}

	ranked := chairs.Rank(snapshot, d)
	if len(ranked) < chairs.SeedCount {
		return nil, ErrExhausted
	}

	trustedFailureClasses := r.opts.TrustedFailureClasses
	if registryauth.Authorization(ctx) != "" {
		trustedFailureClasses = withoutFailureClasses(trustedFailureClasses,
			ifaces.FailureAuth,
			ifaces.FailureNotFound,
			ifaces.FailureRateLimited,
		)
	}

	accepted := make([]chairs.Chair, 0, chairs.SeedCount)
	sawTransientFailure := false

	next := 0
	for len(accepted) < chairs.SeedCount && next < len(ranked) {
		batchSize := chairs.SeedCount - len(accepted)

		end := next + batchSize
		if end > len(ranked) {
			end = len(ranked)
		}

		result := r.dispatchChairs(ctx, ranked[next:end], registry, repository, kind, []digest.Digest{d}, trustedFailureClasses)
		if result.trustedFailure {
			return nil, ErrFailureShortCircuit
		}

		sawTransientFailure = sawTransientFailure || result.transientFailure
		accepted = append(accepted, result.accepted...)

		next = end
	}

	if len(accepted) == 0 {
		if sawTransientFailure {
			return nil, ErrCooldownActive
		}

		return nil, ErrExhausted
	}

	for {
		providers, err := r.pollDHT(ctx, d, kind, expectedSize)
		if err == nil {
			return &Resolution{Providers: providers, Outcome: "chair_cold_start"}, nil
		}

		// The accepted pull can fail after replying STARTED. Re-query those
		// chairs once so terminal origin failures short-circuit before the
		// resolver activates an ordered backup cohort.
		recheck := r.dispatchChairs(ctx, accepted, registry, repository, kind, []digest.Digest{d}, trustedFailureClasses)
		if recheck.trustedFailure {
			return nil, ErrFailureShortCircuit
		}

		sawTransientFailure = sawTransientFailure || recheck.transientFailure
		if next >= len(ranked) {
			if sawTransientFailure {
				return nil, ErrCooldownActive
			}

			return nil, ErrExhausted
		}

		end := next + chairs.SeedCount
		if end > len(ranked) {
			end = len(ranked)
		}

		backup := r.dispatchChairs(ctx, ranked[next:end], registry, repository, kind, []digest.Digest{d}, trustedFailureClasses)
		if backup.trustedFailure {
			return nil, ErrFailureShortCircuit
		}

		sawTransientFailure = sawTransientFailure || backup.transientFailure
		accepted = backup.accepted

		next = end
		for len(accepted) == 0 && next < len(ranked) {
			end = next + chairs.SeedCount
			if end > len(ranked) {
				end = len(ranked)
			}

			backup = r.dispatchChairs(ctx, ranked[next:end], registry, repository, kind, []digest.Digest{d}, trustedFailureClasses)
			if backup.trustedFailure {
				return nil, ErrFailureShortCircuit
			}

			sawTransientFailure = sawTransientFailure || backup.transientFailure
			accepted = backup.accepted
			next = end
		}

		if len(accepted) == 0 {
			if sawTransientFailure {
				return nil, ErrCooldownActive
			}

			return nil, ErrExhausted
		}
	}
}

type chairDispatchResult struct {
	accepted         []chairs.Chair
	trustedFailure   bool
	transientFailure bool
}

type chairCallResult struct {
	chair    chairs.Chair
	outcomes []ifaces.PleasePullOutcome
	err      error
}

func (r *ChairResolver) dispatchChairs(ctx context.Context, targets []chairs.Chair, registry, repository string, kind ifaces.OriginRefKind, digests []digest.Digest, trustedFailureClasses []ifaces.FailureClass) chairDispatchResult {
	results := make(chan chairCallResult, len(targets))

	var wg sync.WaitGroup

	for _, chair := range targets {
		chair := chair

		wg.Add(1)
		go func() {
			defer wg.Done()

			outcomes, err := r.pullChair(ctx, chair, registry, repository, kind, digests)
			results <- chairCallResult{chair: chair, outcomes: outcomes, err: err}
		}()
	}

	wg.Wait()
	close(results)

	result := chairDispatchResult{accepted: make([]chairs.Chair, 0, len(targets))}

	for call := range results {
		if call.err != nil {
			continue
		}

		accepted := false

		for _, outcome := range call.outcomes {
			switch outcome.Outcome {
			case ifaces.PleasePullStarted, ifaces.PleasePullAlreadyPulling:
				accepted = true
			case ifaces.PleasePullRecentlyFailed:
				if isTrustedFailureClass(outcome.FailureClass, trustedFailureClasses) {
					result.trustedFailure = true
				} else if outcome.FailureClass == ifaces.FailureTransient {
					result.transientFailure = true
				}
			}
		}

		if accepted {
			result.accepted = append(result.accepted, call.chair)
		}
	}

	return result
}

func (r *ChairResolver) PrefetchManifestChildren(ctx context.Context, _ digest.Digest, children []ChildDigest, registry, repository string) error {
	if registry == "" || repository == "" {
		return fmt.Errorf("%w: registry=%q repository=%q", ErrPrefetchInvalid, registry, repository)
	}

	if len(children) == 0 {
		return nil
	}

	apiCtx, cancel := context.WithTimeout(ctx, r.opts.APITimeout)
	snapshot, err := r.opts.Chairs.Snapshot(apiCtx, r.opts.CurrentEpoch())

	cancel()

	if err != nil {
		return err
	}

	type groupKey struct {
		peer       ifaces.NodeID
		chair      chairs.ID
		generation int64
		epoch      int64
		kind       ifaces.OriginRefKind
	}

	groups := map[groupKey][]digest.Digest{}
	holders := map[groupKey]chairs.Holder{}

	seen := map[digest.Digest]struct{}{}
	for _, child := range children {
		if _, ok := seen[child.Digest]; ok {
			continue
		}

		seen[child.Digest] = struct{}{}

		ranked := chairs.Rank(snapshot, child.Digest)
		if len(ranked) < chairs.SeedCount {
			continue
		}

		for _, chair := range ranked[:chairs.SeedCount] {
			key := groupKey{
				peer:       chair.Holder.PeerID,
				chair:      chair.ID,
				generation: chair.Generation,
				epoch:      chair.AssignmentEpoch,
				kind:       child.Kind,
			}
			groups[key] = append(groups[key], child.Digest)
			holders[key] = chair.Holder
		}
	}

	keys := make([]groupKey, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}

	sort.Slice(keys, func(i, j int) bool {
		if keys[i].chair != keys[j].chair {
			return keys[i].chair < keys[j].chair
		}

		return keys[i].kind < keys[j].kind
	})

	var (
		wg       sync.WaitGroup
		failures int
		mu       sync.Mutex
	)

	for _, key := range keys {
		key := key
		chair := chairs.Chair{
			ID:              key.chair,
			Holder:          holders[key],
			Generation:      key.generation,
			AssignmentEpoch: key.epoch,
		}
		digests := append([]digest.Digest(nil), groups[key]...)

		wg.Add(1)

		go func() {
			defer wg.Done()

			outcomes, err := r.pullChair(ctx, chair, registry, repository, key.kind, digests)
			if err != nil || !allChairOutcomesAccepted(outcomes) {
				mu.Lock()
				failures++
				mu.Unlock()
			}
		}()
	}

	wg.Wait()

	if failures > 0 {
		return fmt.Errorf("%w: %d/%d groups errored", ErrPrefetchPartial, failures, len(keys))
	}

	return nil
}

func allChairOutcomesAccepted(outcomes []ifaces.PleasePullOutcome) bool {
	if len(outcomes) == 0 {
		return false
	}

	for _, outcome := range outcomes {
		if outcome.Outcome != ifaces.PleasePullStarted && outcome.Outcome != ifaces.PleasePullAlreadyPulling {
			return false
		}
	}

	return true
}

func (r *ChairResolver) pullChair(ctx context.Context, chair chairs.Chair, registry, repository string, kind ifaces.OriginRefKind, digests []digest.Digest) ([]ifaces.PleasePullOutcome, error) {
	outcomes, err := r.pullChairOnce(ctx, chair, registry, repository, kind, digests)
	if err == nil && !containsStaleChair(outcomes) {
		return outcomes, nil
	}

	apiCtx, cancel := context.WithTimeout(ctx, r.opts.APITimeout)
	refreshed, refreshErr := r.opts.Chairs.RefreshChair(apiCtx, chair.ID)

	cancel()

	if refreshErr != nil {
		if err != nil {
			return nil, err
		}

		return outcomes, nil
	}

	if err != nil && r.opts.Claimer != nil && refreshed.Holder.PeerID == chair.Holder.PeerID {
		claimed, ok, claimErr := r.opts.Claimer.ClaimFailed(ctx, refreshed)
		if claimErr != nil {
			return nil, claimErr
		}

		if ok {
			return r.pullChairOnce(ctx, claimed, registry, repository, kind, digests)
		}

		apiCtx, cancel := context.WithTimeout(ctx, r.opts.APITimeout)
		winner, winnerErr := r.opts.Chairs.RefreshChair(apiCtx, chair.ID)

		cancel()

		if winnerErr == nil {
			return r.pullChairOnce(ctx, winner, registry, repository, kind, digests)
		}
	}

	return r.pullChairOnce(ctx, refreshed, registry, repository, kind, digests)
}

func (r *ChairResolver) pullChairOnce(ctx context.Context, chair chairs.Chair, registry, repository string, kind ifaces.OriginRefKind, digests []digest.Digest) ([]ifaces.PleasePullOutcome, error) {
	if r.opts.InstallHolder != nil {
		if err := r.opts.InstallHolder(chair.Holder); err != nil {
			return nil, err
		}
	}

	assignment := ifaces.ChairAssignment{
		ChairID:         uint32(chair.ID),
		Generation:      chair.Generation,
		AssignmentEpoch: chair.AssignmentEpoch,
	}

	callCtx, cancel := context.WithTimeout(ctx, r.opts.QueryTimeout)
	defer cancel()

	if chair.Holder.PeerID == r.opts.SelfPeerID && r.opts.LocalPull != nil {
		return r.opts.LocalPull.StartLocalChairPull(callCtx, registry, repository, kind, digests, assignment)
	}

	return r.opts.Coord.PleasePullChair(callCtx, chair.Holder.PeerID, registry, repository, kind, digests, assignment)
}

func (r *ChairResolver) pollDHT(ctx context.Context, d digest.Digest, kind ifaces.OriginRefKind, expectedSize int64) ([]ifaces.Provider, error) {
	interval := r.opts.PollLayer
	if kind == ifaces.KindManifest {
		interval = r.opts.PollManifest
	}

	deadline := time.Now().Add(r.opts.Inflight.Stalls().ResolveStall(kind, expectedSize))

	pollCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		providers, err := r.opts.Discovery.FindProviders(pollCtx, d)
		if err == nil && len(providers) > 0 {
			return providers, nil
		}

		select {
		case <-pollCtx.Done():
			return nil, ErrExhausted
		case <-ticker.C:
		}
	}
}

func containsStaleChair(outcomes []ifaces.PleasePullOutcome) bool {
	for _, outcome := range outcomes {
		if outcome.Outcome == ifaces.PleasePullStaleChair {
			return true
		}
	}

	return false
}
