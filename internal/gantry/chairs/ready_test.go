// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package chairs_test

import (
	"context"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/Azure/unbounded/internal/gantry/chairs"
)

// TestManagerReadyWithFewerHoldersThanTarget covers the first chair during
// startup and the first batch of a rolling upgrade.
//
// Requiring the complete target before reporting ready would block the
// remaining agents that are still converging on that target.
func TestManagerReadyWithFewerHoldersThanTarget(t *testing.T) {
	const ns = "gantry-system"

	now := time.Unix(2_000_000, 0)
	client := fake.NewClientset(emptyChairObjects(ns)...)
	store := chairs.NewStore(client.CoordinationV1().Leases(ns))
	manager := chairs.NewManager(chairs.ManagerOptions{
		Store:               store,
		Self:                chairs.Holder{PeerID: "upgraded-node", P2PAddrs: []string{"/ip4/10.0.0.3/tcp/4001"}, TransferAddr: "10.0.0.3:5001"},
		Now:                 func() time.Time { return now },
		StartupJitter:       time.Nanosecond,
		ClaimRoundPeriod:    time.Millisecond,
		ClaimJitter:         time.Nanosecond,
		ClaimInitialDivisor: 1,
		LeaseDuration:       time.Minute,
		RenewPeriod:         time.Hour,
		RotationPeriod:      time.Hour,
	})

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})

	go func() {
		defer close(done)

		manager.Run(ctx)
	}()

	deadline := time.After(5 * time.Second)

	for {
		if _, ok := manager.Held(); ok {
			break
		}

		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("manager never claimed a chair")
		case <-time.After(2 * time.Millisecond):
		}
	}

	ready := manager.Ready()

	cancel()
	<-done

	// Only this node's chair is occupied and selectable.
	if !ready {
		t.Fatal("a chair holder reports not ready with one selectable chair")
	}
}

func TestManagerReadyWithOneSelectableChairHeldByPeer(t *testing.T) {
	const ns = "gantry-system"

	client := fake.NewClientset()
	store := chairs.NewStore(client.CoordinationV1().Leases(ns))

	peer := chairs.Holder{PeerID: "seed", P2PAddrs: []string{"/ip4/10.0.0.2/tcp/4001"}, TransferAddr: "10.0.0.2:5001"}
	if _, err := store.Claim(context.Background(), 0, peer, 0, time.Minute, false, time.Unix(0, 0)); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	manager := chairs.NewManager(chairs.ManagerOptions{
		Store:          store,
		Self:           chairs.Holder{PeerID: "non-seed"},
		Now:            func() time.Time { return time.Unix(0, 0) },
		RotationPeriod: time.Hour,
		HolderCount:    50,
	})
	if err := manager.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	if !manager.Ready() {
		t.Fatal("manager is not ready with one selectable peer chair")
	}
}
