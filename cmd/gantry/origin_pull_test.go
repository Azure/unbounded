// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/coord"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
	"github.com/Azure/unbounded/internal/gantry/inflight"
	"github.com/Azure/unbounded/internal/gantry/negcache"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
)

type commitOnlyCache struct{}

func (commitOnlyCache) Has(context.Context, digest.Digest) (bool, error) { return false, nil }
func (commitOnlyCache) Open(context.Context, digest.Digest) (io.ReadCloser, int64, error) {
	return nil, 0, &ifaces.ErrNotFound{}
}

func (commitOnlyCache) Writer(context.Context, digest.Digest) (ifaces.ContentWriter, error) {
	return commitOnlyWriter{}, nil
}

type commitOnlyWriter struct{}

func (commitOnlyWriter) Write(p []byte) (int, error)  { return len(p), nil }
func (commitOnlyWriter) Commit(context.Context) error { return nil }
func (commitOnlyWriter) Abort(context.Context) error  { return nil }

type contextAwareCache struct{}

func (contextAwareCache) Has(ctx context.Context, _ digest.Digest) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	return false, nil
}

func (contextAwareCache) Open(context.Context, digest.Digest) (io.ReadCloser, int64, error) {
	return nil, 0, &ifaces.ErrNotFound{}
}

func (contextAwareCache) Writer(context.Context, digest.Digest) (ifaces.ContentWriter, error) {
	return nil, errors.New("unexpected writer call")
}

type blockingOriginPuller struct {
	started chan struct{}
	release chan struct{}
}

