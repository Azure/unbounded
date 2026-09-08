// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package advertise reconciles the local containerd content store
// against the set of digests this node is advertising on the DHT.
//
// Contract :
//
// - The local containerd content store is the source of truth for
// "what we can serve to peers".
// - The DHT is a hint layer: provider records say "this node might
// have this digest", subject to ≤24 h TTL and eventual consistency.
// - This package owns the local "announced set" - the digests we
// currently believe are present-and-advertised - and reconciles it
// against an inventory source (typically containerdstore.Inventory)
// plus an event stream (cdsub.ImageEvent).
//
// Reconciliation passes call inventory.Inventory(ctx), diff against the
// announced set, and for each delta:
//
// - present-but-not-announced -> DHT.Provide + add to announced set.
// - announced-but-absent -> DHT.Withdraw + remove from announced set.
//
// Withdraw is a soft hint per libp2p has no protocol-level
// withdraw and the existing record will drain via TTL. The advertiser
// simply stops calling Provide for the digest so refresh ticks no
// longer keep it alive.
//
// The announced set is local rebuildable state - it is NOT persisted
// across process restarts. On startup the first reconcile pass
// re-Provides every present digest, which is the same operation
// libp2p performs internally on its 12 h refresh schedule, so the
// extra cost is bounded by inventory size.
package advertise

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

// Inventory is the interface the advertiser uses to discover what
// digests are currently present in the local content store. The
// canonical implementation is containerdstore.Store.Inventory.
type Inventory interface {
	Inventory(ctx context.Context) ([]digest.Digest, error)
}

// Openable is implemented by stores that can prove a digest is locally
// serveable. The containerdstore.Store implements this through Open.
type Openable interface {
	Open(ctx context.Context, d digest.Digest) (io.ReadCloser, int64, error)
}

// MetricsHooks are optional Prometheus shims. Any callback may be nil.
type MetricsHooks struct {
	// OnReconcileStart fires when a reconcile pass begins.
	OnReconcileStart func()
	// OnReconcileEnd fires after each reconcile pass with the elapsed
	// duration, the total inventory size, and the count of digests
	// added/removed from the announced set.
	OnReconcileEnd func(dur time.Duration, inventorySize, added, removed int)
	// OnReconcileError fires when a reconcile pass aborts on Inventory error.
	OnReconcileError func(err error)
	// OnReconcileUnavailable fires when a reconcile pass aborts
	// because the inventory backend (containerd) is unavailable.
	// Distinct from OnReconcileError so dashboards can separate
	// "real error" from "backend hiccup that we deliberately
	// tolerated by preserving the announced set". Per plan
	// "containerd unavailable pauses advertise/reconcile
	// rather than treating everything as absent".
	OnReconcileUnavailable func()
	// OnProvide fires after each successful DHT.Provide call.
	OnProvide func()
	// OnProvideError fires after each failed DHT.Provide call.
	OnProvideError func()
	// OnWithdraw fires after each successful DHT.Withdraw call.
	OnWithdraw func()
	// OnWithdrawError fires after each failed DHT.Withdraw call.
	OnWithdrawError func()
}

// Option configures an Advertiser.
type Option func(*Advertiser)

// WithLogger plumbs a structured logger.
func WithLogger(l *slog.Logger) Option {
	return func(a *Advertiser) {
		if l != nil {
			a.logger = l.With(slog.String("subsystem", "advertise"))
		}
	}
}

// WithReconcileInterval sets the cadence between full reconcile passes.
// Zero or negative leaves the default (5 minutes).
func WithReconcileInterval(d time.Duration) Option {
	return func(a *Advertiser) {
		if d > 0 {
			a.reconcileInterval = d
		}
	}
}

// WithProvideTimeout caps the per-Provide RPC budget. Zero or negative
// leaves the default (30 seconds).
func WithProvideTimeout(d time.Duration) Option {
	return func(a *Advertiser) {
		if d > 0 {
			a.provideTimeout = d
		}
	}
}

// WithWithdrawTimeout caps the per-Withdraw RPC budget. Zero or
// negative leaves the default (10 seconds).
func WithWithdrawTimeout(d time.Duration) Option {
	return func(a *Advertiser) {
		if d > 0 {
			a.withdrawTimeout = d
		}
	}
}

