// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

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
	mu             sync.Mutex
	current        []digest.Digest
	queue          []inventoryResult
	inventoryCalls int
	openableCalls  map[string]int
}

func (f *fakeInventorySource) Inventory(_ context.Context) ([]digest.Digest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.inventoryCalls++

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

func (f *fakeInventorySource) Openable(_ context.Context, d digest.Digest) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.openableCalls == nil {
		f.openableCalls = map[string]int{}
	}

	f.openableCalls[d.String()]++
	if len(f.queue) > 0 {
		res := f.queue[0]
		f.queue = f.queue[1:]

		if res.err != nil {
			return false, res.err
		}
	}

	for _, current := range f.current {
		if current == d {
			return true, nil
		}
	}

	return false, nil
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

func (f *fakeInventorySource) CallCounts(d digest.Digest) (inventory, openable int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.inventoryCalls, f.openableCalls[d.String()]
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
	tracker.probeBudget = 20 * time.Millisecond

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
	tracker.probeBudget = 20 * time.Millisecond

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
	tracker.probeBudget = 20 * time.Millisecond

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

func TestStreamCommitTracker_DuplicateCompletionsAreReportedIndividually(t *testing.T) {
	d := trackerDigestOf([]byte("duplicate-completions"))
	inv := &fakeInventorySource{current: []digest.Digest{d}}

	var (
		observed  []int
		durations []time.Duration
	)

	tracker := newStreamCommitTracker(inv, nil,
		func(n int) { observed = append(observed, n) },
		func(duration time.Duration) { durations = append(durations, duration) },
		nil,
	)
	now := time.Now()
	tracker.pending[d.String()] = []pendingStreamCommit{
		{completedAt: now.Add(-3 * time.Second), deadline: now.Add(time.Minute)},
		{completedAt: now.Add(-2 * time.Second), deadline: now.Add(time.Minute)},
		{completedAt: now.Add(-time.Second), deadline: now.Add(time.Minute)},
	}

	tracker.probe(context.Background())

	if len(observed) != 1 || observed[0] != 3 {
		t.Fatalf("observed callbacks = %v, want [3]", observed)
	}

	if len(durations) != 3 || durations[0] <= durations[1] || durations[1] <= durations[2] {
		t.Fatalf("durations = %v, want oldest completion first", durations)
	}

	if len(tracker.pending) != 0 {
		t.Fatalf("pending = %v, want empty", tracker.pending)
	}
}

func TestStreamCommitTracker_PresenceWinsAtDeadline(t *testing.T) {
	d := trackerDigestOf([]byte("present-at-deadline"))
	inv := &fakeInventorySource{current: []digest.Digest{d}}

	var observed, missing int

	tracker := newStreamCommitTracker(inv, nil,
		func(n int) { observed += n },
		nil,
		func(n int) { missing += n },
	)
	tracker.pending[d.String()] = []pendingStreamCommit{{
		completedAt: time.Now().Add(-time.Minute),
		deadline:    time.Now().Add(-time.Second),
	}}

	tracker.probe(context.Background())

	if observed != 1 || missing != 0 {
		t.Fatalf("observed = %d, missing = %d; want 1, 0", observed, missing)
	}
}

func TestStreamCommitTracker_StorageErrorRetainsEntireProbe(t *testing.T) {
	present := trackerDigestOf([]byte("present-during-error"))
	absent := trackerDigestOf([]byte("absent-during-error"))
	inv := &fakeInventorySource{current: []digest.Digest{present}}
	inv.Queue(inventoryResult{err: errors.New("storage failure")})

	var observed, missing int

	tracker := newStreamCommitTracker(inv, nil,
		func(n int) { observed += n },
		nil,
		func(n int) { missing += n },
	)
	expired := time.Now().Add(-time.Second)
	tracker.pending[present.String()] = []pendingStreamCommit{{deadline: expired}}
	tracker.pending[absent.String()] = []pendingStreamCommit{{deadline: expired}}

	tracker.probe(context.Background())

	if observed != 0 || missing != 0 {
		t.Fatalf("observed = %d, missing = %d; want 0, 0", observed, missing)
	}

	if len(tracker.pending) != 2 {
		t.Fatalf("pending digests = %d, want 2", len(tracker.pending))
	}
}

func TestStreamCommitTracker_CallbackCanRecordCompletion(t *testing.T) {
	observedDigest := trackerDigestOf([]byte("observed-before-callback"))
	newDigest := trackerDigestOf([]byte("recorded-by-callback"))
	inv := &fakeInventorySource{current: []digest.Digest{observedDigest}}

	var tracker *streamCommitTracker

	tracker = newStreamCommitTracker(inv, nil, func(_ int) {
		tracker.RecordCompleted(newDigest)
	}, nil, nil)
	tracker.RecordCompleted(observedDigest)

	tracker.probe(context.Background())

	if _, ok := tracker.pending[newDigest.String()]; !ok {
		t.Fatal("completion recorded by callback is not pending")
	}
}

func TestStreamCommitTracker_ChecksEachPendingDigestOnce(t *testing.T) {
	first := trackerDigestOf([]byte("first-pending"))
	second := trackerDigestOf([]byte("second-pending"))
	unrelated := trackerDigestOf([]byte("unrelated-inventory-entry"))
	inv := &fakeInventorySource{current: []digest.Digest{unrelated}}
	tracker := newStreamCommitTracker(inv, nil, nil, nil, nil)
	deadline := time.Now().Add(time.Minute)
	tracker.pending[first.String()] = []pendingStreamCommit{
		{deadline: deadline},
		{deadline: deadline},
	}
	tracker.pending[second.String()] = []pendingStreamCommit{{deadline: deadline}}

	tracker.probe(context.Background())

	inventoryCalls, firstCalls := inv.CallCounts(first)
	_, secondCalls := inv.CallCounts(second)
	_, unrelatedCalls := inv.CallCounts(unrelated)

	if inventoryCalls != 0 {
		t.Fatalf("Inventory calls = %d, want 0", inventoryCalls)
	}

	if firstCalls != 1 || secondCalls != 1 || unrelatedCalls != 0 {
		t.Fatalf("Openable calls = first:%d second:%d unrelated:%d; want 1, 1, 0", firstCalls, secondCalls, unrelatedCalls)
	}
}
