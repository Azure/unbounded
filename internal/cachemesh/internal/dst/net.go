// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package dst

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"

	"github.com/Azure/unbounded/internal/cachemesh/internal/elect"
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
}

// NetStats counts network-wide drops.
type NetStats struct {
	Sent, Delivered            uint64
	Lost, Partitioned          uint64
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
	stats   NetStats
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
		calls: make(map[uint64]pendingCall),
	}
	n.timeout = &netTimeout{n: n}
	n.slow = make([]uint32, members)
	n.group = make([]uint32, members)
	n.paused = make([]bool, members)
	n.held = make([][]uint32, members)

	return n
}

// SetLoss sets the default loss probability in parts per million.
func (n *Net) SetLoss(ppm uint32) { n.cfg.LossPPM = ppm }

// SetDup sets the default duplication probability in parts per million.
func (n *Net) SetDup(ppm uint32) { n.cfg.DupPPM = ppm }

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
// are held until it resumes.
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

func (n *Net) drop(slot uint32) { n.release(slot) }

func (n *Net) latency(from, to uint32) int64 {
	lat := n.cfg.Latency
	if n.cfg.Jitter > 0 {
		lat += n.rng.Int64N(n.cfg.Jitter + 1)
	}

	if s := n.slow[from]; s != 0 && s != 1000 {
		lat = lat * int64(s) / 1000
	}

	if s := n.slow[to]; s != 0 && s != 1000 {
		lat = lat * int64(s) / 1000
	}

	return lat
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
	n.stats.Sent++

	n.transmit(&m)

	if n.chance(n.cfg.DupPPM) {
		n.stats.Duplicated++
		m.Dup = true
		n.transmit(&m)
	}

	return m.ID
}