func newBlockingOriginPuller() *blockingOriginPuller {
	return &blockingOriginPuller{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
}

func (p *blockingOriginPuller) Pull(ctx context.Context, ref ifaces.OriginRef) (io.ReadCloser, int64, error) {
	select {
	case p.started <- struct{}{}:
	default:
	}

	select {
	case <-p.release:
		body := ref.Digest.String()

		return io.NopCloser(strings.NewReader(body)), int64(len(body)), nil
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}
}

func (p *blockingOriginPuller) Head(context.Context, ifaces.OriginRef) (int64, string, error) {
	return 0, "", nil
}

type authorizationRecordingOriginPuller struct {
	body []byte
	seen chan string
}

func (p *authorizationRecordingOriginPuller) Pull(ctx context.Context, _ ifaces.OriginRef) (io.ReadCloser, int64, error) {
	p.seen <- registryauth.Authorization(ctx)

	return io.NopCloser(strings.NewReader(string(p.body))), int64(len(p.body)), nil
}

func (p *authorizationRecordingOriginPuller) Head(context.Context, ifaces.OriginRef) (int64, string, error) {
	return int64(len(p.body)), "", nil
}

func TestRunOriginPull_ReopenFailurePreventsAdvertiseAndSuccess(t *testing.T) {
	body := []byte("committed-but-not-reopenable")
	d := trackerDigestOf(body)
	originPuller := fakes.NewOriginPuller()
	originPuller.Put(d, body)
	h, _, _ := inflight.New(inflight.DefaultStalls(), nil).Start(d, ifaces.KindBlob, 0)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	var markPresent, successes, downstream int32

	runOriginPull(context.Background(), originPuller, commitOnlyCache{}, nil, logger, h, "registry.example.com", "library/test", d, ifaces.KindBlob,
		func(context.Context, digest.Digest) bool {
			atomic.AddInt32(&markPresent, 1)
			return true
		},
		func(string, int64) { atomic.AddInt32(&successes, 1) },
		func(string, string) { atomic.AddInt32(&downstream, 1) },
		leaseMetricHooks{},
	)

	if got := atomic.LoadInt32(&markPresent); got != 0 {
		t.Fatalf("markPresent calls = %d, want 0 when reopen fails", got)
	}

	if got := atomic.LoadInt32(&successes); got != 0 {
		t.Fatalf("success calls = %d, want 0 when reopen fails", got)
	}

	if got := atomic.LoadInt32(&downstream); got != 1 {
		t.Fatalf("downstream failures = %d, want 1", got)
	}
}

func TestRunOriginPull_MarkPresentFailurePreventsSuccess(t *testing.T) {
	body := []byte("commit-reopen-ok-advertise-fails")
	d := trackerDigestOf(body)
	originPuller := fakes.NewOriginPuller()
	originPuller.Put(d, body)

	cache := fakes.NewCache()
	h, _, _ := inflight.New(inflight.DefaultStalls(), nil).Start(d, ifaces.KindBlob, 0)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	var successes, downstream int32

	runOriginPull(context.Background(), originPuller, cache, nil, logger, h, "registry.example.com", "library/test", d, ifaces.KindBlob,
		func(context.Context, digest.Digest) bool { return false },
		func(string, int64) { atomic.AddInt32(&successes, 1) },
		func(string, string) { atomic.AddInt32(&downstream, 1) },
		leaseMetricHooks{},
	)

	if got := atomic.LoadInt32(&successes); got != 0 {
		t.Fatalf("success calls = %d, want 0 when mark-present fails", got)
	}

	if got := atomic.LoadInt32(&downstream); got != 1 {
		t.Fatalf("downstream failures = %d, want 1", got)
	}

	if ok, err := cache.Has(context.Background(), d); err != nil || !ok {
		t.Fatalf("cache.Has after commit = %v, %v; want true, nil", ok, err)
	}
}

func TestPullerPumpDeclinesWhenSaturated(t *testing.T) {
	d1 := trackerDigestOf([]byte("first"))
	d2 := trackerDigestOf([]byte("second"))
	originPuller := newBlockingOriginPuller()
	cache := fakes.NewCache()
	gate := newPullerPumpGate()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	pump := newPullerPump(
		inflight.New(inflight.DefaultStalls(), nil),
		originPuller,
		cache,
		nil,
		logger,
		gate,
		1,
		func(context.Context, digest.Digest) bool { return true },
		func(string, int64) {},
		func(string, string) {},
		leaseMetricHooks{},
	)

	first := pump(context.Background(), "registry.example.com", "library/test", d1, ifaces.KindBlob)
	if first.Status != coord.PumpStarted {
		t.Fatalf("first status = %v, want PumpStarted", first.Status)
	}

	select {
	case <-originPuller.started:
	case <-time.After(time.Second):
		t.Fatal("first origin pull did not start")
	}

	second := pump(context.Background(), "registry.example.com", "library/test", d2, ifaces.KindBlob)
	if second.Status != coord.PumpDeclined {
		t.Fatalf("second status = %v, want PumpDeclined", second.Status)
	}

	if ok, err := cache.Has(context.Background(), d2); err != nil || ok {
		t.Fatalf("cache.Has for declined digest = %v, %v; want false, nil", ok, err)
	}

	close(originPuller.release)
	gate.Wait()
}

func TestPullerPumpRetainsDelegatedAuthorizationForBackgroundPull(t *testing.T) {
	body := []byte("delegated background pull")
	d := trackerDigestOf(body)
	originPuller := &authorizationRecordingOriginPuller{body: body, seen: make(chan string, 1)}
	cache := fakes.NewCache()
	gate := newPullerPumpGate()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	neg := negcache.New(negcache.Options{})
	neg.RecordFailure(d, ifaces.FailureAuth)

	pump := newPullerPump(
		inflight.New(inflight.DefaultStalls(), nil),
		originPuller,
		cache,
		neg,
		logger,
		gate,
		1,
		func(context.Context, digest.Digest) bool { return true },
		func(string, int64) {},
		func(string, string) {},
		leaseMetricHooks{},
	)

	ctx := registryauth.WithAuthorization(context.Background(), "Bearer requester-token")

	res := pump(ctx, "registry.example.com", "library/test", d, ifaces.KindBlob)
	if res.Status != coord.PumpStarted {
		t.Fatalf("status = %v, want PumpStarted", res.Status)
	}

	select {
	case got := <-originPuller.seen:
		if got != "Bearer requester-token" {
			t.Fatalf("origin authorization = %q, want requester token", got)
		}
	case <-time.After(time.Second):
		t.Fatal("background origin pull did not start")
	}

	gate.Wait()

	if _, ok := neg.Lookup(d); ok {
		t.Fatal("successful delegated pull did not clear prior auth cooldown")
	}
}

func TestPullerPumpDoesNotCacheDelegatedOriginFailure(t *testing.T) {
	d := trackerDigestOf([]byte("missing delegated content"))
	originPuller := fakes.NewOriginPuller()
	cache := fakes.NewCache()
	gate := newPullerPumpGate()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	neg := negcache.New(negcache.Options{})

	pump := newPullerPump(
		inflight.New(inflight.DefaultStalls(), nil),
		originPuller,
		cache,
		neg,
		logger,
		gate,
		1,
		func(context.Context, digest.Digest) bool { return true },
		func(string, int64) {},
		func(string, string) {},
		leaseMetricHooks{},
	)

	ctx := registryauth.WithAuthorization(context.Background(), "Bearer requester-token")

	res := pump(ctx, "registry.example.com", "library/test", d, ifaces.KindBlob)
	if res.Status != coord.PumpStarted {
		t.Fatalf("status = %v, want PumpStarted", res.Status)
	}

	gate.Wait()

	if _, ok := neg.Lookup(d); ok {
		t.Fatal("delegated origin failure poisoned shared negative cache")
	}
}

// TestPullerPumpSameDigestPiggybacksWhenSaturated proves the fanout bound does
// not starve same-digest dedup: while one pull of d holds the only concurrency
// slot, a second please_pull for the SAME d must report AlreadyPulling (ride
// the in-flight pull) rather than Declined. Gating a piggybacking request on
// the concurrent-pull ceiling would wrongly shed load that starts no new work,
// and the peek-before-claim ordering must not leave a phantom inflight entry.
func TestPullerPumpSameDigestPiggybacksWhenSaturated(t *testing.T) {
	d := trackerDigestOf([]byte("popular"))
	originPuller := newBlockingOriginPuller()
	cache := fakes.NewCache()
	gate := newPullerPumpGate()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	pump := newPullerPump(
		inflight.New(inflight.DefaultStalls(), nil),
		originPuller,
		cache,
		nil,
		logger,
		gate,
		1,
		func(context.Context, digest.Digest) bool { return true },
		func(string, int64) {},
		func(string, string) {},
		leaseMetricHooks{},
	)

	first := pump(context.Background(), "registry.example.com", "library/test", d, ifaces.KindBlob)
	if first.Status != coord.PumpStarted {
		t.Fatalf("first status = %v, want PumpStarted", first.Status)
	}

	select {
	case <-originPuller.started:
	case <-time.After(time.Second):
		t.Fatal("first origin pull did not start")
	}

	// Saturated (the only slot is held by the in-flight pull of d), but a
	// second request for the SAME digest must piggyback, not be declined.
	second := pump(context.Background(), "registry.example.com", "library/test", d, ifaces.KindBlob)
	if second.Status != coord.PumpAlreadyPulling {
		t.Fatalf("second status = %v, want PumpAlreadyPulling", second.Status)
	}

	if second.StartedAt.IsZero() {
		t.Fatal("AlreadyPulling result missing StartedAt")
	}

	close(originPuller.release)
	gate.Wait()

	// The blocking puller signals started exactly once per Pull invocation
	// (buffered, cap 1). We drained the first signal above; once every pull
	// goroutine has finished, an empty buffer proves the piggybacking second
	// call spawned no new origin pull (and left no phantom inflight entry).
	if n := len(originPuller.started); n != 0 {
		t.Fatalf("a second origin pull started (started buffer = %d); want piggyback with no new pull", n)
	}
}

func TestPullerPumpDeclinesWhenContextCanceled(t *testing.T) {
	d := trackerDigestOf([]byte("canceled"))
	originPuller := fakes.NewOriginPuller()
	cache := contextAwareCache{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	var starts int32

	pump := newPullerPump(
		inflight.New(inflight.DefaultStalls(), nil),
		originPuller,
		cache,
		nil,
		logger,
		nil,
		1,
		func(context.Context, digest.Digest) bool { return true },
		func(string, int64) { atomic.AddInt32(&starts, 1) },
		func(string, string) {},
		leaseMetricHooks{},
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := pump(ctx, "registry.example.com", "library/test", d, ifaces.KindBlob)
	if res.Status != coord.PumpDeclined {
		t.Fatalf("status = %v, want PumpDeclined", res.Status)
	}

	if got := atomic.LoadInt32(&starts); got != 0 {
		t.Fatalf("origin pulls started after canceled context = %d, want 0", got)
	}
}

func TestPullerPumpStopsAcceptingBeforeWait(t *testing.T) {
	body := []byte("late-please-pull")
	d := trackerDigestOf(body)
	originPuller := fakes.NewOriginPuller()
	originPuller.Put(d, body)

	cache := fakes.NewCache()
	gate := newPullerPumpGate()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	var starts int32

	pump := newPullerPump(
		inflight.New(inflight.DefaultStalls(), nil),
		originPuller,
		cache,
		nil,
		logger,
		gate,
		1,
		func(context.Context, digest.Digest) bool { return true },
		func(string, int64) { atomic.AddInt32(&starts, 1) },
		func(string, string) {},
		leaseMetricHooks{},
	)

	gate.StopAccepting()
	gate.Wait()

	res := pump(context.Background(), "registry.example.com", "library/test", d, ifaces.KindBlob)
	if res.Status != coord.PumpDeclined {
		t.Fatalf("status = %v, want PumpDeclined after gate closed", res.Status)
	}

	gate.Wait()

	if got := atomic.LoadInt32(&starts); got != 0 {
		t.Fatalf("origin pulls started after gate closed = %d, want 0", got)
	}

	if ok, err := cache.Has(context.Background(), d); err != nil || ok {
		t.Fatalf("cache.Has after declined pump = %v, %v; want false, nil", ok, err)
	}
}

// TestPullerPumpDeclinesLateCallWhileShutdownWaits exercises the original race
// the gate was added to close: one origin pull is in flight (gate counter > 0),
// graceful shutdown calls StopAccepting then parks in Wait, and a late
// please_pull lands while Wait is still blocked. The late call must be declined
// without starting new work and without panicking, and Wait must return only
// after the in-flight pull releases its handle.
func TestPullerPumpDeclinesLateCallWhileShutdownWaits(t *testing.T) {
	body := []byte("late-please-pull-during-wait")
	d := trackerDigestOf(body)
	originPuller := fakes.NewOriginPuller()
	originPuller.Put(d, body)

	cache := fakes.NewCache()
	gate := newPullerPumpGate()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	var starts int32

	pump := newPullerPump(
		inflight.New(inflight.DefaultStalls(), nil),
		originPuller,
		cache,
		nil,
		logger,
		gate,
		1,
		func(context.Context, digest.Digest) bool { return true },
		func(string, int64) { atomic.AddInt32(&starts, 1) },
		func(string, string) {},
		leaseMetricHooks{},
	)

	// Simulate one origin pull already in flight: the counter is > 0, exactly
	// as it is after a please_pull spawned runOriginPull but before that
	// goroutine called Done.
	if !gate.TryAdd() {
		t.Fatal("TryAdd on a fresh gate = false, want true")
	}

	// Shutdown stops accepting, signals once that happened, then parks in Wait
	// until the in-flight pull releases. Gating the late call on stopped keeps
	// the test deterministic while still modeling shutdown's StopAccepting+Wait
	// ordering (StopAccepting always precedes Wait in gracefulShutdown).
	stopped := make(chan struct{})
	waitReturned := make(chan struct{})

	go func() {
		gate.StopAccepting()
		close(stopped)
		gate.Wait()
		close(waitReturned)
	}()

	<-stopped

	// The late please_pull arrives while Wait is blocked on the in-flight pull.
	res := pump(context.Background(), "registry.example.com", "library/test", d, ifaces.KindBlob)
	if res.Status != coord.PumpDeclined {
		t.Fatalf("late pump status = %v, want PumpDeclined while shutting down", res.Status)
	}

	// Releasing the in-flight pull is the only thing that may unblock Wait; if
	// the late call had leaked a counter this would never close.
	gate.Done()
	<-waitReturned

	if got := atomic.LoadInt32(&starts); got != 0 {
		t.Fatalf("origin pulls started after gate closed = %d, want 0", got)
	}

	if ok, err := cache.Has(context.Background(), d); err != nil || ok {
		t.Fatalf("cache.Has after declined pump = %v, %v; want false, nil", ok, err)
	}
}
