// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package dst

import (
	"errors"
	"math/rand/v2"
)

// ErrTimeout completes a Call whose response did not arrive in time.
var ErrTimeout = errors.New("dst: rpc timeout")

// Message is an opaque message between two members. Payload and Data are
// never inspected by the network; Data lets the harness pass typed values
// without serializing. A delivered *Message is owned by the network and is
// reused after Deliver or Complete returns; copy anything that must be
// retained. Duplicated messages share Payload and Data, which must therefore
// be treated as read-only.
type Message struct {
	From, To uint32
	Kind     uint16
	// ID is assigned by Send and is unique per run; a duplicate carries the
	// same ID with Dup set.
	ID uint64
	// Call is nonzero on a request sent by Call; pass the message to Reply.
	Call uint64
	// ReplyTo is the Call this message answers, zero otherwise.
	ReplyTo uint64
	Payload []byte
	Data    any
	// Size is the accounted size in bytes; zero means len(Payload).
	Size int
	// Sent is the simulated send time.
	Sent int64
	Dup  bool
}

// Handler receives messages delivered to members. Responses to Call are
// not passed to the Handler; they complete the call instead.
type Handler interface {
	Deliver(m *Message)
}

// Completer receives the single completion of a Call: either the response
// (err nil) or ErrTimeout.
type Completer interface {
	Complete(call uint64, resp *Message, err error)
}

// CompleterFunc adapts a function to Completer.
type CompleterFunc func(call uint64, resp *Message, err error)

// Complete calls f.
func (f CompleterFunc) Complete(call uint64, resp *Message, err error) { f(call, resp, err) }

// NetConfig configures a Net. Zero values select defaults.
type NetConfig struct {
	// Latency is the default one-way base latency. Default 1ms.
	Latency int64
	// Jitter is the default maximum extra latency, drawn uniformly. Default
	// Latency/2; set NoJitter for none. Jitter is what reorders messages.
	Jitter   int64
	NoJitter bool
	// LossPPM and DupPPM are the default loss and duplication
	// probabilities in parts per million.
	LossPPM uint32
	DupPPM  uint32
	// DropWhilePaused drops messages to a paused member instead of holding
	// them until it resumes.
	DropWhilePaused bool
}

// Link overrides the latency and loss of one directed link.
type Link struct {
	Latency int64
	Jitter  int64
	LossPPM uint32
}

// TrafficStats counts messages and bytes.
type TrafficStats struct {
	Msgs  uint64
	Bytes uint64
}

func (t *TrafficStats) add(size int) {
	t.Msgs++
	t.Bytes += uint64(size)
}

// KindStats counts traffic of one message kind.
type KindStats struct {
	Sent      TrafficStats
	Delivered TrafficStats
	Dropped   TrafficStats
}

// NetStats counts network-wide drops.
type NetStats struct {
	Sent, Delivered            uint64
	Lost, Partitioned, Paused  uint64
	Duplicated, LateResponses  uint64
	Timeouts, CallsCompleted   uint64
	HeldWhilePaused, Unhandled uint64
}

type pendingCall struct {
	done  Completer
	timer Handle
}

// Net delivers messages between members identified by dense uint32
// indices.
type Net struct {
	sim     *Sim
	rng     *rand.Rand
	h       Handler
	cfg     NetConfig
	links   map[uint64]Link
	slow    []uint32 // permille, 0 means 1000
	group   []uint32
	paused  []bool
	held    [][]uint32
	msgs    []Message
	free    []uint32
	nextID  uint64
	nextCal uint64
	calls   map[uint64]pendingCall
	timeout *netTimeout

	sent, recv []TrafficStats
	kinds      []KindStats
	stats      NetStats
}

type netTimeout struct{ n *Net }

func (t *netTimeout) Fire(call, _ uint64) { t.n.expire(call) }

// NewNet returns a network for members [0, members) delivering to h.
func NewNet(sim *Sim, members int, h Handler, cfg NetConfig) *Net {
	if cfg.Latency <= 0 {
		cfg.Latency = 1_000_000
	}

	if cfg.NoJitter {
		cfg.Jitter = 0
	} else if cfg.Jitter <= 0 {
		cfg.Jitter = cfg.Latency / 2
	}

	n := &Net{
		sim:   sim,
		rng:   sim.Stream("net"),
		h:     h,
		cfg:   cfg,
		links: make(map[uint64]Link),
		calls: make(map[uint64]pendingCall),
	}
	n.timeout = &netTimeout{n: n}
	n.Grow(members)

	return n
}

