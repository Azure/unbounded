// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package route

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
)

// HeaderSize is the size of the encoded Header in bytes.
const HeaderSize = 32

// HeaderFormat is the only header format version this package produces and
// accepts.
const HeaderFormat uint8 = 1

// Header flags. FlagHedge is set by the caller on a hedged copy of a request;
// the others describe the hop the header is sent on and are recomputed by
// every member that routes it.
const (
	// FlagHedge marks a hedged request. It is preserved across hops.
	FlagHedge uint8 = 1 << iota
	// FlagDirect marks a hop to the owner outside the neighbor budget.
	FlagDirect
	// FlagGateway marks a hop from a non-member to a gateway.
	FlagGateway
	// FlagBackup marks a hop to a backup of an unusable owner. The receiver
	// serves the request itself if it is in the owner's cell.
	FlagBackup

	flagsKnown = FlagHedge | FlagDirect | FlagGateway | FlagBackup
	flagsHop   = FlagDirect | FlagGateway | FlagBackup
)

// Errors returned by ParseHeader and State.Forward.
var (
	// ErrBadHeader reports a malformed header or one that does not fit the
	// receiver's view.
	ErrBadHeader = errors.New("route: bad header")
	// ErrTTLExhausted reports a request that would exceed its hop budget.
	ErrTTLExhausted = errors.New("route: ttl exhausted")
)

// Header travels with every data-plane request. Its wire form is HeaderSize
// bytes, big-endian:
//
//	0      Format
//	1      Flags
//	2      Hops
//	3      TTL
//	4..8   Partition
//	8..12  Sender
//	12..16 Term
//	16..24 Seq
//	24..32 Epoch
//
// Term is the low 32 bits of the sender's view term; members compare it with
// the low 32 bits of their own term, which is exact as long as the cluster
// sees fewer than 2^32 leader terms.
type Header struct {
	// Format is HeaderFormat.
	Format uint8
	// Flags is a set of Flag* bits.
	Flags uint8
	// Hops counts the hops taken so far; the origin sends 0.
	Hops uint8
	// TTL is the hop budget set at the origin: grid dimensions plus two.
	TTL uint8
	// Partition is the partition of the key.
	Partition uint32
	// Sender is the slot of the member that sent this hop, or view.NoSlot.
	Sender view.Slot
	// Term is the sender's view term, truncated to 32 bits.
	Term uint32
	// Seq is the sender's view seq.
	Seq uint64
	// Epoch is the sender's current epoch.
	Epoch uint64
}

// Put encodes h into b.
func (h *Header) Put(b *[HeaderSize]byte) {
	b[0] = h.Format
	b[1] = h.Flags
	b[2] = h.Hops
	b[3] = h.TTL
	binary.BigEndian.PutUint32(b[4:], h.Partition)
	binary.BigEndian.PutUint32(b[8:], h.Sender)
	binary.BigEndian.PutUint32(b[12:], h.Term)
	binary.BigEndian.PutUint64(b[16:], h.Seq)
	binary.BigEndian.PutUint64(b[24:], h.Epoch)
}

// Append appends the encoded header to b.
func (h Header) Append(b []byte) []byte {
	var buf [HeaderSize]byte

	h.Put(&buf)

	return append(b, buf[:]...)
}

// ParseHeader decodes the first HeaderSize bytes of b. It rejects a short
// buffer, an unknown format, unknown flags, and Hops above TTL. Trailing
// bytes are ignored.
func ParseHeader(b []byte) (Header, error) {
	if len(b) < HeaderSize {
		return Header{}, fmt.Errorf("%w: %d bytes", ErrBadHeader, len(b))
	}

	h := Header{
		Format:    b[0],
		Flags:     b[1],
		Hops:      b[2],
		TTL:       b[3],
		Partition: binary.BigEndian.Uint32(b[4:]),
		Sender:    binary.BigEndian.Uint32(b[8:]),
		Term:      binary.BigEndian.Uint32(b[12:]),
		Seq:       binary.BigEndian.Uint64(b[16:]),
		Epoch:     binary.BigEndian.Uint64(b[24:]),
	}

	if err := h.check(); err != nil {
		return Header{}, err
	}

	return h, nil
}

func (h *Header) check() error {
	switch {
	case h.Format != HeaderFormat:
		return fmt.Errorf("%w: format %d", ErrBadHeader, h.Format)
	case h.Flags&^flagsKnown != 0:
		return fmt.Errorf("%w: flags %#x", ErrBadHeader, h.Flags)
	case h.Hops > h.TTL:
		return fmt.Errorf("%w: hops %d above ttl %d", ErrBadHeader, h.Hops, h.TTL)
	}

	return nil
}

// Note records a sender whose view version was ahead of the receiver's.
type Note struct {
	// Term is the sender's view term, truncated to 32 bits.
	Term uint32
	// Seq is the sender's view seq.
	Seq uint64
	// Sender is the slot of the sender in the receiver's view.
	Sender view.Slot
	// Epoch is the sender's epoch.
	Epoch uint64
}

func (n *Note) less(term uint32, seq uint64) bool {
	return n.Term < term || (n.Term == term && n.Seq < seq)
}

// Notes collects the highest view version seen in incoming headers so that
// the control plane can catch up from that sender. Forward calls note on
// the hot path: one atomic load and a comparison unless the header is ahead
// of both this member's view and the recorded note, in which case it
// allocates one Note. The zero value is ready to use and safe for
// concurrent use.
type Notes struct {
	p atomic.Pointer[Note]
}

// note records h if its version is ahead of own (term truncated to 32 bits)
// and of the recorded note.
func (n *Notes) note(h *Header, ownTerm uint32, ownSeq uint64) {
	if h.Sender == view.NoSlot {
		return
	}

	if own := (Note{Term: ownTerm, Seq: ownSeq}); !own.less(h.Term, h.Seq) {
		return
	}

	var nn *Note

	for {
		cur := n.p.Load()
		if cur != nil && !cur.less(h.Term, h.Seq) {
			return
		}

		if nn == nil {
			nn = &Note{Term: h.Term, Seq: h.Seq, Sender: h.Sender, Epoch: h.Epoch}
		}

		if n.p.CompareAndSwap(cur, nn) {
			return
		}
	}
}

// Take returns and clears the recorded note. Control path.
func (n *Notes) Take() (Note, bool) {
	p := n.p.Swap(nil)
	if p == nil {
		return Note{}, false
	}

	return *p, true
}

// Epochs. The code below implements the rolling cluster epoch and the expiry stamps
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
