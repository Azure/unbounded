// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package dst

import (
	"slices"

	"github.com/Azure/unbounded/internal/cachemesh/internal/elect"
)

// RateWindow tracks a count of events and the maximum number that fell in
// any sliding one-second window.
type RateWindow struct {
	times []int64
	head  int
	Total uint64
	// Max is the largest number of events in any interval (t-1s, t].
	Max int
}

func (w *RateWindow) record(now int64) {
	w.Total++
	w.times = append(w.times, now)

	for w.times[w.head] <= now-nsPerSec {
		w.head++
	}

	w.Max = max(w.Max, len(w.times)-w.head)

	if w.head > 1024 && w.head*2 > len(w.times) {
		w.times = append(w.times[:0], w.times[w.head:]...)
		w.head = 0
	}
}

// QPS accounts operations by kind and by member over simulated time.
type QPS struct {
	all     RateWindow
	kinds   [8]RateWindow
	members map[uint32]*RateWindow
	series  []uint32
	first   int64 // absolute second of series[0]
	keep    int
}

// NewQPS returns an accounting that keeps keep recent per-second totals
// (default 600 when keep <= 0).
func NewQPS(keep int) *QPS {
	if keep <= 0 {
		keep = 600
	}

	return &QPS{members: make(map[uint32]*RateWindow), keep: keep}
}

// Record accounts one operation at simulated time now.
func (q *QPS) Record(now int64, kind elect.OpKind, member uint32) {
	q.all.record(now)
	q.kinds[int(kind)%len(q.kinds)].record(now)

	m, ok := q.members[member]
	if !ok {
		m = &RateWindow{}
		q.members[member] = m
	}

	m.record(now)

	second := now / nsPerSec
	if len(q.series) == 0 {
		q.first = second
	}

	for q.first+int64(len(q.series)) <= second {
		q.series = append(q.series, 0)
	}

	if idx := second - q.first; idx >= 0 {
		q.series[idx]++
	}

	if over := len(q.series) - q.keep; over > q.keep {
		q.series = slices.Delete(q.series, 0, over)
		q.first += int64(over)
	}
}

// All returns the accounting over every operation.
func (q *QPS) All() RateWindow { return q.all }

// Kind returns the accounting for one operation kind.
func (q *QPS) Kind(k elect.OpKind) RateWindow { return q.kinds[int(k)%len(q.kinds)] }

// Member returns the accounting for one member.
func (q *QPS) Member(m uint32) RateWindow {
	if w, ok := q.members[m]; ok {
		return *w
	}

	return RateWindow{}
}

// Members returns the members that issued operations, sorted.
func (q *QPS) Members() []uint32 {
	out := make([]uint32, 0, len(q.members))
	for m := range q.members {
		out = append(out, m)
	}

	slices.Sort(out)

	return out
}

// Series returns up to the last n per-second totals ending at the latest
// recorded second, and the absolute simulated second of the first entry.
func (q *QPS) Series(n int) ([]uint32, int64) {
	s := q.series
	if n > 0 && len(s) > n {
		s = s[len(s)-n:]
	}

	return slices.Clone(s), q.first + int64(len(q.series)-len(s))
}