// WithMetrics registers metric callbacks. The Hooks struct is copied
// by value so subsequent mutations by the caller do not affect the
// running advertiser.
func WithMetrics(h MetricsHooks) Option {
	return func(a *Advertiser) { a.metrics = h }
}

// Advertiser keeps DHT provider records in sync with the local content
// store. Methods are safe for concurrent use.
type Advertiser struct {
	inv    Inventory
	open   Openable
	dht    ifaces.DHT
	logger *slog.Logger

	reconcileInterval time.Duration
	provideTimeout    time.Duration
	withdrawTimeout   time.Duration

	metrics MetricsHooks

	mu        sync.Mutex
	announced map[string]struct{} // digest.String() set
}

// New constructs an Advertiser. inv supplies the current local content
// inventory; dht is the announce target.
func New(inv Inventory, dht ifaces.DHT, opts ...Option) *Advertiser {
	a := &Advertiser{
		inv:               inv,
		dht:               dht,
		logger:            slog.Default().With(slog.String("subsystem", "advertise")),
		reconcileInterval: 5 * time.Minute,
		provideTimeout:    30 * time.Second,
		withdrawTimeout:   10 * time.Second,
		announced:         map[string]struct{}{},
	}
	if open, ok := inv.(Openable); ok {
		a.open = open
	}

	for _, opt := range opts {
		opt(a)
	}

	return a
}

// Run drives the reconcile loop until ctx is canceled. Triggers one
// reconcile pass immediately so startup converges fast, then ticks at
// the configured interval with ±25% jitter to spread cluster-wide load.
func (a *Advertiser) Run(ctx context.Context) error {
	// Initial pass: catch up before the first tick.
	if err := a.Reconcile(ctx); err != nil && !errors.Is(err, context.Canceled) {
		a.logger.Warn("advertise: initial reconcile failed", slog.Any("err", err))
	}

	ticker := time.NewTicker(a.reconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}

		if err := a.Reconcile(ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}

			a.logger.Warn("advertise: reconcile failed", slog.Any("err", err))
		}
	}
}

// Reconcile performs a single pass: snapshot the current inventory,
// Provide digests that newly appeared, Withdraw digests that
// disappeared. Returns the Inventory error if the snapshot itself
// fails (Provide/Withdraw failures are logged but do not abort the
// pass - partial progress is better than no progress).
func (a *Advertiser) Reconcile(ctx context.Context) error {
	if a.metrics.OnReconcileStart != nil {
		a.metrics.OnReconcileStart()
	}

	start := time.Now()

	digests, err := a.inv.Inventory(ctx)
	if err != nil {
		// Plan "containerd unavailable pauses
		// advertise/reconcile rather than treating everything as
		// absent". If the inventory backend reports
		// ifaces.ErrUnavailable we MUST NOT diff against an empty
		// snapshot - that would Withdraw every previously-announced
		// digest just because containerd's socket hiccuped. Bail
		// out early; the next reconcile tick retries.
		var eun *ifaces.ErrUnavailable
		if errors.As(err, &eun) {
			if a.metrics.OnReconcileUnavailable != nil {
				a.metrics.OnReconcileUnavailable()
			}

			a.logger.Warn("advertise: inventory unavailable; pausing reconcile (announced set preserved)",
				slog.Any("err", err),
			)

			return err
		}

		if a.metrics.OnReconcileError != nil {
			a.metrics.OnReconcileError(err)
		}

		return err
	}

	wantSet := make(map[string]struct{}, len(digests))
	for _, d := range digests {
		wantSet[d.String()] = struct{}{}
	}

	// Compute deltas under lock. We hold the lock only long enough to
	// snapshot the diff so Provide/Withdraw calls (which may block on
	// network I/O) happen lock-free.
	a.mu.Lock()
	toProvide := make([]digest.Digest, 0)

	for _, d := range digests {
		if _, ok := a.announced[d.String()]; !ok {
			toProvide = append(toProvide, d)
		}
	}

	toWithdraw := make([]digest.Digest, 0)

	for s := range a.announced {
		if _, ok := wantSet[s]; !ok {
			// Reconstruct the typed digest from the stored string.
			d, parseErr := digest.Parse(s)
			if parseErr != nil {
				// Shouldn't happen - we only ever store parsed
				// digests - but if it does we still drop the
				// stale entry from the announced set below.
				delete(a.announced, s)
				continue
			}

			toWithdraw = append(toWithdraw, d)
		}
	}
	a.mu.Unlock()

	// Deterministic order keeps logs/tests stable.
	sort.Slice(toProvide, func(i, j int) bool { return toProvide[i].String() < toProvide[j].String() })
	sort.Slice(toWithdraw, func(i, j int) bool { return toWithdraw[i].String() < toWithdraw[j].String() })

	added := 0

	for _, d := range toProvide {
		if a.provide(ctx, d) {
			a.mu.Lock()
			a.announced[d.String()] = struct{}{}
			a.mu.Unlock()

			added++
		}
	}

	removed := 0

	for _, d := range toWithdraw {
		if a.withdraw(ctx, d) {
			a.mu.Lock()
			delete(a.announced, d.String())
			a.mu.Unlock()

			removed++
		}
	}

	if a.metrics.OnReconcileEnd != nil {
		a.metrics.OnReconcileEnd(time.Since(start), len(digests), added, removed)
	}

	a.logger.Debug("advertise: reconcile complete",
		slog.Int("inventory", len(digests)),
		slog.Int("added", added),
		slog.Int("removed", removed),
		slog.Duration("dur", time.Since(start)),
	)

	return nil
}

