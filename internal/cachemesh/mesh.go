// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cachemesh

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/elect"
	"github.com/Azure/unbounded/internal/cachemesh/internal/health"
	"github.com/Azure/unbounded/internal/cachemesh/internal/node"
	"github.com/Azure/unbounded/internal/cachemesh/internal/route"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
	"github.com/Azure/unbounded/internal/cachemesh/internal/wire"
)

var (
	// ErrNotRunning is returned by ServeControl and Leave when Run is not
	// active.
	ErrNotRunning = errors.New("cachemesh: mesh is not running")
	// ErrAlreadyRun is returned by a second call to Run.
	ErrAlreadyRun = errors.New("cachemesh: Run was already called")
	// ErrTooManyCalls is reported to the control plane for an outbound
	// call that exceeds Tuning.MaxInFlight. The protocol retries it.
	ErrTooManyCalls = errors.New("cachemesh: too many control calls in flight")
	// ErrNoLease is reported to the elector of a member without a Lease.
	ErrNoLease = errors.New("cachemesh: no lease store configured")
	// ErrMessageTooLarge is returned by ServeControl for a message longer
	// than MaxControlMessageSize.
	ErrMessageTooLarge = errors.New("cachemesh: control message too large")
	// ErrTooManyInvalidations is returned by Invalidate when
	// MaxPendingInvalidations keyspaces already have an invalidation
	// pending.
	ErrTooManyInvalidations = errors.New("cachemesh: too many pending invalidations")
)

const (
	// MaxControlMessageSize bounds a control message (request or
	// response), including view snapshots. Transports may use it to bound
	// the bodies they read.
	MaxControlMessageSize = wire.MaxMessageSize
	// MaxPendingInvalidations bounds the keyspaces with an invalidation
	// pending at once.
	MaxPendingInvalidations = 1024
)

const leaseQueue = 16

// Mesh is one cluster member. Create it with New; the zero value is not
// usable. All methods are safe for concurrent use. See the package
// documentation.
type Mesh struct {
	cfg   Config
	log   *slog.Logger
	start time.Time

	// Hot path, lock-free.
	st     atomic.Pointer[route.State]
	health *health.Tracker
	notes  *route.Notes

	leading atomic.Bool
	running atomic.Bool
	ran     atomic.Bool

	// Control plane, guarded by mu.
	mu       sync.Mutex
	n        *node.Node
	lastView *view.View
	lastSelf view.Slot
	isLeader bool
	peers    map[view.Slot]PeerRef // current and lingering neighbors
	env      *route.Env            // rebuilt when the gateways change
	gwSeeds  []string              // seeds the current gateways came from
	seedBuf  []string
	inval    map[uint64]*invalidation

	wake     chan struct{}
	left     chan struct{}
	leftOnce sync.Once
	done     chan struct{}
	sem      chan struct{}
	leaseQ   chan elect.Op

	evMu   sync.Mutex
	evQ    []event
	evWake chan struct{}
}

// event is a hook or watcher call queued for the notify goroutine.
type event struct {
	kind           eventKind
	added, removed []PeerRef
	version        Version
	n              int
	leading        bool
	term           uint64
}

type eventKind uint8

const (
	evNeighbors eventKind = iota
	evView
	evLeadership
)

// invalidation is a pending Invalidate of one keyspace.
type invalidation struct {
	haveBase bool
	base     uint32 // generation in the view when the request was made
	next     int64  // next (re)send, local monotonic time
	done     chan struct{}
}

// New returns a Mesh for cfg. It validates cfg and fills in defaults; the
// member does nothing until Run.
func New(cfg Config) (*Mesh, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	for cfg.Incarnation == 0 {
		cfg.Incarnation = randUint64()
	}

	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	t := &cfg.Tuning
	if t.MaxInFlight == 0 {
		t.MaxInFlight = DefaultMaxInFlight
	}

	if t.EpochAdvance == 0 {
		t.EpochAdvance = DefaultEpochAdvance
	}

	if t.LeaseTimeout == 0 {
		ec := t.Election.WithDefaults()
		t.LeaseTimeout = ec.OpTimeout
	}

	m := &Mesh{
		cfg:      cfg,
		log:      cfg.Logger.With("cachemesh.id", cfg.ID),
		start:    time.Now(),
		lastSelf: view.NoSlot,
		peers:    map[view.Slot]PeerRef{},
		inval:    map[uint64]*invalidation{},
		notes:    &route.Notes{},
		wake:     make(chan struct{}, 1),
		left:     make(chan struct{}),
		done:     make(chan struct{}),
		sem:      make(chan struct{}, t.MaxInFlight),
		leaseQ:   make(chan elect.Op, leaseQueue),
		evWake:   make(chan struct{}, 1),
	}

	nc := cfg.nodeConfig()
	nc.Hooks.OnNeighbors = m.onNeighbors
	nc.Hooks.OnView = func(v *view.View) {
		if cfg.Hooks.OnView != nil {
			m.queue(event{kind: evView, version: v.Version(), n: v.Len()})
		}
	}

	m.n = node.New(nc, rand.New(rand.NewPCG(randUint64(), randUint64())))
	m.health = m.n.Health()
	m.gwSeeds = m.n.Seeds(nil)
	m.env = m.newEnv(m.gwSeeds)
	m.st.Store(route.NewState(nil, view.NoSlot, m.env))

	return m, nil
}

