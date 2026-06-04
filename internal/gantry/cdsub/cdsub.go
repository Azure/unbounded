// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package cdsub subscribes to containerd image/content events and forwards
// digest presence changes to its caller.
//
// Design choices:
//
// - The containerd RPC client is heavy and platform-bound (Linux + a
// containerd socket on the node). To keep the announce-loop testable
// on darwin/CI without a real containerd, the package depends on a
// small ImageSource interface. The concrete containerd implementation
// lives in internal/cdsub/source_containerd.go behind a Linux build
// tag. Tests in this file exercise the loop against a fake source.
//
// - In containerd mode, callers wire WithNotifier to route every
// Image/Create, Image/Update, and content-delete digest into the
// advertiser. The advertiser owns the DHT announced set, verifies
// local serveability, and performs best-effort Withdraw.
//
// - When no notifier is supplied, cdsub preserves the older direct
// dht.Provide path for tests and legacy experiments. Delete events
// remain a no-op in that compatibility mode.
//
// - Exponential-backoff reconnect with the cap (max 30 s). On
// every successful reconnect the loop calls ImageSource.List(ctx)
// and re-Provides every digest, closing the window where in-flight
// events were missed during the disconnect.
package cdsub

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

// ImageEventKind discriminates the three event types cdsub cares about.
type ImageEventKind int

const (
	// EventCreate is an Image/Create - a new image landed.
	EventCreate ImageEventKind = iota
	// EventUpdate is an Image/Update - an existing image's target changed.
	EventUpdate
	// EventDelete is an Image/Delete - image removed.
	EventDelete
)

// ImageEvent is the source-agnostic representation of one containerd event.
type ImageEvent struct {
	Kind     ImageEventKind
	Registry string // host portion of the image reference, e.g. "registry.example.com"
	Image    string // full image reference, kept for logging
	Digests  []digest.Digest
}

// ImageSource is the abstraction over containerd's image-events API.
// Implementations stream events to the returned channel and close it when
// the underlying connection is lost; the loop then reconnects.
type ImageSource interface {
	// List returns every image currently present in containerd, filtered
	// to the configured upstream registries. Used on startup and after
	// every successful reconnect for reconciliation.
	List(ctx context.Context) ([]ImageEvent, error)

	// Subscribe streams ImageEvents for the lifetime of the returned
	// context. Closing the channel signals "disconnected - caller should
	// reconnect after backoff". Subscribe MUST exit cleanly when ctx is
	// cancelled.
	Subscribe(ctx context.Context) (<-chan ImageEvent, error)
}

// Subscriber walks the announce loop: List -> Subscribe -> reconnect on error.
type Subscriber struct {
	src    ImageSource
	dht    ifaces.DHT
	notify func(ctx context.Context, d digest.Digest, present bool)
	logger *slog.Logger

	provideTimeout time.Duration
	backoffInitial time.Duration
	backoffMax     time.Duration

	metrics metricsHooks

	closeOnce sync.Once
	closed    chan struct{}
}

type metricsHooks struct {
	onAnnounce      func()
	onAnnounceError func()
	onReconcile     func(count int)
	onReconnect     func()
}

// Option configures a Subscriber.
type Option func(*Subscriber)

// WithLogger plumbs a structured logger.
func WithLogger(l *slog.Logger) Option {
	return func(s *Subscriber) {
		if l != nil {
			s.logger = l.With(slog.String("subsystem", "cdsub"))
		}
	}
}

// WithProvideTimeout caps per-Provide RPC budget.
func WithProvideTimeout(d time.Duration) Option {
	return func(s *Subscriber) {
		if d > 0 {
			s.provideTimeout = d
		}
	}
}

// WithBackoff overrides the exponential reconnect backoff bounds.
func WithBackoff(initial, maxBackoff time.Duration) Option {
	return func(s *Subscriber) {
		if initial > 0 {
			s.backoffInitial = initial
		}

		if maxBackoff > 0 {
			s.backoffMax = maxBackoff
		}
	}
}

// WithMetrics registers metric callbacks. Any callback may be nil.
func WithMetrics(onAnnounce, onAnnounceError func(), onReconcile func(int), onReconnect func()) Option {
	return func(s *Subscriber) {
		s.metrics.onAnnounce = onAnnounce
		s.metrics.onAnnounceError = onAnnounceError
		s.metrics.onReconcile = onReconcile
		s.metrics.onReconnect = onReconnect
	}
}

// WithNotifier routes digest presence changes to a central owner such as
// internal/advertise.Advertiser. When set, cdsub does not call DHT.Provide
// directly. present=true corresponds to create/update/list observations;
// present=false corresponds to delete observations.
func WithNotifier(fn func(ctx context.Context, d digest.Digest, present bool)) Option {
	return func(s *Subscriber) {
		s.notify = fn
	}
}

