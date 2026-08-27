// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rendezvous

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
)

// BootstrapMetrics records bootstrap behavior.
type BootstrapMetrics struct {
	OnDial              func(outcome, source string)
	OnBootstrapDuration func(time.Duration)
	OnPeerCacheEntries  func(int)
}

// DialResult reports unique peer dials after parsing and bounded selection.
type DialResult struct {
	Attempted int
	Connected int
}

// BootstrapOptions configures the retrying first-contact loop.
type BootstrapOptions struct {
	Manager          *Manager
	PeerID           peer.ID
	Connect          func(context.Context, []string) DialResult
	RoutingTableSize func() int
	SelfTest         func(context.Context) bool
	SingleNode       bool
	RetryMin         time.Duration
	RetryMax         time.Duration
	RenewInterval    time.Duration
	PeerCachePath    string
	Logger           *slog.Logger
	Metrics          BootstrapMetrics
}

// Bootstrap keeps Lease publication alive and retries bounded contact reads
// until the local DHT has a peer. Established DHT operation does not depend on
// subsequent Kubernetes API availability.
type Bootstrap struct {
	opts BootstrapOptions

	ready     chan struct{}
	readyOnce sync.Once
	joined    atomic.Bool
}

// NewBootstrap constructs a bootstrap loop.
func NewBootstrap(opts BootstrapOptions) (*Bootstrap, error) {
	if opts.Connect == nil || opts.RoutingTableSize == nil {
		return nil, errors.New("rendezvous: connect and routing table callbacks required")
	}

	if opts.Manager == nil && !opts.SingleNode {
		return nil, errors.New("rendezvous: manager required in clustered mode")
	}

	if opts.SelfTest == nil && !opts.SingleNode {
		return nil, errors.New("rendezvous: DHT self-test required in clustered mode")
	}

	if opts.PeerID == "" {
		return nil, errors.New("rendezvous: bootstrap peer ID required")
	}

	if opts.RetryMin <= 0 || opts.RetryMax < opts.RetryMin {
		return nil, errors.New("rendezvous: invalid bootstrap retry bounds")
	}

	if opts.RenewInterval <= 0 {
		return nil, errors.New("rendezvous: renew interval must be positive")
	}

	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	return &Bootstrap{opts: opts, ready: make(chan struct{})}, nil
}

// Run blocks until ctx is canceled. Call it in a goroutine.
func (b *Bootstrap) Run(ctx context.Context) {
	started := time.Now()
	if b.opts.SingleNode {
		b.markReady(started)
	}

	if b.opts.Manager == nil {
		<-ctx.Done()

		return
	}

	go b.renewLoop(ctx)

	if b.tryReady(ctx, started) {
		b.claimUntilSettled(ctx)
		<-ctx.Done()

		return
	}

	initialDelay := deterministicDelay(b.opts.PeerID, b.opts.RetryMin)
	if !wait(ctx, initialDelay) {
		return
	}

	if cached, err := readPeerCache(b.opts.PeerCachePath); err != nil {
		b.opts.Logger.Warn("rendezvous peer cache read failed", slog.Any("err", err))
	} else {
		if b.opts.Metrics.OnPeerCacheEntries != nil {
			b.opts.Metrics.OnPeerCacheEntries(len(cached))
		}

		if len(cached) > 0 {
			result := b.opts.Connect(ctx, cached)
			b.recordDials(result, "cache")
			b.logDialPass("cache", len(cached), result)

			if result.Connected > 0 || b.opts.RoutingTableSize() > 0 {
				if b.tryReady(ctx, started) {
					b.claimUntilSettled(ctx)
					<-ctx.Done()

					return
				}
			}
		}
	}

	backoff := b.opts.RetryMin
	for round := uint64(0); ; round++ {
		contacts, err := b.opts.Manager.ReadContacts(ctx, round)
		if err != nil {
			b.opts.Logger.Debug("rendezvous contact read incomplete",
				slog.String("operation", "read"),
				slog.String("outcome", metricOutcome(err)),
				slog.Uint64("round", round),
				slog.Int("contacts_considered", len(contacts)),
				slog.Int("routing_table", b.opts.RoutingTableSize()),
				slog.Any("err", err),
			)
		}

		addresses := contactAddresses(contacts)
		if len(addresses) > 0 {
			result := b.opts.Connect(ctx, addresses)
			b.recordDials(result, "lease")
			b.logDialPass("lease", len(contacts), result)

			if result.Connected > 0 {
				if err := writePeerCache(b.opts.PeerCachePath, addresses); err != nil {
					b.opts.Logger.Warn("rendezvous peer cache write failed", slog.Any("err", err))
				}
			}
		}

		if b.tryReady(ctx, started) {
			b.claimUntilSettled(ctx)
			<-ctx.Done()

			return
		}

		b.claimOnce(ctx)

		if !wait(ctx, jitteredDelay(b.opts.PeerID, round, backoff)) {
			return
		}

		backoff *= 2
		if backoff > b.opts.RetryMax {
			backoff = b.opts.RetryMax
		}
	}
}

// Ready closes once clustered mode has a DHT peer or single-node mode starts.
func (b *Bootstrap) Ready() <-chan struct{} { return b.ready }