// newEnv returns a routing Env whose gateways are seeds other than this
// member's own address.
func (m *Mesh) newEnv(seeds []string) *route.Env {
	gws := make([]PeerRef, 0, len(seeds))
	for _, s := range seeds {
		if s != m.cfg.Addr {
			gws = append(gws, PeerRef{Slot: view.NoSlot, ID: s, Addr: s})
		}
	}

	return route.NewEnv(route.Config{
		Health:          m.health,
		Epoch:           m.n.Epoch(),
		Notes:           m.notes,
		Gateways:        gws,
		SelfID:          m.cfg.ID,
		SelfIncarnation: m.cfg.Incarnation,
		PartitionBits:   m.cfg.PartitionBits,
	})
}

func randUint64() uint64 {
	var b [8]byte

	if _, err := crand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("cachemesh: crypto/rand: %v", err))
	}

	return binary.LittleEndian.Uint64(b[:])
}

// ID returns the member ID.
func (m *Mesh) ID() string { return m.cfg.ID }

// Incarnation returns the member incarnation (chosen at New when the
// config left it zero).
func (m *Mesh) Incarnation() uint64 { return m.cfg.Incarnation }

// Addr returns the control endpoint.
func (m *Mesh) Addr() Endpoint { return m.cfg.Addr }

// mono is node-local monotonic time: nanoseconds since New, plus one so it
// is never zero.
func (m *Mesh) mono() int64 { return int64(time.Since(m.start)) + 1 }

func (m *Mesh) poke() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// afterLocked refreshes everything derived from the node. Callers hold mu.
func (m *Mesh) afterLocked() {
	v, s := m.n.View(), m.n.Self()
	changed := v != m.lastView || s != m.lastSelf

	// While not a member, route through every seed the node knows,
	// including those learned from the Lease and from views.
	if s == view.NoSlot {
		m.seedBuf = m.n.Seeds(m.seedBuf[:0])
		if !slices.Equal(m.seedBuf, m.gwSeeds) {
			m.gwSeeds = slices.Clone(m.seedBuf)
			m.env = m.newEnv(m.gwSeeds)
			changed = true
		}
	}

	if changed {
		viewChanged := v != m.lastView
		m.lastView, m.lastSelf = v, s
		m.st.Store(route.NewState(v, s, m.env))

		if viewChanged && len(m.inval) > 0 {
			m.checkInvalidations(v)
		}
	}

	if leading, term := m.n.Leading(); leading != m.isLeader {
		m.isLeader = leading
		m.leading.Store(leading)
		m.log.Info("cachemesh leadership changed", "leading", leading, "term", term)

		if m.cfg.Hooks.OnLeadership != nil {
			m.queue(event{kind: evLeadership, leading: leading, term: term})
		}
	}

	if m.n.Left() {
		m.leftOnce.Do(func() { close(m.left) })
	}
}

// onNeighbors runs inside node methods (under mu). It maps slots to peers
// with the current view and queues the event for the watcher goroutine.
func (m *Mesh) onNeighbors(added, removed []view.Slot) {
	ev := event{kind: evNeighbors}

	v := m.n.View()

	for _, s := range removed {
		if p, ok := m.peers[s]; ok {
			delete(m.peers, s)

			ev.removed = append(ev.removed, p)
		}
	}

	for _, s := range added {
		mem, ok := v.Member(s)
		if !ok {
			continue
		}

		p := PeerRef{Slot: s, Key: mem.Key, ID: mem.ID, Addr: mem.Addr}
		if old, ok := m.peers[s]; ok {
			if old == p {
				continue
			}

			// Slot reused by another member: the old one is gone at once.
			ev.removed = append(ev.removed, old)
		}

		m.peers[s] = p
		ev.added = append(ev.added, p)
	}

	if m.cfg.Neighbors == nil || (len(ev.added) == 0 && len(ev.removed) == 0) {
		return
	}

	m.queue(ev)
}

