// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cachemesh_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh"
	"github.com/Azure/unbounded/internal/cachemesh/memtransport"
)

// gate is a ControlTransport that fails every call while closed.
type gate struct {
	inner cachemesh.ControlTransport
	open  atomic.Bool
}

var errGateClosed = errors.New("gate closed")

func (g *gate) Send(ctx context.Context, addr cachemesh.Endpoint, msg []byte) ([]byte, error) {
	if !g.open.Load() {
		return nil, errGateClosed
	}

	return g.inner.Send(ctx, addr, msg)
}

// blockingTransport never answers; Send returns only when ctx ends.
type blockingTransport struct{ calls atomic.Int64 }

func (b *blockingTransport) Send(ctx context.Context, _ cachemesh.Endpoint, _ []byte) ([]byte, error) {
	b.calls.Add(1)
	<-ctx.Done()

	return nil, ctx.Err()
}

func eventually(t *testing.T, what string, d time.Duration, f func() bool) {
	t.Helper()

	deadline := time.Now().Add(d)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}

		time.Sleep(5 * time.Millisecond)
	}
}

type runner struct {
	m      *cachemesh.Mesh
	cancel context.CancelFunc
	errc   chan error
}

func start(t *testing.T, cfg cachemesh.Config) *runner {
	t.Helper()

	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}

	m, err := cachemesh.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	r := &runner{m: m, cancel: cancel, errc: make(chan error, 1)}

	go func() { r.errc <- m.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		<-m.Done()
	})

	return r
}

// leaderOf starts a lone member that bootstraps a cluster on lease.
func leaderOf(t *testing.T, net *memtransport.Network, lease cachemesh.LeaseStore, cfg cachemesh.Config) *runner {
	t.Helper()

	if cfg.ID == "" {
		cfg.ID = "leader"
	}

	cfg.Addr = cfg.ID
	cfg.Transport = net.Transport(cfg.Addr)
	cfg.Lease = lease

	if cfg.PartitionBits == 0 {
		cfg.PartitionBits = 12
	}

	if cfg.Tuning.PingInterval == 0 {
		cfg.Tuning = fastTuning()
	}

	if cfg.ExpectedSize == 0 {
		cfg.ExpectedSize = 1
	}

	r := start(t, cfg)
	net.Register(cfg.Addr, r.m.ServeControl)
	eventually(t, "bootstrap", 10*time.Second, func() bool { return r.m.Leading() && r.m.Ready() })

	return r
}

func TestExpectedSizeBootstrapsQuickly(t *testing.T) {
	net := memtransport.NewNetwork()
	lease := memtransport.NewLeaseStore()

	// LotteryRound is 100ms: with ExpectedSize 2 every round is eligible,
	// so the lone member bootstraps within a few rounds.
	began := time.Now()
	r := leaderOf(t, net, lease, cachemesh.Config{ExpectedSize: 2, Tuning: fastTuning()})

	if d := time.Since(began); d > 3*time.Second {
		t.Fatalf("bootstrap took %v", d)
	}

	if r.m.Members() != 1 {
		t.Fatalf("members = %d", r.m.Members())
	}
}

func TestExpectedSizeValidation(t *testing.T) {
	base := cachemesh.Config{ID: "a", Addr: "a", Transport: &blockingTransport{}}

	cfg := base
	cfg.ExpectedSize = -1

	if _, err := cachemesh.New(cfg); err == nil {
		t.Fatal("negative ExpectedSize accepted")
	}
}

func TestLeaseSeedsAreGateways(t *testing.T) {
	net := memtransport.NewNetwork()
	lease := memtransport.NewLeaseStore()
	leaderOf(t, net, lease, cachemesh.Config{})

	// A member with only a Lease and a transport that cannot reach anyone
	// learns the seeds from the Lease but cannot join: it must route
	// through them as gateways.
	g := &gate{inner: net.Transport("joiner")}
	j := start(t, cachemesh.Config{
		ID: "joiner", Addr: "joiner", Transport: g, Lease: lease,
		PartitionBits: 12, ExpectedSize: 1, Tuning: fastTuning(),
	})
	net.Register("joiner", j.m.ServeControl)

	ks := cachemesh.MustKeyspace("blobs", 0)

	eventually(t, "gateway route", 5*time.Second, func() bool {
		d := j.m.Route(ks, []byte("k"))
		return d.Kind == cachemesh.KindGateway && d.Next.Addr == "leader"
	})

	// Once it can reach the cluster it joins and routes as a member.
	g.open.Store(true)
	eventually(t, "join", 10*time.Second, func() bool { return j.m.Ready() && j.m.Members() == 2 })

	if d := j.m.Route(ks, []byte("k")); d.Kind == cachemesh.KindGateway || d.Kind == cachemesh.KindNoRoute {
		t.Fatalf("member still routes as non-member: %+v", d)
	}
}