// IsReady reports whether the bootstrap policy has succeeded.
func (b *Bootstrap) IsReady() bool { return b.joined.Load() }

func (b *Bootstrap) renewLoop(ctx context.Context) {
	ticker := time.NewTicker(b.opts.RenewInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			slot := b.opts.Manager.HeldSlot()
			if slot == "" {
				continue
			}

			if err := b.opts.Manager.Renew(ctx); err != nil {
				b.opts.Logger.Warn("rendezvous slot renewal failed",
					slog.String("operation", "renew"),
					slog.String("outcome", metricOutcome(err)),
					slog.String("slot", slot),
					slog.Int("routing_table", b.opts.RoutingTableSize()),
					slog.Any("err", err),
				)
			}
		}
	}
}

func (b *Bootstrap) markReady(started time.Time) {
	b.readyOnce.Do(func() {
		b.joined.Store(true)
		close(b.ready)

		if b.opts.Metrics.OnBootstrapDuration != nil {
			b.opts.Metrics.OnBootstrapDuration(time.Since(started))
		}
	})
}

func (b *Bootstrap) tryReady(ctx context.Context, started time.Time) bool {
	if b.IsReady() {
		return true
	}

	if b.opts.RoutingTableSize() < 1 {
		return false
	}

	if b.opts.SelfTest != nil {
		testCtx, cancel := context.WithTimeout(ctx, b.opts.RetryMax)
		passed := b.opts.SelfTest(testCtx)

		cancel()

		if !passed {
			b.opts.Logger.Debug("rendezvous DHT self-test failed",
				slog.String("operation", "self_test"),
				slog.String("outcome", "failure"),
				slog.Int("routing_table", b.opts.RoutingTableSize()),
			)

			return false
		}
	}

	b.markReady(started)

	return true
}

func (b *Bootstrap) claimUntilSettled(ctx context.Context) {
	backoff := b.opts.RetryMin

	for round := uint64(0); ; round++ {
		if b.claimOnce(ctx) {
			return
		}

		if !wait(ctx, jitteredDelay(b.opts.PeerID, round, backoff)) {
			return
		}

		backoff *= 2
		if backoff > b.opts.RetryMax {
			backoff = b.opts.RetryMax
		}
	}
}

// claimOnce returns true after a complete bounded pass, including when all
// candidates are occupied. Only transport/API failures require another pass.
func (b *Bootstrap) claimOnce(ctx context.Context) bool {
	if b.opts.Manager.HeldSlot() != "" {
		return true
	}

	slot, err := b.opts.Manager.Claim(ctx)
	if err != nil {
		b.opts.Logger.Warn("rendezvous slot claim failed",
			slog.String("operation", "claim"),
			slog.String("outcome", metricOutcome(err)),
			slog.Int("routing_table", b.opts.RoutingTableSize()),
			slog.Any("err", err),
		)

		return false
	}

	if slot != "" {
		b.opts.Logger.Info("rendezvous slot held",
			slog.String("operation", "claim"),
			slog.String("outcome", "success"),
			slog.String("slot", slot),
			slog.Int("routing_table", b.opts.RoutingTableSize()),
		)
	}

	return true
}

func (b *Bootstrap) logDialPass(source string, contacts int, result DialResult) {
	b.opts.Logger.Debug("rendezvous dial pass",
		slog.String("operation", "dial"),
		slog.String("source", source),
		slog.Int("contacts_considered", contacts),
		slog.Int("attempted", result.Attempted),
		slog.Int("connected", result.Connected),
		slog.Int("routing_table", b.opts.RoutingTableSize()),
	)
}

func (b *Bootstrap) recordDials(result DialResult, source string) {
	if b.opts.Metrics.OnDial == nil {
		return
	}

	for range result.Connected {
		b.opts.Metrics.OnDial("success", source)
	}

	for range max(0, result.Attempted-result.Connected) {
		b.opts.Metrics.OnDial("failure", source)
	}
}

func contactAddresses(contacts []Contact) []string {
	result := make([]string, 0, len(contacts))
	seen := make(map[string]struct{})

	for _, contact := range contacts {
		peerComponent := multiaddr.StringCast("/p2p/" + contact.Info.ID.String())
		for _, addr := range contact.Info.Addrs {
			full := addr.Encapsulate(peerComponent).String()
			if _, ok := seen[full]; ok {
				continue
			}

			seen[full] = struct{}{}
			result = append(result, full)
		}
	}

	return result
}

func deterministicDelay(id peer.ID, maximum time.Duration) time.Duration {
	hash := sha256.Sum256([]byte(id))
	value := binary.BigEndian.Uint64(hash[:8])

	return time.Duration(value % uint64(maximum))
}

func jitteredDelay(id peer.ID, round uint64, base time.Duration) time.Duration {
	var nonce [8]byte
	binary.BigEndian.PutUint64(nonce[:], round)
	hash := sha256.Sum256(append([]byte(id), nonce[:]...))
	value := binary.BigEndian.Uint64(hash[:8])

	half := base / 2
	if half == 0 {
		return base
	}

	return half + time.Duration(value%uint64(half))
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
