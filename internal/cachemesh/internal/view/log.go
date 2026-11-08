// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package view

import "fmt"

// DefaultDeltaLogSize is the default number of deltas a DeltaLog keeps.
const DefaultDeltaLogSize = 512

// LogEntry is a logged delta and its canonical encoding.
type LogEntry struct {
	Delta   *Delta
	Encoded []byte
}

type logEntry struct {
	LogEntry

	baseDigest Digest
}

// DeltaLog is a bounded chain of the most recent deltas a node applied, kept
// with their encodings so serving catch-up does not re-encode. It is not
// safe for concurrent use.
type DeltaLog struct {
	ring  []logEntry
	start int // index of the oldest entry
	n     int

	// head is the version and digest at the end of the chain (or the anchor
	// when the log is empty).
	head       Version
	headDigest Digest
}

// NewDeltaLog returns a log holding up to size deltas (DefaultDeltaLogSize
// if size <= 0), anchored at the genesis view.
func NewDeltaLog(size int) *DeltaLog {
	if size <= 0 {
		size = DefaultDeltaLogSize
	}

	l := &DeltaLog{ring: make([]logEntry, size)}
	l.Reset(Genesis())

	return l
}

// Reset empties the log and anchors it at v, for example after installing a
// snapshot.
func (l *DeltaLog) Reset(v *View) {
	clear(l.ring)
	l.start, l.n = 0, 0
	l.head, l.headDigest = v.Version(), v.Digest()
}

// Len returns the number of logged deltas.
func (l *DeltaLog) Len() int { return l.n }

// Head returns the version and digest at the end of the chain.
func (l *DeltaLog) Head() (Version, Digest) { return l.head, l.headDigest }

// Append adds d, which must have been applied to the log head (d.Base equals
// the head version), evicting the oldest delta when full. encoded is the
// canonical encoding of d; if nil it is computed. The log retains d and
// encoded.
func (l *DeltaLog) Append(d *Delta, encoded []byte) error {
	if d.Base != l.head {
		return fmt.Errorf("%w: log head %v, delta base %v", ErrBase, l.head, d.Base)
	}

	if encoded == nil {
		encoded = EncodeDelta(d)
	}

	i := (l.start + l.n) % len(l.ring)
	if l.n == len(l.ring) {
		l.start = (l.start + 1) % len(l.ring)
	} else {
		l.n++
	}

	l.ring[i] = logEntry{LogEntry: LogEntry{Delta: d, Encoded: encoded}, baseDigest: l.headDigest}
	l.head, l.headDigest = d.Version, d.Digest

	return nil
}

// Since returns up to limit deltas (all if max <= 0) that follow the view
// (have, haveDigest), oldest first, and true, when that view is the head or
// the base of a logged delta. It returns nil and true when have is the head
// and false when the log cannot serve the request (the caller sends a
// snapshot). The returned entries are shared.
func (l *DeltaLog) Since(have Version, haveDigest Digest, limit int) ([]LogEntry, bool) {
	if have == l.head {
		return nil, haveDigest == l.headDigest
	}

	for k := range l.n {
		e := &l.ring[(l.start+k)%len(l.ring)]
		if e.Delta.Base != have {
			continue
		}

		if e.baseDigest != haveDigest {
			return nil, false
		}

		cnt := l.n - k
		if limit > 0 {
			cnt = min(cnt, limit)
		}

		out := make([]LogEntry, cnt)
		for j := range out {
			out[j] = l.ring[(l.start+k+j)%len(l.ring)].LogEntry
		}

		return out, true
	}

	return nil, false
}