// queue appends ev for the notify goroutine. Consecutive view events
// coalesce into the latest, so a slow OnView hook sees fewer calls rather
// than a growing backlog.
func (m *Mesh) queue(ev event) {
	m.evMu.Lock()
	if n := len(m.evQ); ev.kind == evView && n > 0 && m.evQ[n-1].kind == evView {
		m.evQ[n-1] = ev
	} else {
		m.evQ = append(m.evQ, ev)
	}
	m.evMu.Unlock()

	select {
	case m.evWake <- struct{}{}:
	default:
	}
}

// notifyLoop delivers queued events in order, one at a time, without
// holding the Mesh lock, so watchers and hooks may call Mesh methods.
func (m *Mesh) notifyLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.evWake:
		}

		m.evMu.Lock()
		q := m.evQ
		m.evQ = nil
		m.evMu.Unlock()

		for _, ev := range q {
			if ctx.Err() != nil {
				return
			}

			switch ev.kind {
			case evNeighbors:
				m.cfg.Neighbors.OnNeighbors(ev.added, ev.removed)
			case evView:
				m.cfg.Hooks.OnView(ev.version, ev.n)
			case evLeadership:
				m.cfg.Hooks.OnLeadership(ev.leading, ev.term)
			}
		}
	}
}

// Run starts the member and drives it until ctx is canceled or a Leave
// completes. It returns nil after a completed Leave and ctx.Err() on
// cancellation. Run may be called only once.
func (m *Mesh) Run(ctx context.Context) error {
	if !m.ran.CompareAndSwap(false, true) {
		return ErrAlreadyRun
	}

	ctx, cancel := context.WithCancel(ctx)

	var wg sync.WaitGroup

	defer func() {
		m.running.Store(false)
		cancel()
		wg.Wait()
		close(m.done)
	}()

	wg.Add(2)

	go func() {
		defer wg.Done()

		m.leaseLoop(ctx)
	}()

	go func() {
		defer wg.Done()

		m.notifyLoop(ctx)
	}()

	m.mu.Lock()
	m.n.Start(m.mono(), time.Now())
	m.afterLocked()
	m.mu.Unlock()
	m.running.Store(true)
	m.log.Info("cachemesh member started", "addr", m.cfg.Addr, "incarnation", m.cfg.Incarnation)

	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		m.mu.Lock()
		now := m.mono()
		m.n.Tick(now, time.Now())

		if note, ok := m.notes.Take(); ok {
			m.n.NoteVersion(now, note.Sender, expandVersion(m.n.View().Version(), note))
		}

		m.resendInvalidations(now)

		calls := m.n.Outbox()
		ops := m.n.LeaseOps()
		m.afterLocked()
		next := m.n.NextDeadline()
		left := m.n.Left()
		m.mu.Unlock()

		m.dispatch(ctx, &wg, calls)
		m.submitLease(ops)

		if left {
			m.log.Info("cachemesh member left")
			return nil
		}

		wait := min(time.Duration(next-now), m.cfg.Tuning.EpochAdvance)
		timer.Reset(max(wait, 0))

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-m.wake:
		case <-timer.C:
		}
	}
}

// expandVersion widens a header note's 32-bit term using own as reference.
func expandVersion(own view.Version, n route.Note) view.Version {
	t := own.Term&^0xffffffff | uint64(n.Term)
	if n.Term < uint32(own.Term) {
		t += 1 << 32
	}

	return view.Version{Term: t, Seq: n.Seq}
}

