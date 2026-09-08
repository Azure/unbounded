// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package advertise

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
)

// mockInventory returns whatever digests + error were last programmed.
// Tracks how many times Inventory was called so tests can assert
// reconcile cadence.
type mockInventory struct {
	mu      sync.Mutex
	digests []digest.Digest
	err     error
	calls   int32
}

type mockOpenInventory struct {
	mockInventory
	open map[string]error
}

func (m *mockOpenInventory) Open(_ context.Context, d digest.Digest) (io.ReadCloser, int64, error) {
	if m.open != nil {
		if err, ok := m.open[d.String()]; ok {
			if err != nil {
				return nil, 0, err
			}

			return io.NopCloser(bytes.NewReader([]byte("ok"))), 2, nil
		}
	}

	return nil, 0, &ifaces.ErrNotFound{Digest: d}
}

func (m *mockInventory) set(ds []digest.Digest, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.digests = append([]digest.Digest(nil), ds...)
	m.err = err
}

func (m *mockInventory) Inventory(_ context.Context) ([]digest.Digest, error) {
	atomic.AddInt32(&m.calls, 1)
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.err != nil {
		return nil, m.err
	}

	out := make([]digest.Digest, len(m.digests))
	copy(out, m.digests)

	return out, nil
}

func mkDigest(s string) digest.Digest {
	sum := sha256.Sum256([]byte(s))
	return digest.MustParse("sha256:" + hex.EncodeToString(sum[:]))
}

// TestReconcile_ProvideNewDigests verifies the present-but-not-announced
// arm: every digest from the inventory snapshot lands in DHT.Provide
// exactly once and the announced set converges.
func TestReconcile_ProvideNewDigests(t *testing.T) {
	d1, d2, d3 := mkDigest("a"), mkDigest("b"), mkDigest("c")
	inv := &mockInventory{}
	inv.set([]digest.Digest{d1, d2, d3}, nil)

	dht := fakes.NewDHT()
	a := New(inv, dht)

	if err := a.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	for _, d := range []digest.Digest{d1, d2, d3} {
		if dht.ProvideCount(d) != 1 {
			t.Errorf("ProvideCount(%s) = %d, want 1", d, dht.ProvideCount(d))
		}

		if !a.IsAnnounced(d) {
			t.Errorf("digest %s missing from announced set", d)
		}
	}

	if a.AnnouncedSize() != 3 {
		t.Errorf("AnnouncedSize = %d, want 3", a.AnnouncedSize())
	}

	// Second pass with the same inventory must not re-Provide already-
	// announced digests (idempotence requirement).
	if err := a.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile #2: %v", err)
	}

	for _, d := range []digest.Digest{d1, d2, d3} {
		if dht.ProvideCount(d) != 1 {
			t.Errorf("ProvideCount(%s) = %d after second reconcile, want 1 (idempotent)", d, dht.ProvideCount(d))
		}
	}
}

// TestReconcile_WithdrawDisappeared covers the announced-but-absent
// arm: digests that were announced but no longer appear in the
// inventory must trigger DHT.Withdraw and drop from the announced set.
func TestReconcile_WithdrawDisappeared(t *testing.T) {
	d1, d2 := mkDigest("d1"), mkDigest("d2")
	inv := &mockInventory{}
	inv.set([]digest.Digest{d1, d2}, nil)

	dht := fakes.NewDHT()
	a := New(inv, dht)

	if err := a.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile #1: %v", err)
	}
	// Simulate containerd GC: d1 disappeared, d2 still present.
	inv.set([]digest.Digest{d2}, nil)

	if err := a.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile #2: %v", err)
	}

	if dht.WithdrawCount(d1) != 1 {
		t.Errorf("WithdrawCount(d1) = %d, want 1", dht.WithdrawCount(d1))
	}

	if dht.WithdrawCount(d2) != 0 {
		t.Errorf("WithdrawCount(d2) = %d, want 0 (still present)", dht.WithdrawCount(d2))
	}

	if a.IsAnnounced(d1) {
		t.Errorf("d1 still in announced set after withdraw")
	}

	if !a.IsAnnounced(d2) {
		t.Errorf("d2 dropped from announced set despite still present")
	}
}

