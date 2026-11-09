// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cachemesh_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh"
	"github.com/Azure/unbounded/internal/cachemesh/memtransport"
)

// fastTuning shrinks every timer so a cluster forms, fails over, and drains
// within seconds.
func fastTuning() cachemesh.Tuning {
	return cachemesh.Tuning{
		PingInterval:   100 * time.Millisecond,
		PingTimeout:    100 * time.Millisecond,
		SuspectRefresh: 200 * time.Millisecond,
		JoinRetry:      100 * time.Millisecond,
		CallTimeout:    250 * time.Millisecond,
		NeighborLinger: 300 * time.Millisecond,
		LeaderTick:     50 * time.Millisecond,
		TickInterval:   50 * time.Millisecond,
		EpochAdvance:   20 * time.Millisecond,
		Election: cachemesh.ElectionConfig{
			LeaseDuration:      1500 * time.Millisecond,
			RenewDeadline:      1000 * time.Millisecond,
			RetryPeriod:        200 * time.Millisecond,
			NoLeaderTimeout:    2 * time.Second,
			LotteryRound:       100 * time.Millisecond,
			ClockSkewAllowance: 100 * time.Millisecond,
			MinCandidateAge:    300 * time.Millisecond,
		},
		QuarantineFor:   100 * time.Millisecond,
		WarmupStep:      100 * time.Millisecond,
		DrainFor:        300 * time.Millisecond,
		SuspicionWindow: 3 * time.Second,
	}
}

const epochPeriod = 200 * time.Millisecond

// neighborLog checks that a member's watcher sees consistent sets.
type neighborLog struct {
	mu   sync.Mutex
	set  map[string]cachemesh.PeerRef
	errs []string
}

func (l *neighborLog) OnNeighbors(added, removed []cachemesh.PeerRef) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, p := range removed {
		if l.set[p.ID] != p {
			l.errs = append(l.errs, "removed unknown "+p.ID)
		}

		delete(l.set, p.ID)
	}

	for _, p := range added {
		if _, ok := l.set[p.ID]; ok {
			l.errs = append(l.errs, "added twice "+p.ID)
		}

		l.set[p.ID] = p
	}
}

func (l *neighborLog) ids() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return slices.Sorted(maps.Keys(l.set))
}

type member struct {
	m      *cachemesh.Mesh
	cancel context.CancelFunc
	errc   chan error
	nb     *neighborLog
	alive  bool
}

type cluster struct {
	t     *testing.T
	net   *memtransport.Network
	lease *memtransport.LeaseStore
	ms    []*member
	byID  map[string]*member
}

func newCluster(t *testing.T, n int) *cluster {
	t.Helper()

	c := &cluster{
		t: t, net: memtransport.NewNetwork(memtransport.WithLatency(100*time.Microsecond, 200*time.Microsecond)),
		lease: memtransport.NewLeaseStore(), byID: map[string]*member{},
	}
	t.Cleanup(c.stop)

	logger := slog.New(slog.DiscardHandler)

	for i := range n {
		addr := "m" + strconv.Itoa(i)

		var seeds []string
		if i > 0 {
			seeds = []string{"m0", "m" + strconv.Itoa(i/2)}
		}

		nb := &neighborLog{set: map[string]cachemesh.PeerRef{}}

		m, err := cachemesh.New(cachemesh.Config{
			ID: addr, Addr: addr, Transport: c.net.Transport(addr), Seeds: seeds,
			Lease: c.lease, Neighbors: nb, Logger: logger,
			PartitionBits: 12, EpochPeriod: epochPeriod, ExpectedSize: 1, Tuning: fastTuning(),
		})
		if err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		mem := &member{m: m, cancel: cancel, errc: make(chan error, 1), nb: nb, alive: true}
		c.ms = append(c.ms, mem)
		c.byID[addr] = mem

		c.net.Register(addr, m.ServeControl)

		go func() { mem.errc <- m.Run(ctx) }()

		if i == 0 {
			// Let the first member bootstrap before the rest join.
			c.waitFor(t, "bootstrap", 10*time.Second, func() error {
				if !m.Leading() || m.Members() != 1 {
					return errors.New("not yet")
				}

				return nil
			})
		}
	}

	return c
}

