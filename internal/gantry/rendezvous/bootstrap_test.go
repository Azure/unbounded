// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rendezvous

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestBootstrapLeaseContactBecomesReady(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	self := testPeerID(t)
	holderID := testPeerID(t)
	client := fake.NewSimpleClientset(testSlots(t, 2, now, holderID)...)
	manager := testManager(t, client, self, now, func(opts *Options) {
		opts.SlotCount = 2
		opts.ReadsPerRound = 2
	})

	var routingSize atomic.Int32

	bootstrap, err := NewBootstrap(BootstrapOptions{
		Manager: manager,
		PeerID:  self,
		Connect: func(_ context.Context, peers []string) DialResult {
			if len(peers) > 0 {
				routingSize.Store(1)
				return DialResult{Attempted: 1, Connected: 1}
			}

			return DialResult{}
		},
		RoutingTableSize: func() int { return int(routingSize.Load()) },
		SelfTest:         func(context.Context) bool { return true },
		RetryMin:         time.Millisecond,
		RetryMax:         2 * time.Millisecond,
		RenewInterval:    time.Hour,
		PeerCachePath:    filepath.Join(t.TempDir(), "peers.json"),
	})
	if err != nil {
		t.Fatalf("NewBootstrap: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go bootstrap.Run(ctx)

	select {
	case <-bootstrap.Ready():
	case <-time.After(time.Second):
		t.Fatal("bootstrap did not become ready")
	}

	claimDeadline := time.After(time.Second)

	for len(client.Actions()) < 4 {
		select {
		case <-claimDeadline:
			t.Fatalf("bootstrap became Ready but claim pass actions = %d, want at least 4", len(client.Actions()))
		default:
			time.Sleep(time.Millisecond)
		}
	}

	actionsAtReady := len(client.Actions())

	time.Sleep(20 * time.Millisecond)

	if got := len(client.Actions()); got != actionsAtReady {
		t.Fatalf("Kubernetes actions after Ready = %d, want 0", got-actionsAtReady)
	}
}

func TestBootstrapBrandNewPeerStaysNotReadyWithoutContacts(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	self := testPeerID(t)
	client := fake.NewSimpleClientset(testSlots(t, 1, now, "")...)
	manager := testManager(t, client, self, now, func(opts *Options) {
		opts.SlotCount = 1
		opts.ReadsPerRound = 1
		opts.ClaimAttemptsPerRound = 1
	})

	bootstrap, err := NewBootstrap(BootstrapOptions{
		Manager:          manager,
		PeerID:           self,
		Connect:          func(context.Context, []string) DialResult { return DialResult{} },
		RoutingTableSize: func() int { return 0 },
		SelfTest:         func(context.Context) bool { return true },
		RetryMin:         time.Millisecond,
		RetryMax:         2 * time.Millisecond,
		RenewInterval:    time.Hour,
	})
	if err != nil {
		t.Fatalf("NewBootstrap: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go bootstrap.Run(ctx)

	time.Sleep(20 * time.Millisecond)
	cancel()

	if bootstrap.IsReady() {
		t.Fatal("brand-new clustered peer became Ready without a contact")
	}
}

func TestBootstrapSingleNodeReadyWithoutContact(t *testing.T) {
	self := testPeerID(t)

	bootstrap, err := NewBootstrap(BootstrapOptions{
		PeerID:           self,
		Connect:          func(context.Context, []string) DialResult { return DialResult{} },
		RoutingTableSize: func() int { return 0 },
		SelfTest:         func(context.Context) bool { return true },
		SingleNode:       true,
		RetryMin:         time.Millisecond,
		RetryMax:         2 * time.Millisecond,
		RenewInterval:    time.Hour,
	})
	if err != nil {
		t.Fatalf("NewBootstrap: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go bootstrap.Run(ctx)

	select {
	case <-bootstrap.Ready():
	case <-time.After(time.Second):
		t.Fatal("single-node bootstrap did not become ready")
	}
}

func TestBootstrapReadsBeforeClaim(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	self := testPeerID(t)
	client := fake.NewSimpleClientset(testSlots(t, 1, now, "")...)
	manager := testManager(t, client, self, now, singleSlotOptions)

	bootstrap, err := NewBootstrap(BootstrapOptions{
		Manager:          manager,
		PeerID:           self,
		Connect:          func(context.Context, []string) DialResult { return DialResult{} },
		RoutingTableSize: func() int { return 0 },
		SelfTest:         func(context.Context) bool { return true },
		RetryMin:         time.Millisecond,
		RetryMax:         time.Millisecond,
		RenewInterval:    time.Hour,
	})
	if err != nil {
		t.Fatalf("NewBootstrap: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go bootstrap.Run(ctx)

	deadline := time.After(time.Second)

	for len(client.Actions()) < 3 {
		select {
		case <-deadline:
			cancel()
			t.Fatalf("actions = %v, want read GET, claim GET, update", client.Actions())
		default:
			time.Sleep(time.Millisecond)
		}
	}

	cancel()

	verbs := []string{client.Actions()[0].GetVerb(), client.Actions()[1].GetVerb(), client.Actions()[2].GetVerb()}
	if verbs[0] != "get" || verbs[1] != "get" || verbs[2] != "update" {
		t.Fatalf("operation order = %v, want [get get update]", verbs)
	}
}

func TestBootstrapCacheSuccessSkipsLeaseDiscovery(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	self := testPeerID(t)
	cachedPeer := testPeerID(t)
	client := fake.NewSimpleClientset(testSlots(t, 1, now, "")...)
	manager := testManager(t, client, self, now, singleSlotOptions)

	cachePath := filepath.Join(t.TempDir(), "peers.json")
	if err := writePeerCache(cachePath, []string{testFullAddr(cachedPeer, "10.0.0.2")}); err != nil {
		t.Fatalf("writePeerCache: %v", err)
	}

	var routingSize atomic.Int32

	bootstrap, err := NewBootstrap(BootstrapOptions{
		Manager: manager,
		PeerID:  self,
		Connect: func(context.Context, []string) DialResult {
			routingSize.Store(1)

			return DialResult{Attempted: 1, Connected: 1}
		},
		RoutingTableSize: func() int { return int(routingSize.Load()) },
		SelfTest:         func(context.Context) bool { return true },
		RetryMin:         time.Millisecond,
		RetryMax:         time.Second,
		RenewInterval:    time.Hour,
		PeerCachePath:    cachePath,
	})
	if err != nil {
		t.Fatalf("NewBootstrap: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go bootstrap.Run(ctx)

	select {
	case <-bootstrap.Ready():
	case <-time.After(time.Second):
		cancel()
		t.Fatal("cached bootstrap did not become ready")
	}

	time.Sleep(10 * time.Millisecond)
	cancel()

	if got := countLeaseActions(client.Actions(), "get"); got != 1 {
		t.Fatalf("Lease GETs = %d, want one claim GET and no discovery GET", got)
	}

	if got := countLeaseActions(client.Actions(), "update"); got != 1 {
		t.Fatalf("Lease updates = %d, want one claim", got)
	}
}

func TestBootstrapSelfTestGatesClusteredReadiness(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	self := testPeerID(t)
	client := fake.NewSimpleClientset(testSlots(t, 1, now, "")...)
	manager := testManager(t, client, self, now, singleSlotOptions)

	bootstrap, err := NewBootstrap(BootstrapOptions{
		Manager:          manager,
		PeerID:           self,
		Connect:          func(context.Context, []string) DialResult { return DialResult{} },
		RoutingTableSize: func() int { return 1 },
		SelfTest:         func(context.Context) bool { return false },
		RetryMin:         time.Millisecond,
		RetryMax:         2 * time.Millisecond,
		RenewInterval:    time.Hour,
	})
	if err != nil {
		t.Fatalf("NewBootstrap: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go bootstrap.Run(ctx)

	time.Sleep(20 * time.Millisecond)
	cancel()

	if bootstrap.IsReady() {
		t.Fatal("bootstrap became Ready after a failed DHT self-test")
	}
}

func TestEstablishedDHTReadyDuringLeaseAPIFailure(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	self := testPeerID(t)
	client := fake.NewSimpleClientset(testSlots(t, 1, now, "")...)
	client.PrependReactor("*", "leases", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("API unavailable")
	})
	manager := testManager(t, client, self, now, singleSlotOptions)

	bootstrap, err := NewBootstrap(BootstrapOptions{
		Manager:          manager,
		PeerID:           self,
		Connect:          func(context.Context, []string) DialResult { return DialResult{} },
		RoutingTableSize: func() int { return 1 },
		SelfTest:         func(context.Context) bool { return true },
		RetryMin:         time.Millisecond,
		RetryMax:         time.Second,
		RenewInterval:    time.Hour,
	})
	if err != nil {
		t.Fatalf("NewBootstrap: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go bootstrap.Run(ctx)

	select {
	case <-bootstrap.Ready():
	case <-time.After(time.Second):
		t.Fatal("established DHT did not become Ready during API failure")
	}
}

func TestEstablishedDHTClaimsAfterLeaseAPIRecovers(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	self := testPeerID(t)
	client := fake.NewSimpleClientset(testSlots(t, 1, now, "")...)

	var unavailable atomic.Bool
	unavailable.Store(true)
	client.PrependReactor("*", "leases", func(clienttesting.Action) (bool, runtime.Object, error) {
		if unavailable.Load() {
			return true, nil, errors.New("API unavailable")
		}

		return false, nil, nil
	})

	manager := testManager(t, client, self, now, singleSlotOptions)

	bootstrap, err := NewBootstrap(BootstrapOptions{
		Manager:          manager,
		PeerID:           self,
		Connect:          func(context.Context, []string) DialResult { return DialResult{} },
		RoutingTableSize: func() int { return 1 },
		SelfTest:         func(context.Context) bool { return true },
		RetryMin:         time.Millisecond,
		RetryMax:         2 * time.Millisecond,
		RenewInterval:    time.Hour,
	})
	if err != nil {
		t.Fatalf("NewBootstrap: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go bootstrap.Run(ctx)

	select {
	case <-bootstrap.Ready():
	case <-time.After(time.Second):
		t.Fatal("established DHT did not become Ready during API failure")
	}

	unavailable.Store(false)

	deadline := time.After(time.Second)

	for manager.HeldSlot() == "" {
		select {
		case <-deadline:
			t.Fatalf("slot not claimed after API recovery; actions = %v", client.Actions())
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func TestPeerCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peers.json")

	want := []string{"/ip4/10.0.0.1/tcp/4001/p2p/peer-a"}
	if err := writePeerCache(path, want); err != nil {
		t.Fatalf("writePeerCache: %v", err)
	}

	got, err := readPeerCache(path)
	if err != nil {
		t.Fatalf("readPeerCache: %v", err)
	}

	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("peer cache = %v, want %v", got, want)
	}
}

func TestPeerCacheRejectsOversizedReadAndWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peers.json")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxPeerCacheBytes+1)), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := readPeerCache(path); err == nil {
		t.Fatal("readPeerCache accepted oversized file")
	}

	oversized := []string{strings.Repeat("x", maxPeerCacheBytes+1)}
	if err := writePeerCache(path, oversized); err == nil {
		t.Fatal("writePeerCache accepted oversized payload")
	}
}