func TestInvalidate(t *testing.T) {
	net := memtransport.NewNetwork()
	lease := memtransport.NewLeaseStore()
	meta := cachemesh.MustKeyspace("meta", cachemesh.MinTTL)

	l := leaderOf(t, net, lease, cachemesh.Config{})

	t.Run("Leader", func(t *testing.T) {
		st := l.m.Stamp(meta)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := l.m.Invalidate(ctx, meta); err != nil {
			t.Fatal(err)
		}

		if l.m.Fresh(meta, st) {
			t.Fatal("stamp still fresh")
		}

		if !l.m.Fresh(meta, l.m.Stamp(meta)) {
			t.Fatal("new stamp not fresh")
		}
	})

	t.Run("RetriedUntilReachable", func(t *testing.T) {
		g := &gate{inner: net.Transport("f")}
		f := start(t, cachemesh.Config{
			ID: "f", Addr: "f", Transport: g, Seeds: []string{"leader"},
			PartitionBits: 12, ExpectedSize: 1, Tuning: fastTuning(),
		})
		net.Register("f", f.m.ServeControl)
		g.open.Store(true)
		eventually(t, "join", 10*time.Second, func() bool { return f.m.Ready() })

		st := l.m.Stamp(meta)

		// Cut the follower off: the request must wait, not be dropped.
		g.open.Store(false)

		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()

		if err := f.m.Invalidate(ctx, meta); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Invalidate while cut off = %v", err)
		}

		if !l.m.Fresh(meta, st) {
			t.Fatal("invalidated while cut off")
		}

		g.open.Store(true)

		// The queued request is delivered without another call.
		eventually(t, "queued invalidate", 10*time.Second, func() bool { return !l.m.Fresh(meta, st) })

		// A second waiter for a new request completes too.
		ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel2()

		var wg sync.WaitGroup

		errs := make([]error, 4)
		for i := range errs {
			wg.Go(func() { errs[i] = f.m.Invalidate(ctx2, meta) })
		}

		wg.Wait()

		for _, err := range errs {
			if err != nil {
				t.Fatalf("concurrent Invalidate: %v", err)
			}
		}
	})

	t.Run("NotRunning", func(t *testing.T) {
		m, err := cachemesh.New(cachemesh.Config{ID: "x", Addr: "x", Transport: &blockingTransport{}})
		if err != nil {
			t.Fatal(err)
		}

		if err := m.Invalidate(context.Background(), meta); !errors.Is(err, cachemesh.ErrNotRunning) {
			t.Fatalf("before Run: %v", err)
		}
	})

	t.Run("Bounded", func(t *testing.T) {
		r := start(t, cachemesh.Config{
			ID: "lonely", Addr: "lonely", Transport: &blockingTransport{}, Seeds: []string{"nowhere"},
			ExpectedSize: 1, Tuning: fastTuning(),
		})
		eventually(t, "running", 5*time.Second, func() bool {
			_, err := r.m.ServeControl(context.Background(), "x", nil)
			return err == nil
		})

		done, cancel := context.WithCancel(context.Background())
		cancel()

		for i := range cachemesh.MaxPendingInvalidations {
			ks := cachemesh.MustKeyspace("ks"+strconv.Itoa(i), 0)
			if err := r.m.Invalidate(done, ks); !errors.Is(err, context.Canceled) {
				t.Fatalf("Invalidate %d: %v", i, err)
			}
		}

		// The same keyspace coalesces; a new one is refused.
		if err := r.m.Invalidate(done, cachemesh.MustKeyspace("ks0", 0)); !errors.Is(err, context.Canceled) {
			t.Fatalf("coalesced: %v", err)
		}

		if err := r.m.Invalidate(done, cachemesh.MustKeyspace("one-more", 0)); !errors.Is(err, cachemesh.ErrTooManyInvalidations) {
			t.Fatalf("over limit: %v", err)
		}

		// Waiters return ErrNotRunning when Run stops.
		errc := make(chan error, 1)

		go func() { errc <- r.m.Invalidate(context.Background(), cachemesh.MustKeyspace("ks1", 0)) }()

		time.Sleep(20 * time.Millisecond)
		r.cancel()

		if err := <-errc; !errors.Is(err, cachemesh.ErrNotRunning) {
			t.Fatalf("waiter after Run returned: %v", err)
		}
	})
}