func (c *cluster) stop() {
	for _, m := range c.ms {
		if m.alive {
			m.cancel()
			<-m.errc
			m.alive = false
		}
	}
}

func (c *cluster) kill(m *member) {
	c.net.Unregister(m.m.Addr())
	m.cancel()

	if err := <-m.errc; !errors.Is(err, context.Canceled) {
		c.t.Errorf("Run returned %v", err)
	}

	m.alive = false
}

func (c *cluster) live() []*member {
	var out []*member

	for _, m := range c.ms {
		if m.alive {
			out = append(out, m)
		}
	}

	return out
}

func (c *cluster) waitFor(t testing.TB, what string, d time.Duration, f func() error) {
	t.Helper()

	deadline := time.Now().Add(d)

	for {
		err := f()
		if err == nil {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("%s: %v", what, err)
		}

		time.Sleep(20 * time.Millisecond)
	}
}

// converged checks one leader, one version, and all live members Active.
func (c *cluster) converged() error {
	live := c.live()
	leaders := 0
	v := live[0].m.Version()

	for _, m := range live {
		if m.m.Leading() {
			leaders++
		}

		if m.m.Version() != v {
			return fmt.Errorf("%s at %v, %s at %v", live[0].m.ID(), v, m.m.ID(), m.m.Version())
		}

		if !m.m.Ready() {
			return fmt.Errorf("%s not ready", m.m.ID())
		}

		if m.m.Members() != len(live) {
			return fmt.Errorf("%s sees %d members, want %d", m.m.ID(), m.m.Members(), len(live))
		}
	}

	if leaders != 1 {
		return fmt.Errorf("%d leaders", leaders)
	}

	return nil
}

func (c *cluster) leader() *member {
	for _, m := range c.live() {
		if m.m.Leading() {
			return m
		}
	}

	return nil
}

// checkRoutes routes keys from every member hop by hop and checks each
// request ends at the owner.
func (c *cluster) checkRoutes(ks cachemesh.Keyspace, keys int) error {
	for _, src := range c.live() {
		for k := range keys {
			key := []byte("key-" + strconv.Itoa(k))
			d := src.m.Route(ks, key)
			owner := d.Owner
			cur := src

			for hops := 0; d.Kind != cachemesh.KindSelf; hops++ {
				if d.Kind == cachemesh.KindNoRoute || d.Kind == cachemesh.KindDirect || d.Kind == cachemesh.KindGateway {
					return fmt.Errorf("%s: key %d: kind %v", cur.m.ID(), k, d.Kind)
				}

				next := c.byID[d.Next.ID]
				if next == nil || !next.alive {
					return fmt.Errorf("%s: key %d: next %s not live", cur.m.ID(), k, d.Next.ID)
				}

				cur.m.Observe(d.Next, cachemesh.Success, time.Millisecond)

				var err error

				if d, err = next.m.Forward(d.Header); err != nil {
					return fmt.Errorf("%s: key %d: forward: %w", next.m.ID(), k, err)
				}

				cur = next

				if hops > 8 {
					return fmt.Errorf("key %d: too many hops", k)
				}
			}

			// A backup in the owner's cell may serve when it sees the
			// owner as unusable (for example after timeouts under -race).
			if d.Owner.ID != owner.ID || (cur.m.ID() != owner.ID && d.Header.Flags&cachemesh.FlagBackup == 0) {
				return fmt.Errorf("key %d from %s ended at %s, owner %s", k, src.m.ID(), cur.m.ID(), owner.ID)
			}
		}
	}

	return nil
}

func (c *cluster) checkNeighbors() error {
	for _, m := range c.live() {
		m.nb.mu.Lock()
		errs := slices.Clone(m.nb.errs)
		m.nb.mu.Unlock()

		if len(errs) > 0 {
			return fmt.Errorf("%s watcher: %v", m.m.ID(), errs)
		}

		var want []string
		for _, p := range m.m.Neighbors() {
			want = append(want, p.ID)
		}

		slices.Sort(want)

		if got := m.nb.ids(); !slices.Equal(got, want) {
			return fmt.Errorf("%s watcher has %v, want %v", m.m.ID(), got, want)
		}
	}

	return nil
}