func (n *Net) transmit(m *Message) {
	lat := n.latency(m.From, m.To)
	if n.chance(n.cfg.LossPPM) {
		n.stats.Lost++

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
		n.stats.HeldWhilePaused++
		n.held[m.To] = append(n.held[m.To], slot)

		return
	}

	n.stats.Delivered++

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

// ErrUnavailable is the transient error returned during an API server
// outage.
var ErrUnavailable = errors.New("dst: lease server unavailable")

// NoMember attributes a Lease operation to no member.
const NoMember = ^uint32(0)

// LeaseConfig configures a LeaseServer. Zero values select defaults.
type LeaseConfig struct {
	// Latency is the base latency of an asynchronous operation. Default 5ms.
	Latency int64
	// Jitter is the maximum extra latency, drawn uniformly. Default
	// Latency.
	Jitter int64
	// ConflictPPM is the probability, in parts per million, that a create or
	// update fails with an injected ErrConflict.
	ConflictPPM uint32
}

// Window is a half-open simulated time interval [Start, End).
type Window struct{ Start, End int64 }

// Write is one successful Lease write, for invariant checks.
type Write struct {
	Time        int64
	Member      uint32
	Kind        elect.OpKind
	Holder      string
	Incarnation uint64
	Term        uint64
	RV          uint64
}

// LeaseServer is an in-memory Lease with API server semantics: reads,
// create, compare-and-swap update, server-assigned resourceVersion and UID,
// latency, outages, and injected conflicts. Cached reads are always fresh.
// Callers keep their own operation counts (see control.go).
type LeaseServer struct {
	sim     *Sim
	rng     *rand.Rand
	cfg     LeaseConfig
	exists  bool
	cur     elect.Record
	rv      uint64
	uids    uint64
	outages []Window
	audit   []Write
}

// NewLeaseServer returns a server whose Lease does not exist yet.
func NewLeaseServer(sim *Sim, cfg LeaseConfig) *LeaseServer {
	if cfg.Latency <= 0 {
		cfg.Latency = 5_000_000
	}

	if cfg.Jitter <= 0 {
		cfg.Jitter = cfg.Latency
	}

	return &LeaseServer{
		sim: sim,
		rng: sim.Stream("lease"),
		cfg: cfg,
	}
}

// SetConflictPPM changes the injected conflict probability.
func (l *LeaseServer) SetConflictPPM(ppm uint32) { l.cfg.ConflictPPM = ppm }

// AddOutage makes every operation in [start, end) fail with ErrUnavailable.
func (l *LeaseServer) AddOutage(start, end int64) {
	l.outages = append(l.outages, Window{start, end})
}

func (l *LeaseServer) down(now int64) bool {
	for _, w := range l.outages {
		if now >= w.Start && now < w.End {
			return true
		}
	}

	return false
}

// Audit returns the successful writes in order.
func (l *LeaseServer) Audit() []Write { return l.audit }

// Do executes op at simulated time now on behalf of member.
func (l *LeaseServer) Do(member uint32, op elect.Op, now int64) elect.Result {
	res := elect.Result{Seq: op.Seq, Kind: op.Kind}

	if l.down(now) {
		res.Err = ErrUnavailable

		return res
	}

	switch op.Kind {
	case elect.OpGet, elect.OpGetCached:
		if !l.exists {
			res.Err = elect.ErrNotFound

			return res
		}

		res.Record = l.cur.Clone()
	case elect.OpCreate:
		if l.exists || l.injectConflict() {
			res.Err = elect.ErrConflict

			return res
		}

		l.uids++
		r := op.Record.Clone()
		r.UID = "uid-" + strconv.FormatUint(l.uids, 10)
		res.Record = l.write(now, member, op.Kind, r)
	case elect.OpUpdate:
		if !l.exists {
			res.Err = elect.ErrNotFound

			return res
		}

		if op.Record.ResourceVersion != l.cur.ResourceVersion ||
			(op.Record.UID != "" && op.Record.UID != l.cur.UID) || l.injectConflict() {
			res.Err = elect.ErrConflict

			return res
		}

		r := op.Record.Clone()
		r.UID = l.cur.UID
		res.Record = l.write(now, member, op.Kind, r)
	default:
		res.Err = errors.New("dst: unknown lease op")
	}

	return res
}

func (l *LeaseServer) injectConflict() bool {
	return l.cfg.ConflictPPM > 0 && l.rng.Uint32N(1_000_000) < l.cfg.ConflictPPM
}

func (l *LeaseServer) write(now int64, member uint32, kind elect.OpKind, r elect.Record) elect.Record {
	l.rv++
	r.ResourceVersion = strconv.FormatUint(l.rv, 10)
	l.exists, l.cur = true, r
	l.audit = append(l.audit, Write{
		Time: now, Member: member, Kind: kind,
		Holder: r.Holder, Incarnation: r.Incarnation, Term: r.Term, RV: l.rv,
	})

	return r.Clone()
}

// Submit executes op after a random latency on behalf of member and passes
// the result to done. The operation takes effect at the server at the
// midpoint of the latency, and the response arrives at the end.
func (l *LeaseServer) Submit(member uint32, op elect.Op, done func(elect.Result)) {
	lat := l.cfg.Latency + l.rng.Int64N(l.cfg.Jitter+1)
	l.sim.After(lat/2, func() {
		res := l.Do(member, op, l.sim.Now())
		l.sim.After(lat-lat/2, func() { done(res) })
	})
}

// CheckAudit verifies the write log: terms never decrease, a change of
// holder or incarnation strictly increases the term, and no term has two
// holders. Releases (empty Holder) are allowed at the current term.
func CheckAudit(ws []Write) error {
	type who struct {
		holder string
		inc    uint64
	}

	owner := make(map[uint64]who)

	var prev *Write

	for i := range ws {
		w := &ws[i]
		if w.Holder != "" {
			cur := who{w.Holder, w.Incarnation}
			if o, ok := owner[w.Term]; ok && o != cur {
				return fmt.Errorf("term %d held by %s/%d and %s/%d (rv %d)",
					w.Term, o.holder, o.inc, w.Holder, w.Incarnation, w.RV)
			}

			owner[w.Term] = cur
		}

		if prev != nil {
			if w.Term < prev.Term {
				return fmt.Errorf("term decreased from %d to %d at rv %d", prev.Term, w.Term, w.RV)
			}

			changed := w.Holder != "" && prev.Holder != "" &&
				(w.Holder != prev.Holder || w.Incarnation != prev.Incarnation)
			if changed && w.Term <= prev.Term {
				return fmt.Errorf("holder changed %s/%d -> %s/%d without a new term at rv %d",
					prev.Holder, prev.Incarnation, w.Holder, w.Incarnation, w.RV)
			}
		}

		prev = w
	}

	return nil
}

// State is a simulated process's lifecycle state.
type State uint8

const (
	// Down means the process has never started or has finished leaving.
	Down State = iota
	// Starting means the process has started but not yet reported ready.
	Starting
	// Up means the process is running.
	Up
	// Paused means the process is frozen (stop the world).
	Paused
	// Leaving means the process is draining before a graceful exit.
	Leaving
	// Crashed means the process died without cleanup.
	Crashed
)

// String returns the state name.
func (s State) String() string {
	switch s {
	case Down:
		return "down"
	case Starting:
		return "starting"
	case Up:
		return "up"
	case Paused:
		return "paused"
	case Leaving:
		return "leaving"
	case Crashed:
		return "crashed"
	default:
		return "unknown"
	}
}

// Alive reports whether the process is running (possibly paused).
func (s State) Alive() bool { return s == Starting || s == Up || s == Paused || s == Leaving }

// Hooks are called by Processes on lifecycle transitions. The harness uses
// them to build or tear down the member under test.
type Hooks interface {
	// OnStart is called when member starts with a new incarnation.
	OnStart(member uint32, incarnation uint64)
	// OnCrash is called when member dies without cleanup.
	OnCrash(member uint32)
	// OnLeave is called when member begins a graceful exit.
	OnLeave(member uint32)
	// OnPause and OnResume freeze and unfreeze member.
	OnPause(member uint32)
	OnResume(member uint32)
}

// Process is one member's lifecycle record.
type Process struct {
	State       State
	Incarnation uint64
	// Starts counts how many times the process started.
	Starts uint32
	// Since is the simulated time of the last transition.
	Since int64
	// pausedFrom is the state to return to on Resume.
	pausedFrom State
}

// Processes is a table of simulated processes keyed by member index.
type Processes struct {
	sim   *Sim
	rng   *rand.Rand
	hooks Hooks
	procs []Process
}

// NewProcesses returns a table of members processes, all Down.
func NewProcesses(sim *Sim, members int, hooks Hooks) *Processes {
	return &Processes{sim: sim, rng: sim.Stream("process"), hooks: hooks, procs: make([]Process, members)}
}

// Get returns member's record.
func (p *Processes) Get(member uint32) Process { return p.procs[member] }

// State returns member's state.
func (p *Processes) State(member uint32) State { return p.procs[member].State }

func (p *Processes) set(m uint32, s State) {
	p.procs[m].State = s
	p.procs[m].Since = p.sim.Now()
}

// Start starts a Down or Crashed member with a fresh nonzero incarnation
// and calls OnStart. It reports whether the member started.
func (p *Processes) Start(member uint32) bool {
	pr := &p.procs[member]
	if pr.State != Down && pr.State != Crashed {
		return false
	}

	inc := p.rng.Uint64()
	for inc == 0 || inc == pr.Incarnation {
		inc = p.rng.Uint64()
	}

	pr.Incarnation = inc
	pr.Starts++

	p.set(member, Starting)

	if p.hooks != nil {
		p.hooks.OnStart(member, inc)
	}

	return true
}

// MarkUp moves a Starting member to Up.
func (p *Processes) MarkUp(member uint32) bool {
	if p.procs[member].State != Starting {
		return false
	}

	p.set(member, Up)

	return true
}

// Crash kills a live member and calls OnCrash.
func (p *Processes) Crash(member uint32) bool {
	if !p.procs[member].State.Alive() {
		return false
	}

	p.set(member, Crashed)

	if p.hooks != nil {
		p.hooks.OnCrash(member)
	}

	return true
}

// Leave begins a graceful exit of a Starting or Up member and calls
// OnLeave. Call Exited when it finishes.
func (p *Processes) Leave(member uint32) bool {
	s := p.procs[member].State
	if s != Up && s != Starting {
		return false
	}

	p.set(member, Leaving)

	if p.hooks != nil {
		p.hooks.OnLeave(member)
	}

	return true
}

// Exited moves a Leaving member to Down.
func (p *Processes) Exited(member uint32) bool {
	if p.procs[member].State != Leaving {
		return false
	}

	p.set(member, Down)

	return true
}

// Pause freezes a Starting, Up or Leaving member and calls OnPause.
func (p *Processes) Pause(member uint32) bool {
	pr := &p.procs[member]
	if pr.State != Up && pr.State != Starting && pr.State != Leaving {
		return false
	}

	pr.pausedFrom = pr.State

	p.set(member, Paused)

	if p.hooks != nil {
		p.hooks.OnPause(member)
	}

	return true
}

// Resume unfreezes a Paused member and calls OnResume.
func (p *Processes) Resume(member uint32) bool {
	pr := &p.procs[member]
	if pr.State != Paused {
		return false
	}

	p.set(member, pr.pausedFrom)

	if p.hooks != nil {
		p.hooks.OnResume(member)
	}

	return true
}

// Live appends the members that are alive (Starting, Up, Paused, or
// Leaving) to dst in index order and returns it.
func (p *Processes) Live(dst []uint32) []uint32 {
	for i := range p.procs {
		if p.procs[i].State.Alive() {
			dst = append(dst, uint32(i))
		}
	}

	return dst
}

// InState appends the members in state s to dst in index order.
func (p *Processes) InState(dst []uint32, s State) []uint32 {
	for i := range p.procs {
		if p.procs[i].State == s {
			dst = append(dst, uint32(i))
		}
	}

	return dst
}