// New builds a Subscriber. Run drives the loop until ctx is cancelled.
// dht may be nil when WithNotifier is supplied.
func New(src ImageSource, dht ifaces.DHT, opts ...Option) *Subscriber {
	s := &Subscriber{
		src:            src,
		dht:            dht,
		logger:         slog.Default().With(slog.String("subsystem", "cdsub")),
		provideTimeout: 30 * time.Second,
		backoffInitial: time.Second,
		backoffMax:     30 * time.Second,
		closed:         make(chan struct{}),
	}
	for _, opt := range opts {
		opt(s)
	}

	return s
}

// Run blocks until ctx is cancelled. On each iteration:
//
// 1. Run reconciliation: List -> notify/provide every digest.
// 2. Subscribe and process events as they arrive.
// 3. On Subscribe error, channel close, or any non-context error,
// sleep with jittered exponential backoff and retry.
func (s *Subscriber) Run(ctx context.Context) error {
	backoff := s.backoffInitial

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		if err := s.runOnce(ctx); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}

			s.logger.Warn("cdsub: stream lost, backing off",
				slog.Duration("backoff", backoff),
				slog.Any("err", err),
			)
		} else {
			// Channel closed without error -> just reconnect with reset backoff.
			backoff = s.backoffInitial
			continue
		}

		// Jittered sleep.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(jitter(backoff)):
		}
		// Exponential growth, capped.
		backoff *= 2
		if backoff > s.backoffMax {
			backoff = s.backoffMax
		}
	}
}

// Close releases any subscriber-owned resources. Safe to call multiple times.
func (s *Subscriber) Close() {
	s.closeOnce.Do(func() { close(s.closed) })
}

func (s *Subscriber) runOnce(ctx context.Context) error {
	// 1. Reconciliation.
	if events, err := s.src.List(ctx); err == nil {
		s.reconcile(ctx, events)
	} else {
		return err
	}

	if s.metrics.onReconnect != nil {
		s.metrics.onReconnect()
	}

	// 2. Subscribe.
	ch, err := s.src.Subscribe(ctx)
	if err != nil {
		return err
	}

	// 3. Drain the channel.
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				return nil
			}

			s.handle(ctx, ev)
		}
	}
}

// reconcile is called once per successful (re)connect. It announces every
// digest the local containerd currently has via the configured notifier
// or compatibility DHT provider path, then bumps the reconcile metric.
func (s *Subscriber) reconcile(ctx context.Context, events []ImageEvent) {
	count := 0

	for _, ev := range events {
		for _, d := range ev.Digests {
			if s.announce(ctx, d, true) {
				count++
			}
		}
	}

	if s.metrics.onReconcile != nil {
		s.metrics.onReconcile(count)
	}

	s.logger.Info("cdsub: reconcile complete", slog.Int("digests", count))
}

func (s *Subscriber) handle(ctx context.Context, ev ImageEvent) {
	switch ev.Kind {
	case EventCreate, EventUpdate:
		for _, d := range ev.Digests {
			s.announce(ctx, d, true)
		}
	case EventDelete:
		for _, d := range ev.Digests {
			s.announce(ctx, d, false)
		}
	}
}

func (s *Subscriber) announce(ctx context.Context, d digest.Digest, present bool) bool {
	if s.notify != nil {
		s.notify(ctx, d, present)

		if s.metrics.onAnnounce != nil {
			s.metrics.onAnnounce()
		}

		return true
	}

	if present {
		return s.provide(ctx, d)
	}

	s.logger.Debug("cdsub: image deleted; no notifier wired, relying on DHT TTL expiry",
		slog.String("digest", d.String()),
	)

	return false
}

// provide is the single-digest announce path. Returns true on success.
func (s *Subscriber) provide(ctx context.Context, d digest.Digest) bool {
	if s.dht == nil {
		if s.metrics.onAnnounceError != nil {
			s.metrics.onAnnounceError()
		}

		s.logger.Debug("cdsub: no DHT provider wired", slog.String("digest", d.String()))

		return false
	}

	pctx, cancel := context.WithTimeout(ctx, s.provideTimeout)
	defer cancel()

	if err := s.dht.Provide(pctx, d); err != nil {
		if s.metrics.onAnnounceError != nil {
			s.metrics.onAnnounceError()
		}

		s.logger.Debug("cdsub: provide failed",
			slog.String("digest", d.String()),
			slog.Any("err", err),
		)

		return false
	}

	if s.metrics.onAnnounce != nil {
		s.metrics.onAnnounce()
	}

	return true
}

// jitter applies ±25% jitter to a duration.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}

	span := int64(d) / 2
	if span <= 0 {
		return d
	}

	delta := rand.Int64N(span) - span/2 //nolint:gosec // jitter, not crypto

	return d + time.Duration(delta)
}
