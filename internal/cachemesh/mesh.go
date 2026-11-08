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
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/elect"
	"github.com/Azure/unbounded/internal/cachemesh/internal/health"
	"github.com/Azure/unbounded/internal/cachemesh/internal/node"
	"github.com/Azure/unbounded/internal/cachemesh/internal/route"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
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
)

const leaseQueue = 16

// Mesh is one cluster member. See the package documentation.
type Mesh struct {
	cfg   Config
	log   *slog.Logger
	start time.Time

	// Hot path, lock-free.
	st     atomic.Pointer[route.State]
	health *health.Tracker
	env    *route.Env

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

	wake     chan struct{}
	left     chan struct{}
	leftOnce sync.Once
	done     chan struct{}
	sem      chan struct{}
	leaseQ   chan elect.Op

	nbMu   sync.Mutex
	nbQ    []nbEvent
	nbWake chan struct{}
}

type nbEvent struct{ added, removed []PeerRef }

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
		wake:     make(chan struct{}, 1),
		left:     make(chan struct{}),
		done:     make(chan struct{}),
		sem:      make(chan struct{}, t.MaxInFlight),
		leaseQ:   make(chan elect.Op, leaseQueue),
		nbWake:   make(chan struct{}, 1),
	}

	nc := cfg.nodeConfig()
	nc.Hooks.OnNeighbors = m.onNeighbors
	nc.Hooks.OnView = func(v *view.View) {
		if h := cfg.Hooks.OnView; h != nil {
			h(v.Version(), v.Len())
		}
	}

	m.n = node.New(nc, rand.New(rand.NewPCG(randUint64(), randUint64())))
	m.health = m.n.Health()

	gws := make([]PeerRef, 0, len(cfg.Seeds))
	for _, s := range cfg.Seeds {
		if s != cfg.Addr {
			gws = append(gws, PeerRef{Slot: view.NoSlot, ID: s, Addr: s})
		}
	}

	m.env = route.NewEnv(route.Config{
		Health:          m.health,
		Epoch:           m.n.Epoch(),
		Notes:           &route.Notes{},
		Gateways:        gws,
		SelfID:          cfg.ID,
		SelfIncarnation: cfg.Incarnation,
		PartitionBits:   cfg.PartitionBits,
	})
	m.st.Store(route.NewState(nil, view.NoSlot, m.env))

	return m, nil
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
	if v, s := m.n.View(), m.n.Self(); v != m.lastView || s != m.lastSelf {
		m.lastView, m.lastSelf = v, s
		m.st.Store(route.NewState(v, s, m.env))
	}

	if leading, term := m.n.Leading(); leading != m.isLeader {
		m.isLeader = leading
		m.leading.Store(leading)
		m.log.Info("cachemesh leadership changed", "leading", leading, "term", term)

		if h := m.cfg.Hooks.OnLeadership; h != nil {
			h(leading, term)
		}
	}

	if m.n.Left() {
		m.leftOnce.Do(func() { close(m.left) })
	}
}

// onNeighbors runs inside node methods (under mu). It maps slots to peers
// with the current view and queues the event for the watcher goroutine.
func (m *Mesh) onNeighbors(added, removed []view.Slot) {
	var ev nbEvent

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

	m.nbMu.Lock()
	m.nbQ = append(m.nbQ, ev)
	m.nbMu.Unlock()

	select {
	case m.nbWake <- struct{}{}:
	default:
	}
}

func (m *Mesh) notifyLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.nbWake:
		}

		m.nbMu.Lock()
		q := m.nbQ
		m.nbQ = nil
		m.nbMu.Unlock()

		for _, ev := range q {
			m.cfg.Neighbors.OnNeighbors(ev.added, ev.removed)
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

		if m.cfg.Neighbors != nil {
			m.notifyLoop(ctx)
		}
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

		if note, ok := m.env.Notes().Take(); ok {
			m.n.NoteVersion(now, note.Sender, expandVersion(m.n.View().Version(), note))
		}

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
// concurrent use and returns ErrNotRunning unless Run is active. msg is not
// retained after ServeControl returns.
func (m *Mesh) ServeControl(_ context.Context, from Endpoint, msg []byte) ([]byte, error) {
	if !m.running.Load() {
		return nil, ErrNotRunning
	}

	m.mu.Lock()
	resp := m.n.Handle(m.mono(), from, msg)
	m.afterLocked()
	m.mu.Unlock()
	m.poke()

	return resp, nil
}

// Invalidate asks the leader to bump the generation of ks, which makes
// every Stamp issued for ks before the bump stale once members see the new
// view. It is fire-and-forget: if no leader is known the request is
// dropped, and it is not retried.
func (m *Mesh) Invalidate(ks Keyspace) {
	m.mu.Lock()
	m.n.Invalidate(m.mono(), ks.seed)
	m.afterLocked()
	m.mu.Unlock()
	m.poke()
}

// Leave starts a graceful departure and waits until this member has
// observed its own removal (after which Run returns nil), Run stops, or ctx
// is done. The member keeps serving while it drains.
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
// ones).
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
