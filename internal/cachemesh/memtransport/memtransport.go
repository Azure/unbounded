// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package memtransport connects cachemesh members in one process, for tests
// and examples. A Network routes control messages between registered
// handlers (typically Mesh.ServeControl) with optional latency, message
// loss, partitions, and crashes. LeaseStore is an in-memory leader Lease
// with the same compare-and-swap semantics as kubelease.Store.
package memtransport

import (
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh"
)

var (
	// ErrUnreachable is returned when the target is not registered or is
	// in another partition group.
	ErrUnreachable = errors.New("memtransport: unreachable")
	// ErrSenderDown is returned when the sender is not registered (it
	// crashed).
	ErrSenderDown = errors.New("memtransport: sender is not registered")
)

// Handler serves one control request, like Mesh.ServeControl.
type Handler func(ctx context.Context, from string, msg []byte) ([]byte, error)

// Network is an in-memory control network. It is safe for concurrent use.
type Network struct {
	mu       sync.RWMutex
	handlers map[string]Handler
	groups   map[string]int
	latency  time.Duration
	jitter   time.Duration
	drop     float64

	rngMu sync.Mutex
	rng   *rand.Rand
}

// Option configures a Network.
type Option func(*Network)

// WithLatency delays every message by d plus a uniform random jitter in
// [0, jitter).
func WithLatency(d, jitter time.Duration) Option {
	return func(n *Network) { n.latency, n.jitter = d, jitter }
}

// WithDropRate drops each request with probability p. A dropped request
// blocks until the caller's context is done, like a lost packet.
func WithDropRate(p float64) Option {
	return func(n *Network) { n.drop = p }
}

// WithSeed seeds the random source used for jitter and drops.
func WithSeed(seed uint64) Option {
	return func(n *Network) { n.rng = rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)) }
}

// NewNetwork returns an empty Network.
func NewNetwork(opts ...Option) *Network {
	n := &Network{handlers: map[string]Handler{}, groups: map[string]int{}}
	WithSeed(1)(n)

	for _, o := range opts {
		o(n)
	}

	return n
}

// Register makes addr reachable, served by h. It replaces any previous
// handler (a restart).
func (n *Network) Register(addr string, h Handler) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.handlers[addr] = h
}

// Unregister makes addr unreachable, simulating a crash: requests to it
// fail and requests from it fail.
func (n *Network) Unregister(addr string) {
	n.mu.Lock()
	defer n.mu.Unlock()

	delete(n.handlers, addr)
}

// Partition splits the listed addresses into groups that cannot reach each
// other. Addresses not listed can reach everyone. It replaces any previous
// partition.
func (n *Network) Partition(groups ...[]string) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.groups = map[string]int{}

	for i, g := range groups {
		for _, a := range g {
			n.groups[a] = i + 1
		}
	}
}

// Heal removes any partition.
func (n *Network) Heal() { n.Partition() }

// SetLatency changes the latency; see WithLatency.
func (n *Network) SetLatency(d, jitter time.Duration) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.latency, n.jitter = d, jitter
}

// SetDropRate changes the drop rate; see WithDropRate.
func (n *Network) SetDropRate(p float64) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.drop = p
}

// Addrs returns the registered addresses in sorted order.
func (n *Network) Addrs() []string {
	n.mu.RLock()
	defer n.mu.RUnlock()

	out := make([]string, 0, len(n.handlers))
	for a := range n.handlers {
		out = append(out, a)
	}

	slices.Sort(out)

	return out
}

// Transport returns the ControlTransport used by the member at from.
func (n *Network) Transport(from string) cachemesh.ControlTransport {
	return transport{n: n, from: from}
}

type transport struct {
	n    *Network
	from string
}

func (t transport) Send(ctx context.Context, addr cachemesh.Endpoint, msg []byte) ([]byte, error) {
	return t.n.Send(ctx, t.from, addr, msg)
}

func (n *Network) random() (float64, int64) {
	n.rngMu.Lock()
	defer n.rngMu.Unlock()

	return n.rng.Float64(), n.rng.Int64()
}

