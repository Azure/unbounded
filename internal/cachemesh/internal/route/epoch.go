// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Epochs. This file implements the rolling cluster epoch and the expiry stamps
// built on it.
//
// The leader publishes an epoch number every period as a heartbeat. Each
// member maps its own monotonic clock onto that epoch with a EpochTracker:
//
//	current = max(previous, anchorEpoch + floor((now - anchorMono) / period))
//
// where (anchorEpoch, anchorMono, period) come from the most recent Anchor
// call. A member cut off from the leader keeps advancing on its own clock, so
// expiry is never suspended.
//
// The epoch never decreases, and an anchor never moves the phase backward:
// an anchor whose epoch is at or below the epoch the current anchor already
// gives for now (with the same period) is stale information and is ignored.
// Heartbeats reach a member up to a period after their epoch began (a
// catch-up delta or snapshot replays an old Epoch op at the time it is
// applied), so honoring such an anchor would delay this member's epoch
// boundaries. Worse, a member that became leader with a delayed phase would
// publish it, every member would stall to match, and each leader change
// could stretch every stamp's lifetime by up to another period. Only an
// anchor ahead of this member's epoch (or a period change) re-anchors; a
// member therefore lags the leader by at most the delay of the anchor that
// last moved it forward, and a leader that lags its members does not drag
// them back.
//
// Concurrency contract: Anchor and Advance are called from a single writer
// goroutine (the runtime tick). Current may be called from any goroutine and
// is one atomic load. The request path never reads the clock; time is passed
// in as int64 nanoseconds of a node-local monotonic clock.
//
// Stamps: a value with TTL T is stamped at the owner with
// Expires = current + ceil(T/period) + 1. Because the stamp is issued at an
// unknown offset within the current epoch, a value is Fresh for at least T and
// at most ceil(T/period)*period + period of the issuing clock: T + period
// when T is a multiple of the period, always less than T + 2*period. A TTL of zero never expires.
// Nothing here uses floating point; members compare stamps exactly.

package route

import (
	"math"
	"sync/atomic"
	"time"
)

const (
	// MinTTL is the smallest nonzero TTL a metadata keyspace may use.
	MinTTL = 5 * time.Minute
	// DefaultEpochPeriod is the default epoch period.
	DefaultEpochPeriod = 15 * time.Second
	// Never is the Expires value of a stamp that never expires.
	Never = math.MaxUint64
)

// EpochTracker maps local monotonic time to the cluster epoch. The zero value is
// ready to use with epoch 0 anchored at mono 0 and DefaultPeriod.
type EpochTracker struct {
	cur atomic.Uint64

	// Writer-only fields.
	anchorEpoch uint64
	anchorMono  int64
	anchored    bool

	// period is written by Anchor and read by Period, which the request
	// path may call concurrently (route.State.Stamp), so it is atomic.
	period atomic.Int64
}

// Current returns the current epoch. Safe for concurrent use.
func (t *EpochTracker) Current() uint64 { return t.cur.Load() }

// Period returns the period of the latest anchor.
func (t *EpochTracker) Period() time.Duration {
	if p := time.Duration(t.period.Load()); p > 0 {
		return p
	}

	return DefaultEpochPeriod
}

// Anchor records that epoch began (or was observed) at local time now with
// the given period, then advances. A non-positive period means DefaultPeriod.
// An anchor at or below the epoch the current anchor gives for now, with an
// unchanged period, is ignored (see the package documentation). The
// returned value is the current epoch, which may exceed epoch if this
// member is already ahead.
func (t *EpochTracker) Anchor(epoch uint64, period time.Duration, now int64) uint64 {
	if period <= 0 {
		period = DefaultEpochPeriod
	}

	if t.anchored && period == t.Period() && epoch <= t.at(now) {
		return t.Advance(now)
	}

	t.anchored = true
	t.anchorEpoch = epoch
	t.anchorMono = now
	t.period.Store(int64(period))

	return t.Advance(now)
}

// at returns the epoch the current anchor gives for local time now.
func (t *EpochTracker) at(now int64) uint64 {
	e := t.anchorEpoch
	if d := now - t.anchorMono; d > 0 {
		n := uint64(d / int64(t.Period()))
		if e > Never-n {
			return Never
		}

		e += n
	}

	return e
}

// Advance recomputes the epoch for local time now and returns it. It never
// decreases the epoch.
func (t *EpochTracker) Advance(now int64) uint64 {
	e := t.at(now)

	if prev := t.cur.Load(); e <= prev {
		return prev
	}

	t.cur.Store(e)

	return e
}

// Stamp is an expiry stamp carried with a cached value.
type Stamp struct {
	// Expires is the first epoch at which the value is stale.
	Expires uint64
	// Gen is the keyspace generation at stamping time.
	Gen uint32
}

// StampFor returns the stamp for a value issued at epoch current with the
// given TTL and period. A zero TTL never expires. A non-positive period means
// DefaultPeriod.
func StampFor(current uint64, ttl, period time.Duration, gen uint32) Stamp {
	if ttl == 0 {
		return Stamp{Expires: Never, Gen: gen}
	}

	if period <= 0 {
		period = DefaultEpochPeriod
	}

	if ttl < 0 {
		// Invalid; treat as already expired rather than wrapping.
		return Stamp{Expires: current, Gen: gen}
	}

	// ceil(ttl/period) without overflowing ttl + period - 1.
	n := uint64(ttl / period)
	if ttl%period != 0 {
		n++
	}

	if current >= Never-n-1 {
		return Stamp{Expires: Never, Gen: gen}
	}

	return Stamp{Expires: current + n + 1, Gen: gen}
}

// Fresh reports whether a value with stamp s is still valid at epoch current
// under keyspace generation gen.
func Fresh(s Stamp, current uint64, gen uint32) bool {
	return current < s.Expires && s.Gen == gen
}
