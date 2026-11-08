// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package memtransport

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh"
)

func echo(_ context.Context, from string, msg []byte) ([]byte, error) {
	return append([]byte(from+":"), msg...), nil
}

func TestNetwork(t *testing.T) {
	n := NewNetwork()
	n.Register("a", echo)
	n.Register("b", echo)

	ctx := context.Background()

	resp, err := n.Transport("a").Send(ctx, "b", []byte("hi"))
	if err != nil || string(resp) != "a:hi" {
		t.Fatalf("send: %q %v", resp, err)
	}

	if _, err := n.Send(ctx, "a", "c", nil); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("unknown target: %v", err)
	}

	if _, err := n.Send(ctx, "c", "a", nil); !errors.Is(err, ErrSenderDown) {
		t.Fatalf("unknown sender: %v", err)
	}

	n.Register("c", echo)
	n.Partition([]string{"a"}, []string{"b"})

	if _, err := n.Send(ctx, "a", "b", nil); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("partitioned: %v", err)
	}

	if _, err := n.Send(ctx, "a", "c", nil); err != nil {
		t.Fatalf("unlisted address must be reachable: %v", err)
	}

	n.Heal()

	if _, err := n.Send(ctx, "a", "b", nil); err != nil {
		t.Fatalf("healed: %v", err)
	}

	n.Unregister("b")

	if _, err := n.Send(ctx, "a", "b", nil); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("crashed: %v", err)
	}

	if got := n.Addrs(); len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Fatalf("addrs %v", got)
	}
}

func TestNetworkDropAndLatency(t *testing.T) {
	n := NewNetwork(WithDropRate(1), WithSeed(3))
	n.Register("a", echo)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if _, err := n.Send(ctx, "a", "a", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("dropped: %v", err)
	}

	n.SetDropRate(0)
	n.SetLatency(5*time.Millisecond, time.Millisecond)

	start := time.Now()

	if _, err := n.Send(context.Background(), "a", "a", nil); err != nil {
		t.Fatal(err)
	}

	if d := time.Since(start); d < 10*time.Millisecond {
		t.Fatalf("latency not applied: %v", d)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel2()

	if _, err := n.Send(ctx2, "a", "a", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout during latency: %v", err)
	}
}

func TestNetworkConcurrent(t *testing.T) {
	n := NewNetwork(WithLatency(0, time.Microsecond))
	n.Register("a", echo)

	var wg sync.WaitGroup

	for range 8 {
		wg.Go(func() {
			for range 100 {
				_, _ = n.Send(context.Background(), "a", "a", []byte("x"))
				n.Partition([]string{"a"})
			}
		})
	}

	wg.Wait()
}

func TestLeaseStore(t *testing.T) {
	s := NewLeaseStore()
	ctx := context.Background()

	if _, err := s.Get(ctx); !errors.Is(err, cachemesh.ErrLeaseNotFound) {
		t.Fatalf("get missing: %v", err)
	}

	if _, err := s.Update(ctx, cachemesh.LeaseRecord{}); !errors.Is(err, cachemesh.ErrLeaseNotFound) {
		t.Fatalf("update missing: %v", err)
	}

	r, err := s.Create(ctx, cachemesh.LeaseRecord{Holder: "a", Seeds: []string{"x"}})
	if err != nil || r.ResourceVersion == "" || r.UID == "" {
		t.Fatalf("create: %+v %v", r, err)
	}

	if _, err := s.Create(ctx, cachemesh.LeaseRecord{Holder: "b"}); !errors.Is(err, cachemesh.ErrLeaseConflict) {
		t.Fatalf("create existing: %v", err)
	}

	r.Seeds[0] = "mutated"

	got, _ := s.GetCached(ctx)
	if got.Seeds[0] != "x" {
		t.Fatal("store aliases caller memory")
	}

	r.Holder = "b"

	r2, err := s.Update(ctx, r)
	if err != nil || r2.ResourceVersion == r.ResourceVersion || r2.UID != r.UID || r2.Holder != "b" {
		t.Fatalf("update: %+v %v", r2, err)
	}

	if _, err := s.Update(ctx, r); !errors.Is(err, cachemesh.ErrLeaseConflict) {
		t.Fatalf("stale update: %v", err)
	}

	outage := errors.New("outage")
	s.SetError(outage)

	if _, err := s.Get(ctx); !errors.Is(err, outage) {
		t.Fatalf("outage: %v", err)
	}

	s.SetError(nil)

	if rec, ok := s.Record(); !ok || rec.Holder != "b" || s.Calls() != 8 {
		t.Fatalf("record %+v %v calls %d", rec, ok, s.Calls())
	}
}

var _ cachemesh.LeaseStore = (*LeaseStore)(nil)
