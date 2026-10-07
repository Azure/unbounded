// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"sync"
	"time"

	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// leaseFence tracks how long this process can rely on holding the leader lease,
// based on its own successful lease writes rather than on the leader elector's
// view of who the holder is.
//
// The elector only notices a lost lease once a renewal fails or times out, and
// it reports leadership from the last observed holder identity without checking
// expiry. A leader stalled by CPU starvation, a GC pause, or a partition can
// therefore keep acting after another candidate has taken the lease over. The
// fence instead expires on its own once renewals stop succeeding.
//
// A successful write at time t means no other candidate can acquire the lease
// before t+leaseDuration, because candidates wait a full lease duration after
// observing that write. The fence trusts the lease only until t+window, where
// window is the renew deadline: client-go requires renewDeadline to exceed
// retryPeriod*1.2, so a healthy leader renews before the fence expires, and
// leaseDuration-renewDeadline is left as margin for clock-rate skew and for
// writes still in flight when the fence expires.
type leaseFence struct {
	window time.Duration
	now    func() time.Time

	mu         sync.Mutex
	validUntil time.Time
}

func newLeaseFence(window time.Duration) *leaseFence {
	return &leaseFence{window: window, now: time.Now}
}

// ValidUntil returns when the lease must stop being trusted, or the zero time
// if it is not held.
func (f *leaseFence) ValidUntil() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.validUntil
}

// wrap returns a lock that updates the fence on every lease write.
func (f *leaseFence) wrap(lock resourcelock.Interface) resourcelock.Interface {
	return &fencedLock{Interface: lock, fence: f}
}

// observeWrite records the outcome of a lease write that started at start.
func (f *leaseFence) observeWrite(start time.Time, holder, identity string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case err != nil:
		// The write may still have been applied, but an unconfirmed renewal
		// cannot extend the fence. Leave the previous deadline to expire.
	case holder == identity:
		// Measure from the call start: the server applied the write no earlier
		// than this, so other candidates cannot take over any sooner.
		if until := start.Add(f.window); until.After(f.validUntil) {
			f.validUntil = until
		}
	default:
		// The lease was released or written for another holder.
		f.validUntil = time.Time{}
	}
}

// fencedLock is a resourcelock.Interface that reports each Create and Update
// to a leaseFence.
type fencedLock struct {
	resourcelock.Interface

	fence *leaseFence
}

func (l *fencedLock) Create(ctx context.Context, ler resourcelock.LeaderElectionRecord) error {
	start := l.fence.now()
	err := l.Interface.Create(ctx, ler)
	l.fence.observeWrite(start, ler.HolderIdentity, l.Identity(), err)

	return err
}

func (l *fencedLock) Update(ctx context.Context, ler resourcelock.LeaderElectionRecord) error {
	start := l.fence.now()
	err := l.Interface.Update(ctx, ler)
	l.fence.observeWrite(start, ler.HolderIdentity, l.Identity(), err)

	return err
}