// TestReconcile_InventoryErrorAborts verifies that a failed Inventory
// snapshot returns the error and does NOT mutate the announced set
// (better to keep stale-but-valid records than purge based on a
// transient backend failure).
func TestReconcile_InventoryErrorAborts(t *testing.T) {
	d1 := mkDigest("only")
	inv := &mockInventory{}
	inv.set([]digest.Digest{d1}, nil)

	dht := fakes.NewDHT()

	a := New(inv, dht)
	if err := a.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile #1: %v", err)
	}

	wantErr := errors.New("containerd: simulated outage")
	inv.set(nil, wantErr)

	err := a.Reconcile(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Reconcile error = %v, want %v", err, wantErr)
	}

	if !a.IsAnnounced(d1) {
		t.Errorf("announced set mutated despite inventory failure")
	}

	if dht.WithdrawCount(d1) != 0 {
		t.Errorf("Withdraw fired on inventory-failure path; want 0")
	}
}

// TestNotify_FastPath exercises the event-driven Provide/Withdraw entry
// point used by the cdsub event handler between reconcile ticks.
func TestNotify_FastPath(t *testing.T) {
	d := mkDigest("event-driven")
	inv := &mockInventory{}
	dht := fakes.NewDHT()
	a := New(inv, dht)

	a.Notify(context.Background(), d, true)

	if !a.IsAnnounced(d) {
		t.Fatalf("digest not in announced set after Notify(present=true)")
	}

	if dht.ProvideCount(d) != 1 {
		t.Errorf("ProvideCount = %d, want 1", dht.ProvideCount(d))
	}

	a.Notify(context.Background(), d, false)

	if a.IsAnnounced(d) {
		t.Errorf("digest still announced after Notify(present=false)")
	}

	if dht.WithdrawCount(d) != 1 {
		t.Errorf("WithdrawCount = %d, want 1", dht.WithdrawCount(d))
	}
}

func TestNotify_PresentRequiresOpenableDigest(t *testing.T) {
	d := mkDigest("event-driven-openable")
	inv := &mockOpenInventory{open: map[string]error{}}
	dht := fakes.NewDHT()
	a := New(inv, dht)

	a.Notify(context.Background(), d, true)

	if a.IsAnnounced(d) {
		t.Fatalf("digest announced even though Open returned not found")
	}

	if dht.ProvideCount(d) != 0 {
		t.Fatalf("ProvideCount = %d, want 0", dht.ProvideCount(d))
	}

	inv.open[d.String()] = nil
	a.Notify(context.Background(), d, true)

	if !a.IsAnnounced(d) {
		t.Fatalf("digest not announced after Open succeeded")
	}

	if dht.ProvideCount(d) != 1 {
		t.Fatalf("ProvideCount = %d, want 1", dht.ProvideCount(d))
	}
}

func TestNotify_UnavailableDoesNotWithdrawExistingAnnouncement(t *testing.T) {
	d := mkDigest("event-driven-unavailable")
	inv := &mockOpenInventory{open: map[string]error{d.String(): nil}}
	dht := fakes.NewDHT()
	a := New(inv, dht)
	a.Notify(context.Background(), d, true)

	if !a.IsAnnounced(d) {
		t.Fatalf("digest not announced after initial openable notify")
	}

	inv.open[d.String()] = &ifaces.ErrUnavailable{Op: "Open", Cause: errors.New("socket down")}
	a.Notify(context.Background(), d, true)

	if !a.IsAnnounced(d) {
		t.Fatalf("announced set was lost on unavailable notify")
	}

	if dht.WithdrawCount(d) != 0 {
		t.Fatalf("WithdrawCount = %d, want 0", dht.WithdrawCount(d))
	}

	if dht.ProvideCount(d) != 1 {
		t.Fatalf("ProvideCount = %d, want 1 (unavailable notify must not re-provide)", dht.ProvideCount(d))
	}
}