// TestHooksReenter checks that every hook and the watcher can call Mesh
// methods, including ones that take the Mesh lock, without deadlocking.
func TestHooksReenter(t *testing.T) {
	net := memtransport.NewNetwork()
	lease := memtransport.NewLeaseStore()

	var (
		m                    atomic.Pointer[cachemesh.Mesh]
		views, leads, nbs    atomic.Int64
		lastVersion          atomic.Pointer[cachemesh.Version]
		outOfOrder           atomic.Bool
		controlErrs, leaseEs atomic.Int64
	)

	touch := func() {
		mm := m.Load()
		_ = mm.Neighbors()
		_, _ = mm.Leader()
		_ = mm.Members()
		_ = mm.Ready()
		_, _ = mm.ServeControl(context.Background(), "hook", []byte{0})
	}

	cfg := cachemesh.Config{
		ID: "leader", Addr: "leader", Transport: net.Transport("leader"), Lease: lease,
		Logger: slog.New(slog.DiscardHandler), PartitionBits: 12, ExpectedSize: 1, Tuning: fastTuning(),
		Neighbors: cachemesh.NeighborFunc(func(_, _ []cachemesh.PeerRef) {
			nbs.Add(1)
			touch()
		}),
		Hooks: cachemesh.Hooks{
			OnView: func(v cachemesh.Version, _ int) {
				if p := lastVersion.Load(); p != nil && v.Less(*p) {
					outOfOrder.Store(true)
				}

				lastVersion.Store(&v)
				views.Add(1)
				touch()
			},
			OnLeadership: func(bool, uint64) {
				leads.Add(1)
				touch()
			},
			OnControlError: func(cachemesh.Endpoint, error) {
				controlErrs.Add(1)
				touch()
			},
			OnLeaseError: func(string, error) {
				leaseEs.Add(1)
				touch()
			},
		},
	}

	mesh, err := cachemesh.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	m.Store(mesh)
	net.Register("leader", mesh.ServeControl)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = mesh.Run(ctx) }()

	eventually(t, "bootstrap", 10*time.Second, func() bool { return mesh.Leading() && mesh.Ready() })

	// A second member produces neighbor events and control errors once it
	// is killed.
	f := start(t, cachemesh.Config{
		ID: "f", Addr: "f", Transport: net.Transport("f"), Seeds: []string{"leader"},
		PartitionBits: 12, ExpectedSize: 1, Tuning: fastTuning(),
	})
	net.Register("f", f.m.ServeControl)
	eventually(t, "neighbor events", 10*time.Second, func() bool { return nbs.Load() > 0 && f.m.Ready() })

	f.cancel()
	net.Unregister("f")
	eventually(t, "control errors", 10*time.Second, func() bool { return controlErrs.Load() > 0 })

	lease.SetError(errors.New("apiserver down"))
	eventually(t, "lease errors", 10*time.Second, func() bool { return leaseEs.Load() > 0 })
	lease.SetError(nil)

	if views.Load() == 0 || leads.Load() == 0 {
		t.Fatalf("views=%d leads=%d", views.Load(), leads.Load())
	}

	if outOfOrder.Load() {
		t.Fatal("OnView versions out of order")
	}

	// The Mesh still serves after all hooks reentered it.
	eventually(t, "mesh responsive", 5*time.Second, func() bool { return mesh.Members() >= 1 })
}