func TestCluster(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	const n = 96

	c := newCluster(t, n)
	blobs := cachemesh.MustKeyspace("blobs", 0)
	meta := cachemesh.MustKeyspace("meta", cachemesh.MinTTL)

	c.waitFor(t, "initial convergence", 30*time.Second, c.converged)
	c.waitFor(t, "routes", 10*time.Second, func() error { return c.checkRoutes(blobs, 64) })
	c.waitFor(t, "neighbors", 10*time.Second, c.checkNeighbors)

	t.Run("Invalidate", func(t *testing.T) {
		stamp := c.ms[3].m.Stamp(meta)
		for _, m := range c.live() {
			if !m.m.Fresh(meta, stamp) {
				t.Fatalf("%s: stamp not fresh", m.m.ID())
			}
		}

		blob := c.ms[3].m.Stamp(blobs)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// Invalidate returns once the bump is in this member's view.
		if err := c.ms[7].m.Invalidate(ctx, meta); err != nil {
			t.Fatalf("Invalidate: %v", err)
		}

		if c.ms[7].m.Fresh(meta, stamp) {
			t.Fatal("stamp fresh at the invalidating member after Invalidate")
		}

		c.waitFor(t, "invalidate", 10*time.Second, func() error {
			for _, m := range c.live() {
				if m.m.Fresh(meta, stamp) {
					return fmt.Errorf("%s: stamp still fresh", m.m.ID())
				}
			}

			return nil
		})

		for _, m := range c.live() {
			if !m.m.Fresh(blobs, blob) {
				t.Fatalf("%s: other keyspace invalidated", m.m.ID())
			}

			if !m.m.Fresh(meta, m.m.Stamp(meta)) {
				t.Fatalf("%s: new stamp not fresh", m.m.ID())
			}
		}
	})

	t.Run("TTL", func(t *testing.T) {
		const ttl = 400 * time.Millisecond

		short := cachemesh.UnsafeKeyspace("short", ttl)
		m := c.ms[5].m
		start := time.Now()
		st := m.Stamp(short)

		if !m.Fresh(short, st) {
			t.Fatal("not fresh when issued")
		}

		if !c.ms[9].m.Fresh(short, st) {
			t.Fatal("copy not fresh on another member")
		}

		time.Sleep(ttl + 2*epochPeriod + 50*time.Millisecond - time.Since(start))

		for _, mm := range c.live() {
			if mm.m.Fresh(short, st) {
				t.Fatalf("%s: fresh after TTL + 2 periods", mm.m.ID())
			}
		}
	})

	t.Run("LeaderFailover", func(t *testing.T) {
		old := c.leader()
		if old == nil {
			t.Fatal("no leader")
		}

		oldTerm := old.m.Version().Term
		c.kill(old)

		c.waitFor(t, "failover", 40*time.Second, func() error {
			if err := c.converged(); err != nil {
				return err
			}

			if l := c.leader(); l.m.Version().Term <= oldTerm {
				return errors.New("term did not advance")
			}

			return nil
		})
		c.waitFor(t, "routes after failover", 10*time.Second, func() error { return c.checkRoutes(blobs, 32) })
	})

	t.Run("Leave", func(t *testing.T) {
		var leaving *member

		for _, m := range c.live() {
			if !m.m.Leading() {
				leaving = m
				break
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		if err := leaving.m.Leave(ctx); err != nil {
			t.Fatalf("Leave: %v", err)
		}

		if err := <-leaving.errc; err != nil {
			t.Fatalf("Run after Leave returned %v", err)
		}

		leaving.alive = false
		c.net.Unregister(leaving.m.Addr())

		c.waitFor(t, "leave convergence", 20*time.Second, c.converged)

		for _, m := range c.live() {
			for _, p := range m.m.Neighbors() {
				if p.ID == leaving.m.ID() {
					t.Fatalf("%s still neighbors the leaver", m.m.ID())
				}
			}
		}

		c.waitFor(t, "routes after leave", 10*time.Second, func() error { return c.checkRoutes(blobs, 32) })
		c.waitFor(t, "neighbors after leave", 10*time.Second, c.checkNeighbors)
	})
}