func (m *Mesh) dispatch(ctx context.Context, wg *sync.WaitGroup, calls []node.Call) {
	var refused []node.Call

	for _, c := range calls {
		select {
		case m.sem <- struct{}{}:
		default:
			refused = append(refused, c)
			continue
		}

		wg.Add(1)

		go func(c node.Call) {
			defer wg.Done()
			defer func() { <-m.sem }()

			timeout := c.Timeout
			if timeout <= 0 {
				timeout = node.DefaultCallTimeout
			}

			cctx, cancel := context.WithTimeout(ctx, timeout)
			resp, err := m.cfg.Transport.Send(cctx, c.To, c.Msg)

			cancel()

			if err != nil {
				m.controlError(c.To, err)
			}

			m.mu.Lock()
			m.n.OnReply(m.mono(), c.ID, resp, err)
			m.afterLocked()
			m.mu.Unlock()
			m.poke()
		}(c)
	}

	if len(refused) == 0 {
		return
	}

	for _, c := range refused {
		m.controlError(c.To, ErrTooManyCalls)
	}

	m.mu.Lock()
	for _, c := range refused {
		m.n.OnReply(m.mono(), c.ID, nil, ErrTooManyCalls)
	}

	m.afterLocked()
	m.mu.Unlock()
	m.poke()
}

func (m *Mesh) controlError(addr string, err error) {
	if h := m.cfg.Hooks.OnControlError; h != nil {
		h(addr, err)
	}

	m.log.Debug("cachemesh control call failed", "to", addr, "err", err)
}

func (m *Mesh) submitLease(ops []elect.Op) {
	var refused []elect.Op

	for _, op := range ops {
		if m.cfg.Lease == nil {
			refused = append(refused, op)
			continue
		}

		select {
		case m.leaseQ <- op:
		default:
			refused = append(refused, op)
		}
	}

	if len(refused) == 0 {
		return
	}

	m.mu.Lock()
	for _, op := range refused {
		m.n.OnLease(m.mono(), time.Now(), elect.Result{Seq: op.Seq, Kind: op.Kind, Err: ErrNoLease})
	}

	m.afterLocked()
	m.mu.Unlock()
}

// leaseLoop runs Lease operations one at a time.
func (m *Mesh) leaseLoop(ctx context.Context) {
	for {
		var op elect.Op

		select {
		case <-ctx.Done():
			return
		case op = <-m.leaseQ:
		}

		lctx, cancel := context.WithTimeout(ctx, m.cfg.Tuning.LeaseTimeout)
		r := elect.Execute(lctx, m.cfg.Lease, op)

		cancel()

		if r.Err != nil && !errors.Is(r.Err, elect.ErrConflict) && !errors.Is(r.Err, elect.ErrNotFound) {
			if h := m.cfg.Hooks.OnLeaseError; h != nil {
				h(op.Kind.String(), r.Err)
			}

			m.log.Warn("cachemesh lease operation failed", "op", op.Kind.String(), "err", r.Err)
		}

		m.mu.Lock()
		m.n.OnLease(m.mono(), time.Now(), r)
		m.afterLocked()
		m.mu.Unlock()
		m.poke()
	}
}

// ServeControl handles one inbound control request from the transport
// address from and returns the response to send back. It is safe for
// concurrent use, does not block on the network, and returns ErrNotRunning
// unless Run is active and ErrMessageTooLarge for a message longer than
// MaxControlMessageSize. Malformed or hostile messages are answered with
// an encoded rejection, not an error, so the transport just returns the
// response. from is informational (the sender's own Addr is inside the
// message). msg is not retained after ServeControl returns, and the
// response is a new slice owned by the caller.
func (m *Mesh) ServeControl(_ context.Context, from Endpoint, msg []byte) ([]byte, error) {
	if !m.running.Load() {
		return nil, ErrNotRunning
	}

	if len(msg) > MaxControlMessageSize {
		return nil, ErrMessageTooLarge
	}

	m.mu.Lock()
	resp := m.n.Handle(m.mono(), from, msg)
	m.afterLocked()
	m.mu.Unlock()
	m.poke()

	return resp, nil
}