// Grow makes room for members [0, members).
func (n *Net) Grow(members int) {
	for len(n.slow) < members {
		n.slow = append(n.slow, 0)
		n.group = append(n.group, 0)
		n.paused = append(n.paused, false)
		n.held = append(n.held, nil)
		n.sent = append(n.sent, TrafficStats{})
		n.recv = append(n.recv, TrafficStats{})
	}
}

// Members returns the number of member slots.
func (n *Net) Members() int { return len(n.slow) }

// SetLoss sets the default loss probability in parts per million.
func (n *Net) SetLoss(ppm uint32) { n.cfg.LossPPM = ppm }

// SetDup sets the default duplication probability in parts per million.
func (n *Net) SetDup(ppm uint32) { n.cfg.DupPPM = ppm }

// SetLatency sets the default base latency and jitter.
func (n *Net) SetLatency(latency, jitter int64) {
	n.cfg.Latency, n.cfg.Jitter = latency, jitter
}

// SetLink overrides one directed link.
func (n *Net) SetLink(from, to uint32, l Link) { n.links[uint64(from)<<32|uint64(to)] = l }

// ClearLink removes a link override.
func (n *Net) ClearLink(from, to uint32) { delete(n.links, uint64(from)<<32|uint64(to)) }

// SetSlow multiplies the latency of every message to or from member by
// permille/1000. 1000 (or 0) is normal speed.
func (n *Net) SetSlow(member, permille uint32) { n.slow[member] = permille }

// SetGroup puts member in partition group g. Messages between different
// groups are dropped at delivery time. All members start in group 0.
func (n *Net) SetGroup(member, g uint32) { n.group[member] = g }

// Group returns member's partition group.
func (n *Net) Group(member uint32) uint32 { return n.group[member] }

// Heal puts every member back into group 0.
func (n *Net) Heal() { clear(n.group) }

// Paused reports whether member is paused.
func (n *Net) Paused(member uint32) bool { return n.paused[member] }

// Pause stops deliveries to member. Messages that arrive while it is paused
// are held (or dropped with DropWhilePaused).
func (n *Net) Pause(member uint32) { n.paused[member] = true }

// Resume delivers held messages to member, in arrival order, at the current
// time.
func (n *Net) Resume(member uint32) {
	if !n.paused[member] {
		return
	}

	n.paused[member] = false

	held := n.held[member]
	n.held[member] = nil

	for _, slot := range held {
		n.sim.AtTarget(n.sim.Now(), n, uint64(slot), 1)
	}
}

// DropHeld discards messages held for a paused member, for example when it
// crashes.
func (n *Net) DropHeld(member uint32) {
	for _, slot := range n.held[member] {
		n.drop(slot)
	}

	n.held[member] = nil
}

func (n *Net) kind(k uint16) *KindStats {
	for int(k) >= len(n.kinds) {
		n.kinds = append(n.kinds, KindStats{})
	}

	return &n.kinds[k]
}

func size(m *Message) int {
	if m.Size > 0 {
		return m.Size
	}

	return len(m.Payload)
}

func (n *Net) alloc(m *Message) uint32 {
	if k := len(n.free); k > 0 {
		slot := n.free[k-1]
		n.free = n.free[:k-1]
		n.msgs[slot] = *m

		return slot
	}

	n.msgs = append(n.msgs, *m)

	return uint32(len(n.msgs) - 1)
}

func (n *Net) release(slot uint32) {
	n.msgs[slot] = Message{}
	n.free = append(n.free, slot)
}

func (n *Net) drop(slot uint32) {
	m := &n.msgs[slot]
	n.kind(m.Kind).Dropped.add(size(m))
	n.release(slot)
}

func (n *Net) latency(from, to uint32) (int64, uint32) {
	base, jitter, loss := n.cfg.Latency, n.cfg.Jitter, n.cfg.LossPPM

	if len(n.links) > 0 {
		if l, ok := n.links[uint64(from)<<32|uint64(to)]; ok {
			base, jitter, loss = l.Latency, l.Jitter, l.LossPPM
		}
	}

	lat := base
	if jitter > 0 {
		lat += n.rng.Int64N(jitter + 1)
	}

	if s := n.slow[from]; s != 0 && s != 1000 {
		lat = lat * int64(s) / 1000
	}

	if s := n.slow[to]; s != 0 && s != 1000 {
		lat = lat * int64(s) / 1000
	}

	return lat, loss
}

func (n *Net) chance(ppm uint32) bool {
	return ppm > 0 && n.rng.Uint32N(1_000_000) < ppm
}

