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

// TestManagerReadyWithFewerHoldersThanSeedCount covers the first batch of a
// rolling upgrade and any cluster smaller than SeedCount.
//
// A node holds exactly one chair, so requiring SeedCount occupied chairs before
// reporting ready is unsatisfiable until SeedCount nodes are already running
// the new build. On an eight-node cluster at maxUnavailable 50% only four pods
// are replaced at a time: they would never become ready, the availability
// budget would stay spent, and the rollout could never create the holders it
// was waiting for. Holding a chair makes a node a usable seed on its own.
func TestManagerReadyWithFewerHoldersThanSeedCount(t *testing.T) {
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

	// Far fewer than SeedCount chairs are occupied here: this node holds one
	// and nothing else is running.
	if !ready {
		t.Fatal("a chair holder reports not ready with fewer than SeedCount chairs occupied; a rolling upgrade of a small cluster would deadlock")
	}
}
