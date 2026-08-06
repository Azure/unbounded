// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

func trackerDigestOf(b []byte) digest.Digest {
	sum := sha256.Sum256(b)

	d, err := digest.Parse("sha256:" + hex.EncodeToString(sum[:]))
	if err != nil {
		panic(err)
	}

	return d
}

type inventoryResult struct {
	digests []digest.Digest
	err     error
}

type fakeInventorySource struct {
	mu      sync.Mutex
	current []digest.Digest
	queue   []inventoryResult
}

func (f *fakeInventorySource) Inventory(_ context.Context) ([]digest.Digest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.queue) > 0 {
		res := f.queue[0]
		f.queue = f.queue[1:]
		out := make([]digest.Digest, len(res.digests))
		copy(out, res.digests)

		return out, res.err
	}

	out := make([]digest.Digest, len(f.current))
	copy(out, f.current)

	return out, nil
}

func (f *fakeInventorySource) SetCurrent(ds ...digest.Digest) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.current = append([]digest.Digest(nil), ds...)
}

func (f *fakeInventorySource) Queue(res inventoryResult) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.queue = append(f.queue, res)
}

func waitForAtomic(t *testing.T, counter *int32, want int32) {
	t.Helper()

	deadline := time.Now().Add(750 * time.Millisecond)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(counter) == want {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("counter = %d, want %d", atomic.LoadInt32(counter), want)
}

func TestStreamCommitTracker_ObservedAfterInventoryAppears(t *testing.T) {
	d := trackerDigestOf([]byte("observed-after-stream"))
	inv := &fakeInventorySource{}

	var observed, missing, durations int32

	tracker := newStreamCommitTracker(inv, nil,
		func(n int) { atomic.AddInt32(&observed, int32(n)) },
		func(duration time.Duration) {
			if duration <= 0 {
				t.Errorf("observed duration = %s, want positive", duration)
			}

			atomic.AddInt32(&durations, 1)
		},
		func(n int) { atomic.AddInt32(&missing, int32(n)) },
	)
	tracker.probeInterval = 5 * time.Millisecond
	tracker.verifyWindow = 50 * time.Millisecond
	tracker.inventoryBudget = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = tracker.Run(ctx) }() //nolint:errcheck // best-effort

	tracker.RecordCompleted(d)
	time.Sleep(15 * time.Millisecond)
	inv.SetCurrent(d)

	waitForAtomic(t, &observed, 1)
	waitForAtomic(t, &durations, 1)

	if got := atomic.LoadInt32(&missing); got != 0 {
		t.Fatalf("missing = %d, want 0", got)
	}
}

func TestStreamCommitTracker_MissingAfterDeadline(t *testing.T) {
	d := trackerDigestOf([]byte("never-committed"))
	inv := &fakeInventorySource{}

	var observed, missing int32

	tracker := newStreamCommitTracker(inv, nil,
		func(n int) { atomic.AddInt32(&observed, int32(n)) },
		nil,
		func(n int) { atomic.AddInt32(&missing, int32(n)) },
	)
	tracker.probeInterval = 5 * time.Millisecond
	tracker.verifyWindow = 20 * time.Millisecond
	tracker.inventoryBudget = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = tracker.Run(ctx) }() //nolint:errcheck // best-effort

	tracker.RecordCompleted(d)
	waitForAtomic(t, &missing, 1)

	if got := atomic.LoadInt32(&observed); got != 0 {
		t.Fatalf("observed = %d, want 0", got)
	}
}

func TestStreamCommitTracker_RetriesAfterUnavailableInventory(t *testing.T) {
	d := trackerDigestOf([]byte("appears-after-unavailable"))
	inv := &fakeInventorySource{}
	inv.Queue(inventoryResult{err: &ifaces.ErrUnavailable{Op: "Inventory", Cause: errors.New("socket down")}})

	var observed, missing int32

	tracker := newStreamCommitTracker(inv, nil,
		func(n int) { atomic.AddInt32(&observed, int32(n)) },
		nil,
		func(n int) { atomic.AddInt32(&missing, int32(n)) },
	)
	tracker.probeInterval = 5 * time.Millisecond
	tracker.verifyWindow = 50 * time.Millisecond
	tracker.inventoryBudget = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = tracker.Run(ctx) }() //nolint:errcheck // best-effort

	tracker.RecordCompleted(d)
	time.Sleep(15 * time.Millisecond)
	inv.SetCurrent(d)

	waitForAtomic(t, &observed, 1)

	if got := atomic.LoadInt32(&missing); got != 0 {
		t.Fatalf("missing = %d, want 0", got)
	}
}

func TestStreamCommitTracker_ReportsLatestCompletedStreamLast(t *testing.T) {
	earlier := trackerDigestOf([]byte("earlier"))
	later := trackerDigestOf([]byte("later"))
	inv := &fakeInventorySource{current: []digest.Digest{earlier, later}}

	var durations []time.Duration

	tracker := newStreamCommitTracker(inv, nil, nil, func(duration time.Duration) {
		durations = append(durations, duration)
	}, nil)
	now := time.Now()
	tracker.pending[earlier.String()] = []pendingStreamCommit{{
		completedAt: now.Add(-2 * time.Second),
		deadline:    now.Add(time.Minute),
	}}
	tracker.pending[later.String()] = []pendingStreamCommit{{
		completedAt: now.Add(-time.Second),
		deadline:    now.Add(time.Minute),
	}}

	tracker.probe(context.Background())

	if len(durations) != 2 || durations[0] <= durations[1] {
		t.Fatalf("durations = %v, want earlier completion before latest completion", durations)
	}
}