// Send delivers msg from one address to another and returns the response.
func (n *Network) Send(ctx context.Context, from, to string, msg []byte) ([]byte, error) {
	n.mu.RLock()
	_, up := n.handlers[from]
	h, ok := n.handlers[to]
	gf, gt := n.groups[from], n.groups[to]
	lat, jit, drop := n.latency, n.jitter, n.drop
	n.mu.RUnlock()

	if !up {
		return nil, ErrSenderDown
	}

	if !ok || (gf != 0 && gt != 0 && gf != gt) {
		return nil, ErrUnreachable
	}

	p, r := n.random()
	if drop > 0 && p < drop {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	if jit > 0 {
		lat += time.Duration(r % int64(jit))
	}

	if err := sleep(ctx, lat); err != nil {
		return nil, err
	}

	resp, err := h(ctx, from, slices.Clone(msg))
	if err != nil {
		return nil, err
	}

	if err := sleep(ctx, lat); err != nil {
		return nil, err
	}

	return slices.Clone(resp), nil
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}

	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// LeaseStore is an in-memory leader Lease implementing
// cachemesh.LeaseStore: Create fails with cachemesh.ErrLeaseConflict when
// the Lease exists, Update compares ResourceVersion and fails with
// cachemesh.ErrLeaseConflict on mismatch or cachemesh.ErrLeaseNotFound when
// missing. It is safe for concurrent use.
type LeaseStore struct {
	mu    sync.Mutex
	rec   *cachemesh.LeaseRecord
	rv    uint64
	uids  uint64
	err   error
	calls int
}

// NewLeaseStore returns a store with no Lease.
func NewLeaseStore() *LeaseStore { return &LeaseStore{} }

// SetError makes every operation fail with err (an API server outage)
// until it is called with nil.
func (s *LeaseStore) SetError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.err = err
}

// Calls returns the number of operations served.
func (s *LeaseStore) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.calls
}

// Record returns the stored record, if any.
func (s *LeaseStore) Record() (cachemesh.LeaseRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.rec == nil {
		return cachemesh.LeaseRecord{}, false
	}

	return s.rec.Clone(), true
}

func (s *LeaseStore) begin(ctx context.Context) error {
	s.calls++

	if err := ctx.Err(); err != nil {
		return err
	}

	return s.err
}

// Get implements cachemesh.LeaseStore.
func (s *LeaseStore) Get(ctx context.Context) (cachemesh.LeaseRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.begin(ctx); err != nil {
		return cachemesh.LeaseRecord{}, err
	}

	if s.rec == nil {
		return cachemesh.LeaseRecord{}, cachemesh.ErrLeaseNotFound
	}

	return s.rec.Clone(), nil
}

// GetCached implements cachemesh.LeaseStore. It is never stale.
func (s *LeaseStore) GetCached(ctx context.Context) (cachemesh.LeaseRecord, error) {
	return s.Get(ctx)
}

// Create implements cachemesh.LeaseStore.
func (s *LeaseStore) Create(ctx context.Context, r cachemesh.LeaseRecord) (cachemesh.LeaseRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.begin(ctx); err != nil {
		return cachemesh.LeaseRecord{}, err
	}

	if s.rec != nil {
		return cachemesh.LeaseRecord{}, cachemesh.ErrLeaseConflict
	}

	s.uids++
	r = r.Clone()
	r.UID = "uid-" + strconv.FormatUint(s.uids, 10)
	s.store(&r)

	return r.Clone(), nil
}

// Update implements cachemesh.LeaseStore.
func (s *LeaseStore) Update(ctx context.Context, r cachemesh.LeaseRecord) (cachemesh.LeaseRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.begin(ctx); err != nil {
		return cachemesh.LeaseRecord{}, err
	}

	if s.rec == nil {
		return cachemesh.LeaseRecord{}, cachemesh.ErrLeaseNotFound
	}

	if r.ResourceVersion != s.rec.ResourceVersion {
		return cachemesh.LeaseRecord{}, cachemesh.ErrLeaseConflict
	}

	r = r.Clone()
	r.UID = s.rec.UID
	s.store(&r)

	return r.Clone(), nil
}

func (s *LeaseStore) store(r *cachemesh.LeaseRecord) {
	s.rv++
	r.ResourceVersion = strconv.FormatUint(s.rv, 10)
	s.rec = r
}
