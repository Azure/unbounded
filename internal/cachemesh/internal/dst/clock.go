// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package dst

import (
	"math/bits"
	"time"
)

// Epoch is the wall time at simulated time 0.
var Epoch = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

// MaxDriftPPB bounds the magnitude of a clock's drift (50%).
const MaxDriftPPB = 500_000_000

const nsPerSec = 1_000_000_000

// Clock is one member's view of time. Its rate differs from simulated time
// by DriftPPB parts per billion, its monotonic reading starts at an
// arbitrary base, and its wall reading is offset from the true simulated
// wall time. The library under test must only ever see its own member's
// Clock.
//
// Changing the drift re-bases the clock at the current simulated time, so
// Mono never decreases. Changing the offset steps the wall clock only.
type Clock struct {
	anchor   int64 // simulated time of the last re-base
	monoBase int64 // Mono at anchor
	wallBase int64 // wall nanoseconds since Epoch at anchor
	offset   int64
	ppb      int64
}

// NewClock returns a clock whose Mono reads monoBase at simulated time 0,
// with no offset and no drift.
func NewClock(monoBase int64) *Clock {
	return &Clock{monoBase: monoBase}
}

// elapsed scales a non-negative simulated duration by the drift without
// overflowing for any int64 duration.
func (c *Clock) elapsed(dt int64) int64 {
	q, r := dt/nsPerSec, dt%nsPerSec

	return dt + q*c.ppb + r*c.ppb/nsPerSec
}

func (c *Clock) since(now int64) int64 {
	if now < c.anchor {
		now = c.anchor
	}

	return c.elapsed(now - c.anchor)
}

// Mono returns the member's monotonic reading at simulated time now. It
// never decreases as now increases.
func (c *Clock) Mono(now int64) int64 { return c.monoBase + c.since(now) }

// Wall returns the member's wall time at simulated time now.
func (c *Clock) Wall(now int64) time.Time {
	return Epoch.Add(time.Duration(c.wallBase + c.since(now)))
}

// DriftPPB returns the current drift.
func (c *Clock) DriftPPB() int64 { return c.ppb }

// Offset returns the current wall offset.
func (c *Clock) Offset() time.Duration { return time.Duration(c.offset) }

func (c *Clock) rebase(now int64) {
	if now < c.anchor {
		now = c.anchor
	}

	e := c.since(now)
	c.monoBase += e
	c.wallBase += e
	c.anchor = now
}

// SetDrift changes the drift from simulated time now on. ppb is clamped to
// [-MaxDriftPPB, MaxDriftPPB].
func (c *Clock) SetDrift(now, ppb int64) {
	c.rebase(now)
	c.ppb = max(-MaxDriftPPB, min(MaxDriftPPB, ppb))
}

// SetOffset steps the wall clock so it reads off ahead of a drift-free
// clock re-based at now. Mono is unaffected.
func (c *Clock) SetOffset(now int64, off time.Duration) {
	c.rebase(now)
	c.wallBase += int64(off) - c.offset
	c.offset = int64(off)
}

// SimTime returns the earliest simulated time at or after the last re-base
// at which Mono reaches mono, under the current drift. Use it to schedule a
// member's timer; if the drift changes before the timer fires, the harness
// must convert again. Deadlines already reached map to the re-base time,
// which the scheduler clamps to Now.
func (c *Clock) SimTime(mono int64) int64 {
	d := mono - c.monoBase
	if d <= 0 {
		return c.anchor
	}

	hi, lo := bits.Mul64(uint64(d), nsPerSec)
	q, _ := bits.Div64(hi, lo, uint64(nsPerSec+c.ppb))
	dt := int64(q)

	for c.elapsed(dt) < d {
		dt++
	}

	for dt > 0 && c.elapsed(dt-1) >= d {
		dt--
	}

	return c.anchor + dt
}