// TestReconcile_MetricsHooks verifies the lifecycle metric hooks fire
// at the expected counts and carry sensible payloads.
func TestReconcile_MetricsHooks(t *testing.T) {
	d1, d2 := mkDigest("m1"), mkDigest("m2")
	inv := &mockInventory{}
	inv.set([]digest.Digest{d1, d2}, nil)

	dht := fakes.NewDHT()

	var (
		starts, ends                        int32
		provides, withdraws                 int32
		lastInvSize, lastAdded, lastRemoved int
	)

	a := New(inv, dht, WithMetrics(MetricsHooks{
		OnReconcileStart: func() { atomic.AddInt32(&starts, 1) },
		OnReconcileEnd: func(_ time.Duration, invSize, added, removed int) {
			atomic.AddInt32(&ends, 1)

			lastInvSize = invSize
			lastAdded = added
			lastRemoved = removed
		},
		OnProvide:  func() { atomic.AddInt32(&provides, 1) },
		OnWithdraw: func() { atomic.AddInt32(&withdraws, 1) },
	}))

	if err := a.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if got := atomic.LoadInt32(&starts); got != 1 {
		t.Errorf("OnReconcileStart fired %d times, want 1", got)
	}

	if got := atomic.LoadInt32(&ends); got != 1 {
		t.Errorf("OnReconcileEnd fired %d times, want 1", got)
	}

	if got := atomic.LoadInt32(&provides); got != 2 {
		t.Errorf("OnProvide fired %d times, want 2", got)
	}

	if lastInvSize != 2 || lastAdded != 2 || lastRemoved != 0 {
		t.Errorf("OnReconcileEnd payload = (size=%d, added=%d, removed=%d), want (2,2,0)",
			lastInvSize, lastAdded, lastRemoved)
	}

	// Remove a digest and verify withdraw + remove metric.
	inv.set([]digest.Digest{d2}, nil)

	if err := a.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile #2: %v", err)
	}

	if got := atomic.LoadInt32(&withdraws); got != 1 {
		t.Errorf("OnWithdraw fired %d times, want 1", got)
	}

	if lastInvSize != 1 || lastAdded != 0 || lastRemoved != 1 {
		t.Errorf("OnReconcileEnd payload after withdraw = (size=%d, added=%d, removed=%d), want (1,0,1)",
			lastInvSize, lastAdded, lastRemoved)
	}
}

// TestRun_TicksAndStops verifies Run drives at least one reconcile
// pass and exits cleanly on context cancellation.
func TestRun_TicksAndStops(t *testing.T) {
	d := mkDigest("tick")
	inv := &mockInventory{}
	inv.set([]digest.Digest{d}, nil)

	dht := fakes.NewDHT()
	a := New(inv, dht, WithReconcileInterval(50*time.Millisecond))

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)

	go func() { done <- a.Run(ctx) }()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Run returned %v, want context cancel/deadline", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit within deadline")
	}

	if atomic.LoadInt32(&inv.calls) < 2 {
		t.Errorf("Inventory called %d times, want >= 2 (initial + at least one tick)", inv.calls)
	}

	if dht.ProvideCount(d) < 1 {
		t.Errorf("ProvideCount(d) = %d, want >= 1", dht.ProvideCount(d))
	}
}

// TestReconcile_InventoryUnavailableFiresHook verifies that when
// inventory returns *ifaces.ErrUnavailable the reconcile pass calls
// OnReconcileUnavailable (NOT OnReconcileError) and preserves the
// announced set. Per containerd-unreachable must
// pause reconcile rather than treating "everything as absent".
func TestReconcile_InventoryUnavailableFiresHook(t *testing.T) {
	d1 := mkDigest("survives")
	inv := &mockInventory{}
	inv.set([]digest.Digest{d1}, nil)

	dht := fakes.NewDHT()

	var (
		unavailableCount int
		genericErrCount  int
	)

	a := New(inv, dht, WithMetrics(MetricsHooks{
		OnReconcileUnavailable: func() { unavailableCount++ },
		OnReconcileError:       func(error) { genericErrCount++ },
	}))
	if err := a.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile #1: %v", err)
	}

	inv.set(nil, &ifaces.ErrUnavailable{Op: "Walk"})

	if err := a.Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile returned nil on ErrUnavailable")
	}

	if unavailableCount != 1 {
		t.Errorf("OnReconcileUnavailable fired %d times; want 1", unavailableCount)
	}

	if genericErrCount != 0 {
		t.Errorf("OnReconcileError fired %d times on ErrUnavailable; want 0", genericErrCount)
	}

	if !a.IsAnnounced(d1) {
		t.Errorf("announced set lost d1 across ErrUnavailable; plan says it must be preserved")
	}

	if dht.WithdrawCount(d1) != 0 {
		t.Errorf("Withdraw fired on ErrUnavailable path; want 0")
	}
}