// Invalidate asks the leader to bump the generation of ks, which makes
// every Stamp of ks issued before the bump stale (Fresh returns false) on
// each member once it sees the new view. It waits until this member's view
// shows the bump and then returns nil.
//
// The request is queued and sent to the leader every Tuning.JoinRetry
// until the bump is seen, so it survives a missing or failing leader.
// Calls for a keyspace that already has a request pending share it: one
// bump completes them all. A retry may cause an extra bump, which only
// makes values stale earlier. Before this member has a view the request
// waits for one, and the bump must then be seen in a later view.
//
// If ctx ends first Invalidate returns ctx.Err(), but the request stays
// queued and is still delivered while Run is active. It returns
// ErrNotRunning unless Run is active (or when Run returns while waiting),
// and ErrTooManyInvalidations when MaxPendingInvalidations other keyspaces
// are pending. It is safe for concurrent use.
func (m *Mesh) Invalidate(ctx context.Context, ks Keyspace) error {
	if !m.running.Load() {
		return ErrNotRunning
	}

	m.mu.Lock()
	inv, ok := m.inval[ks.seed]

	if !ok {
		if len(m.inval) >= MaxPendingInvalidations {
			m.mu.Unlock()
			return ErrTooManyInvalidations
		}

		inv = &invalidation{done: make(chan struct{})}
		if v := m.n.View(); hasConfig(v) {
			inv.haveBase, inv.base = true, v.Gen(ks.seed)
		}

		m.inval[ks.seed] = inv
		m.resendInvalidations(m.mono())
		m.afterLocked()
	}

	m.mu.Unlock()
	m.poke()

	select {
	case <-inv.done:
		return nil
	case <-m.done:
		return ErrNotRunning
	case <-ctx.Done():
		return ctx.Err()
	}
}

func hasConfig(v *view.View) bool {
	_, ok := v.Config()
	return ok
}

// checkInvalidations completes pending invalidations whose generation
// moved in v. Callers hold mu.
func (m *Mesh) checkInvalidations(v *view.View) {
	if !hasConfig(v) {
		return
	}

	for seed, inv := range m.inval {
		switch g := v.Gen(seed); {
		case !inv.haveBase:
			inv.haveBase, inv.base = true, g
		case g != inv.base:
			close(inv.done)
			delete(m.inval, seed)
		}
	}
}

// resendInvalidations sends due pending invalidations to the leader.
// Callers hold mu.
func (m *Mesh) resendInvalidations(now int64) {
	retry := m.cfg.Tuning.JoinRetry
	if retry <= 0 {
		retry = node.DefaultJoinRetry
	}

	for seed, inv := range m.inval {
		if inv.haveBase && inv.next <= now {
			m.n.Invalidate(now, seed)
			inv.next = now + int64(retry)
		}
	}
}

// Leave starts a graceful departure and waits until this member has
// observed its own removal (after which Run returns nil), Run stops, or ctx
// is done. The member keeps serving while it drains. It returns nil once
// the member has left, ErrNotRunning if Run is not active or returns
// first, and ctx.Err() if ctx ends first, in which case the departure
// continues in the background.
func (m *Mesh) Leave(ctx context.Context) error {
	if !m.running.Load() {
		return ErrNotRunning
	}

	m.mu.Lock()
	m.n.Leave(m.mono())
	m.afterLocked()
	m.mu.Unlock()
	m.poke()

	select {
	case <-m.left:
		return nil
	case <-m.done:
		select {
		case <-m.left:
			return nil
		default:
			return ErrNotRunning
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Done is closed when Run has returned.
func (m *Mesh) Done() <-chan struct{} { return m.done }

// --- Status ---

// Version returns the version of this member's view.
func (m *Mesh) Version() Version { return m.st.Load().View().Version() }

// Members returns the member count of this member's view.
func (m *Mesh) Members() int { return m.st.Load().View().Len() }

// Leader returns the leader named by this member's view.
func (m *Mesh) Leader() (PeerRef, bool) {
	st := m.st.Load()

	v := st.View()
	if v.Term() == 0 {
		return PeerRef{}, false
	}

	return st.Peer(v.Leader())
}

// Self returns this member's own PeerRef when it is in its view.
func (m *Mesh) Self() (PeerRef, bool) {
	st := m.st.Load()
	if !st.Member() {
		return PeerRef{}, false
	}

	return st.Peer(st.Self())
}

// Leading reports whether this member currently publishes as leader.
func (m *Mesh) Leading() bool { return m.leading.Load() }

// Ready reports whether this member is Active in its view, that is, it has
// been admitted and has finished quarantine.
func (m *Mesh) Ready() bool {
	st := m.st.Load()

	return st.Member() && st.View().State(st.Self()) == view.Active
}

// Neighbors returns the current data-plane neighbors (without lingering
// ones) in a new slice. It takes the Mesh lock; prefer a NeighborWatcher
// to track changes.
func (m *Mesh) Neighbors() []PeerRef {
	m.mu.Lock()
	defer m.mu.Unlock()

	v := m.n.View()

	var out []PeerRef

	for _, s := range m.n.Neighbors(nil) {
		if mem, ok := v.Member(s); ok {
			out = append(out, PeerRef{Slot: s, Key: mem.Key, ID: mem.ID, Addr: mem.Addr})
		}
	}

	return out
}
