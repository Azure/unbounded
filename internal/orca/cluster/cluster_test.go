// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cluster

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/orca/config"
)

// fakePeerSource implements PeerSource for unit tests.
type fakePeerSource struct {
	mu    func() ([]Peer, error)
	calls atomic.Int64
}

func (f *fakePeerSource) Peers(_ context.Context) ([]Peer, error) {
	f.calls.Add(1)

	return f.mu()
}

// TestRefresh_RetainsPreviousOnError verifies that a discovery error
// after a successful refresh retains the previous peer-set rather
// than clobbering it with [Self].
//
// Regression test for B3.
func TestRefresh_RetainsPreviousOnError(t *testing.T) {
	t.Parallel()

	good := []Peer{
		{IP: "10.0.0.1", Self: false},
		{IP: "10.0.0.2", Self: true},
		{IP: "10.0.0.3", Self: false},
	}

	var failing atomic.Bool

	src := &fakePeerSource{
		mu: func() ([]Peer, error) {
			if failing.Load() {
				return nil, errors.New("transient DNS failure")
			}

			out := make([]Peer, len(good))
			copy(out, good)

			return out, nil
		},
	}

	c, err := New(t.Context(),
		config.Cluster{
			Service:           "test",
			SelfPodIP:         "10.0.0.2",
			MembershipRefresh: time.Hour, // disable auto-refresh; we drive it manually
		},
		WithPeerSource(src),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(func() { _ = c.Close(context.Background()) })

	// Initial refresh ran during New; verify good peers are loaded.
	if got := len(c.Peers()); got != 3 {
		t.Fatalf("initial Peers()=%d want 3", got)
	}

	failing.Store(true)
	// First few error refreshes: retain previous snapshot.
	for i := 0; i < maxStalePeerRefreshes; i++ {
		c.refresh(t.Context())

		if got := len(c.Peers()); got != 3 {
			t.Errorf("after error %d: Peers()=%d want 3 (retain previous)", i+1, got)
		}
	}
	// Next refresh exceeds the staleness ceiling -> fall back to self.
	c.refresh(t.Context())

	if got := c.Peers(); len(got) != 1 || !got[0].Self {
		t.Errorf("after ceiling exceeded: Peers()=%+v want [Self]", got)
	}
	// Recovery: source returns good peers again. Error counter resets.
	failing.Store(false)
	c.refresh(t.Context())

	if got := len(c.Peers()); got != 3 {
		t.Errorf("after recovery: Peers()=%d want 3", got)
	}

	if got := c.consecutiveRefreshErrors.Load(); got != 0 {
		t.Errorf("error counter not reset after success: got %d", got)
	}
}

// TestRefresh_BootstrapErrorFallsBackToSelf verifies that on bootstrap
// (no previous snapshot) a discovery error falls back to [Self]
// immediately - we cannot retain something that does not exist.
func TestRefresh_BootstrapErrorFallsBackToSelf(t *testing.T) {
	t.Parallel()

	src := &fakePeerSource{
		mu: func() ([]Peer, error) {
			return nil, errors.New("DNS not reachable yet")
		},
	}

	c, err := New(t.Context(),
		config.Cluster{
			Service:           "test",
			SelfPodIP:         "10.0.0.1",
			MembershipRefresh: time.Hour,
		},
		WithPeerSource(src),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(func() { _ = c.Close(context.Background()) })

	got := c.Peers()
	if len(got) != 1 || !got[0].Self {
		t.Errorf("bootstrap with error source: Peers()=%+v want [Self]", got)
	}
}

// TestRefresh_EmptyResultFallsBackToSelf verifies that a successful
// discovery returning zero peers (the legitimate "I'm alone" answer)
// still falls back to [Self] without bumping the error counter.
func TestRefresh_EmptyResultFallsBackToSelf(t *testing.T) {
	t.Parallel()

	src := &fakePeerSource{
		mu: func() ([]Peer, error) {
			return nil, nil // no error, zero peers
		},
	}

	c, err := New(t.Context(),
		config.Cluster{
			Service:           "test",
			SelfPodIP:         "10.0.0.1",
			MembershipRefresh: time.Hour,
		},
		WithPeerSource(src),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(func() { _ = c.Close(context.Background()) })

	got := c.Peers()
	if len(got) != 1 || !got[0].Self {
		t.Errorf("empty source: Peers()=%+v want [Self]", got)
	}

	if got := c.consecutiveRefreshErrors.Load(); got != 0 {
		t.Errorf("empty (non-error) result should not bump error counter; got %d", got)
	}
}