// Send sends m from m.From to m.To and returns its ID. m is copied.
func (n *Net) Send(m Message) uint64 {
	n.nextID++
	m.ID = n.nextID
	m.Sent = n.sim.Now()
	m.Dup = false
	sz := size(&m)

	n.stats.Sent++
	n.sent[m.From].add(sz)
	n.kind(m.Kind).Sent.add(sz)

	n.transmit(&m)

	if n.chance(n.cfg.DupPPM) {
		n.stats.Duplicated++
		m.Dup = true
		n.transmit(&m)
	}

	return m.ID
}

func (n *Net) transmit(m *Message) {
	lat, loss := n.latency(m.From, m.To)
	if n.chance(loss) {
		n.stats.Lost++
		n.kind(m.Kind).Dropped.add(size(m))

		return
	}

	slot := n.alloc(m)
	n.sim.AfterTarget(lat, n, uint64(slot), 0)
}

// Fire delivers the message in slot a. It implements Target; b is 1 for a
// redelivery of a message held while paused.
func (n *Net) Fire(a, _ uint64) {
	slot := uint32(a)
	m := &n.msgs[slot]

	if n.group[m.From] != n.group[m.To] {
		n.stats.Partitioned++
		n.drop(slot)

		return
	}

	if n.paused[m.To] {
		if n.cfg.DropWhilePaused {
			n.stats.Paused++
			n.drop(slot)

			return
		}

		n.stats.HeldWhilePaused++
		n.held[m.To] = append(n.held[m.To], slot)

		return
	}

	sz := size(m)
	n.stats.Delivered++
	n.recv[m.To].add(sz)
	n.kind(m.Kind).Delivered.add(sz)

	if m.ReplyTo != 0 {
		pc, ok := n.calls[m.ReplyTo]
		if !ok {
			n.stats.LateResponses++
		} else {
			delete(n.calls, m.ReplyTo)
			n.sim.Cancel(pc.timer)
			n.stats.CallsCompleted++

			pc.done.Complete(m.ReplyTo, m, nil)
		}
	} else if n.h != nil {
		n.h.Deliver(m)
	} else {
		n.stats.Unhandled++
	}

	n.release(slot)
}

// Call sends m as a request and arranges exactly one completion on done:
// the first response sent with Reply, or ErrTimeout after timeout
// nanoseconds. Later responses (including duplicates) are dropped. It
// returns the call ID.
func (n *Net) Call(m Message, timeout int64, done Completer) uint64 {
	n.nextCal++
	call := n.nextCal
	m.Call = call
	m.ReplyTo = 0
	n.calls[call] = pendingCall{done: done, timer: n.sim.AfterTarget(timeout, n.timeout, call, 0)}
	n.Send(m)

	return call
}

// Reply sends resp as the response to req. From, To and ReplyTo are filled
// in. Replying to a message that was not sent with Call sends nothing.
func (n *Net) Reply(req *Message, resp Message) {
	if req.Call == 0 {
		return
	}

	resp.From, resp.To = req.To, req.From
	resp.ReplyTo = req.Call
	resp.Call = 0
	n.Send(resp)
}

// CancelCall abandons a pending call without completing it. It reports
// whether the call was pending.
func (n *Net) CancelCall(call uint64) bool {
	pc, ok := n.calls[call]
	if !ok {
		return false
	}

	delete(n.calls, call)
	n.sim.Cancel(pc.timer)

	return true
}

func (n *Net) expire(call uint64) {
	pc, ok := n.calls[call]
	if !ok {
		return
	}

	delete(n.calls, call)
	n.stats.Timeouts++

	pc.done.Complete(call, nil, ErrTimeout)
}

// PendingCalls returns the number of calls awaiting completion.
func (n *Net) PendingCalls() int { return len(n.calls) }

// Stats returns network-wide counters.
func (n *Net) Stats() NetStats { return n.stats }

// Sent returns traffic sent by member.
func (n *Net) Sent(member uint32) TrafficStats { return n.sent[member] }

// Received returns traffic delivered to member.
func (n *Net) Received(member uint32) TrafficStats { return n.recv[member] }

// Kind returns traffic counters for one message kind.
func (n *Net) Kind(k uint16) KindStats {
	if int(k) < len(n.kinds) {
		return n.kinds[k]
	}

	return KindStats{}
}

// MaxSent returns the member that sent the most bytes and its counters.
func (n *Net) MaxSent() (uint32, TrafficStats) {
	var (
		best uint32
		bs   TrafficStats
	)

	for i, s := range n.sent {
		if s.Bytes > bs.Bytes {
			best, bs = uint32(i), s
		}
	}

	return best, bs
}