// Notify is a fast-path hook for the cdsub event loop: a single digest
// event ("we just observed an image/content change for d") translates
// to either Provide (present=true) or Withdraw (present=false). Used
// to react to image-lifecycle events between reconcile ticks without
// waiting for the next full inventory pass.
func (a *Advertiser) Notify(ctx context.Context, d digest.Digest, present bool) bool {
	if present {
		if !a.openable(ctx, d) {
			return false
		}

		if a.provide(ctx, d) {
			a.mu.Lock()
			a.announced[d.String()] = struct{}{}
			a.mu.Unlock()

			return true
		}

		return false
	}

	if a.withdraw(ctx, d) {
		a.mu.Lock()
		delete(a.announced, d.String())
		a.mu.Unlock()

		return true
	}

	return false
}

func (a *Advertiser) openable(ctx context.Context, d digest.Digest) bool {
	if a.open == nil {
		return true
	}

	rc, _, err := a.open.Open(ctx, d)
	if err == nil {
		_ = rc.Close() //nolint:errcheck // best-effort close
		return true
	}

	var unavailable *ifaces.ErrUnavailable
	if errors.As(err, &unavailable) {
		if a.metrics.OnReconcileUnavailable != nil {
			a.metrics.OnReconcileUnavailable()
		}

		a.logger.Warn("advertise: notify skipped because storage is unavailable",
			slog.String("digest", d.String()),
			slog.Any("err", err),
		)

		return false
	}

	a.logger.Debug("advertise: notify skipped because digest is not openable",
		slog.String("digest", d.String()),
		slog.Any("err", err),
	)

	return false
}

// AnnouncedSize returns the current size of the announced set. Used
// by readiness/observability probes; not by the reconcile path itself.
func (a *Advertiser) AnnouncedSize() int {
	a.mu.Lock()
	defer a.mu.Unlock()

	return len(a.announced)
}

// IsAnnounced reports whether d is currently in the announced set.
// Helper for tests and observability probes.
func (a *Advertiser) IsAnnounced(d digest.Digest) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	_, ok := a.announced[d.String()]

	return ok
}

func (a *Advertiser) provide(ctx context.Context, d digest.Digest) bool {
	pctx, cancel := context.WithTimeout(ctx, a.provideTimeout)
	defer cancel()

	if err := a.dht.Provide(pctx, d); err != nil {
		if a.metrics.OnProvideError != nil {
			a.metrics.OnProvideError()
		}

		a.logger.Debug("advertise: provide failed",
			slog.String("digest", d.String()),
			slog.Any("err", err),
		)

		return false
	}

	if a.metrics.OnProvide != nil {
		a.metrics.OnProvide()
	}

	return true
}

func (a *Advertiser) withdraw(ctx context.Context, d digest.Digest) bool {
	wctx, cancel := context.WithTimeout(ctx, a.withdrawTimeout)
	defer cancel()

	if err := a.dht.Withdraw(wctx, d); err != nil {
		if a.metrics.OnWithdrawError != nil {
			a.metrics.OnWithdrawError()
		}

		a.logger.Debug("advertise: withdraw failed",
			slog.String("digest", d.String()),
			slog.Any("err", err),
		)

		return false
	}

	if a.metrics.OnWithdraw != nil {
		a.metrics.OnWithdraw()
	}

	return true
}