func TestServeControlHostile(t *testing.T) {
	r := start(t, cachemesh.Config{ID: "a", Addr: "a", Transport: &blockingTransport{}, Seeds: []string{"s"}, ExpectedSize: 1, Tuning: fastTuning()})
	eventually(t, "running", 5*time.Second, func() bool {
		_, err := r.m.ServeControl(context.Background(), "x", nil)
		return err == nil
	})

	if _, err := r.m.ServeControl(context.Background(), "x", make([]byte, cachemesh.MaxControlMessageSize+1)); !errors.Is(err, cachemesh.ErrMessageTooLarge) {
		t.Fatalf("oversized: %v", err)
	}

	for i, msg := range [][]byte{nil, {0}, {0xff, 0xff, 0xff}, make([]byte, 4096)} {
		resp, err := r.m.ServeControl(context.Background(), "x", msg)
		if err != nil || len(resp) == 0 {
			t.Fatalf("message %d: resp %d bytes, err %v", i, len(resp), err)
		}
	}
}

// TestSlowTransport checks that a transport that never answers bounds the
// calls in flight, reports refusals, and does not keep Run from returning.
func TestSlowTransport(t *testing.T) {
	bt := &blockingTransport{}

	var refused atomic.Int64

	tu := fastTuning()
	tu.MaxInFlight = 2

	seeds := make([]string, 16)
	for i := range seeds {
		seeds[i] = fmt.Sprintf("s%d", i)
	}

	m, err := cachemesh.New(cachemesh.Config{
		ID: "a", Addr: "a", Transport: bt, Seeds: seeds, Tuning: tu,
		Logger: slog.New(slog.DiscardHandler),
		Hooks: cachemesh.Hooks{OnControlError: func(_ cachemesh.Endpoint, err error) {
			if errors.Is(err, cachemesh.ErrTooManyCalls) {
				refused.Add(1)
			}
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)

	go func() { errc <- m.Run(ctx) }()

	eventually(t, "refused calls", 10*time.Second, func() bool { return refused.Load() > 0 })

	cancel()

	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestLeaveContext(t *testing.T) {
	r := start(t, cachemesh.Config{ID: "a", Addr: "a", Transport: &blockingTransport{}, Seeds: []string{"s"}, ExpectedSize: 1, Tuning: fastTuning()})
	eventually(t, "running", 5*time.Second, func() bool {
		_, err := r.m.ServeControl(context.Background(), "x", nil)
		return err == nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if err := r.m.Leave(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Leave = %v", err)
	}

	// A canceled Run ends a pending Leave with ErrNotRunning.
	errc := make(chan error, 1)

	go func() { errc <- r.m.Leave(context.Background()) }()

	time.Sleep(20 * time.Millisecond)
	r.cancel()

	if err := <-errc; !errors.Is(err, cachemesh.ErrNotRunning) {
		t.Fatalf("Leave after cancel = %v", err)
	}
}

// TestNoGoroutineLeak runs a small cluster, stops it, and checks that the
// goroutine count returns to where it started.
func TestNoGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()

	func() {
		net := memtransport.NewNetwork()
		lease := memtransport.NewLeaseStore()

		var ms []*cachemesh.Mesh

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		for i := range 4 {
			addr := "n" + strconv.Itoa(i)

			var seeds []string
			if i > 0 {
				seeds = []string{"n0"}
			}

			m, err := cachemesh.New(cachemesh.Config{
				ID: addr, Addr: addr, Transport: net.Transport(addr), Seeds: seeds, Lease: lease,
				Logger: slog.New(slog.DiscardHandler), PartitionBits: 12, ExpectedSize: 1, Tuning: fastTuning(),
				Neighbors: cachemesh.NeighborFunc(func(_, _ []cachemesh.PeerRef) {}),
				Hooks:     cachemesh.Hooks{OnView: func(cachemesh.Version, int) {}},
			})
			if err != nil {
				t.Fatal(err)
			}

			net.Register(addr, m.ServeControl)
			ms = append(ms, m)

			go func() { _ = m.Run(ctx) }()

			if i == 0 {
				eventually(t, "bootstrap", 10*time.Second, func() bool { return m.Leading() })
			}
		}

		eventually(t, "cluster", 10*time.Second, func() bool {
			for _, m := range ms {
				if !m.Ready() || m.Members() != len(ms) {
					return false
				}
			}

			return true
		})

		cancel()

		for _, m := range ms {
			<-m.Done()
		}
	}()

	deadline := time.Now().Add(5 * time.Second)

	for {
		n := runtime.NumGoroutine()
		if n <= before {
			return
		}

		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("goroutines: %d before, %d after\n%s", before, n, buf[:runtime.Stack(buf, true)])
		}

		time.Sleep(10 * time.Millisecond)
	}
}
