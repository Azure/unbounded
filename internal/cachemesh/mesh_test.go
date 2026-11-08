// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cachemesh

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/route"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
)

type nopTransport struct{}

func (nopTransport) Send(ctx context.Context, _ string, _ []byte) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestNewKeyspace(t *testing.T) {
	for _, tc := range []struct {
		name string
		ttl  time.Duration
		ok   bool
	}{
		{"blobs", 0, true},
		{"meta", MinTTL, true},
		{"meta", time.Hour, true},
		{"meta", MinTTL - 1, false},
		{"meta", time.Second, false},
		{"meta", -time.Second, false},
		{"", 0, false},
	} {
		ks, err := NewKeyspace(tc.name, tc.ttl)
		if (err == nil) != tc.ok {
			t.Fatalf("NewKeyspace(%q, %v) err = %v, want ok=%v", tc.name, tc.ttl, err, tc.ok)
		}

		if tc.ok && (ks.Name() != tc.name || ks.TTL() != tc.ttl || ks.Seed() == 0) {
			t.Fatalf("bad keyspace %+v", ks)
		}
	}

	a, b := MustKeyspace("a", 0), MustKeyspace("b", 0)
	if a.Seed() == b.Seed() || a.Seed() != MustKeyspace("a", time.Hour).Seed() {
		t.Fatal("seed must depend only on name")
	}

	defer func() {
		if recover() == nil {
			t.Fatal("MustKeyspace did not panic")
		}
	}()

	MustKeyspace("x", time.Second)
}

func TestConfigValidation(t *testing.T) {
	good := Config{ID: "a", Addr: "a:1", Transport: nopTransport{}}
	if _, err := New(good); err != nil {
		t.Fatal(err)
	}

	for name, mut := range map[string]func(*Config){
		"ID":            func(c *Config) { c.ID = "" },
		"addr":          func(c *Config) { c.Addr = "" },
		"transport":     func(c *Config) { c.Transport = nil },
		"PartitionBits": func(c *Config) { c.PartitionBits = 40 },
		"EpochPeriod":   func(c *Config) { c.EpochPeriod = -1 },
		"counts":        func(c *Config) { c.Tuning.MaxInFlight = -1 },
		"RenewDeadline": func(c *Config) { c.Tuning.Election.RenewDeadline = time.Minute },
	} {
		c := good
		mut(&c)

		_, err := New(c)
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestDefaults(t *testing.T) {
	m, err := New(Config{ID: "a", Addr: "a:1", Transport: nopTransport{}})
	if err != nil {
		t.Fatal(err)
	}

	if m.Incarnation() == 0 {
		t.Fatal("incarnation not chosen")
	}

	tu := m.cfg.Tuning
	if tu.MaxInFlight != DefaultMaxInFlight || tu.EpochAdvance != DefaultEpochAdvance || tu.LeaseTimeout <= 0 {
		t.Fatalf("defaults not applied: %+v", tu)
	}

	if m.Version() != (Version{}) || m.Members() != 0 || m.Leading() || m.Ready() {
		t.Fatal("unexpected status before Run")
	}

	if _, ok := m.Leader(); ok {
		t.Fatal("leader before Run")
	}

	if _, ok := m.Self(); ok {
		t.Fatal("self before Run")
	}

	m2, _ := New(Config{ID: "a", Addr: "a:1", Transport: nopTransport{}, Incarnation: 7})
	if m2.Incarnation() != 7 {
		t.Fatal("incarnation overridden")
	}
}

func TestBeforeRun(t *testing.T) {
	ks := MustKeyspace("blobs", 0)

	m, _ := New(Config{ID: "a", Addr: "a:1", Transport: nopTransport{}})
	if d := m.Route(ks, []byte("k")); d.Kind != KindNoRoute || !d.Next.IsZero() {
		t.Fatalf("no seeds: %+v", d)
	}

	m, _ = New(Config{ID: "a", Addr: "a:1", Transport: nopTransport{}, Seeds: []string{"a:1", "s:1", "s:2"}})

	d := m.Route(ks, []byte("k"))
	if d.Kind != KindGateway || (d.Next.Addr != "s:1" && d.Next.Addr != "s:2") || d.Header.Flags&FlagGateway == 0 {
		t.Fatalf("seeds: %+v", d)
	}

	if d2 := m.RouteHash(ks, ks.Hash([]byte("k"))); d2 != d {
		t.Fatalf("RouteHash %+v != %+v", d2, d)
	}

	out := make([]Decision, 2)
	m.RouteMany(ks, [][]byte{[]byte("k"), []byte("k")}, out)

	if out[0] != d || out[1] != d {
		t.Fatal("RouteMany mismatch")
	}

	if _, err := m.ServeControl(context.Background(), "x", []byte{1}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("ServeControl err = %v", err)
	}

	if err := m.Leave(context.Background()); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Leave err = %v", err)
	}

	// Stamps of a never-expiring keyspace are fresh; Observe on a gateway
	// is harmless.
	if !m.Fresh(ks, m.Stamp(ks)) {
		t.Fatal("blob stamp not fresh")
	}

	m.Observe(d.Next, Failure, time.Millisecond)

	if _, err := m.Forward(Header{}); !errors.Is(err, ErrBadHeader) {
		t.Fatalf("Forward err = %v", err)
	}
}

func TestRunOnceAndCancel(t *testing.T) {
	m, _ := New(Config{ID: "a", Addr: "a:1", Transport: nopTransport{}, Seeds: []string{"s:1"}})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)

	go func() { errc <- m.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for !m.running.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	if _, err := m.ServeControl(ctx, "x", []byte{0xff}); err != nil {
		t.Fatalf("ServeControl while running: %v", err)
	}

	if err := m.Run(ctx); !errors.Is(err, ErrAlreadyRun) {
		t.Fatalf("second Run: %v", err)
	}

	cancel()

	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v", err)
	}

	<-m.Done()

	if _, err := m.ServeControl(context.Background(), "x", []byte{1}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("after Run: %v", err)
	}
}

func TestExpandVersion(t *testing.T) {
	own := view.Version{Term: 5<<32 | 10, Seq: 3}
	for _, tc := range []struct {
		term uint32
		want uint64
	}{
		{10, 5<<32 | 10},
		{11, 5<<32 | 11},
		{9, 6<<32 | 9},
	} {
		if got := expandVersion(own, route.Note{Term: tc.term, Seq: 1}); got.Term != tc.want || got.Seq != 1 {
			t.Errorf("term %d: got %+v", tc.term, got)
		}
	}
}

func TestExpectedSizeFeedsElection(t *testing.T) {
	c := Config{ExpectedSize: 7}
	if got := c.nodeConfig().Elect.ExpectedClusterSize; got != 7 {
		t.Fatalf("ExpectedClusterSize = %d, want 7", got)
	}

	c = Config{}
	c.Tuning.Election.ExpectedClusterSize = 9

	if got := c.nodeConfig().Elect.ExpectedClusterSize; got != 9 {
		t.Fatalf("explicit ExpectedClusterSize = %d, want 9", got)
	}

	// Zero leaves the elector default in place.
	if got := (&Config{}).nodeConfig().Elect.ExpectedClusterSize; got != 0 {
		t.Fatalf("default = %d, want 0 (elector default)", got)
	}
}
