// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package chairs_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Azure/unbounded/internal/gantry/chairs"
)

// abandonedChairObjects returns a full set of Leases whose holders stopped
// renewing long ago and whose assignments predate the current epoch, which is
// what a wholesale node-pool replacement leaves behind.
func abandonedChairObjects(namespace string, renewedAt time.Time, epoch int64) []runtime.Object {
	objects := make([]runtime.Object, 0, chairs.Count)

	for index := range chairs.Count {
		micro := metav1.NewMicroTime(renewedAt)
		holder := fmt.Sprintf("departed-node-%02d", index)
		seconds := int32(60)

		objects = append(objects, &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name:      chairs.ID(index).Name(),
				Namespace: namespace,
				Labels:    map[string]string{chairs.LabelChair: "true"},
				Annotations: map[string]string{
					chairs.AnnotationEpoch: fmt.Sprint(epoch),
				},
			},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       &holder,
				LeaseDurationSeconds: &seconds,
				RenewTime:            &micro,
			},
		})
	}

	return objects
}

// TestManagerReclaimsAbandonedChairs covers recovery after a node pool is
// replaced: every Lease still records a holder, so no chair is empty. Treating
// occupancy as proof of a live holder would leave nothing claimable and the
// deployment could never regain a seed cohort.
func TestManagerReclaimsAbandonedChairs(t *testing.T) {
	const ns = "gantry-system"

	now := time.Unix(1_000_000, 0)
	// Renewed far enough in the past to be abandoned rather than merely late.
	objects := abandonedChairObjects(ns, now.Add(-time.Hour), 0)

	client := fake.NewClientset(objects...)
	store := chairs.NewStore(client.CoordinationV1().Leases(ns))
	manager := chairs.NewManager(chairs.ManagerOptions{
		Store:               store,
		Self:                chairs.Holder{PeerID: "replacement-node", P2PAddrs: []string{"/ip4/10.0.0.1/tcp/4001"}, TransferAddr: "10.0.0.1:5001"},
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
	defer cancel()

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
			t.Fatal("manager never reclaimed an abandoned chair; a replaced node pool cannot recover")
		case <-time.After(2 * time.Millisecond):
		}
	}

	cancel()
	<-done
}

// TestManagerDoesNotStealLiveChairs is the counterweight: a chair whose holder
// is renewing normally must not be taken over just because a claimant wants
// one.
func TestManagerDoesNotStealLiveChairs(t *testing.T) {
	const ns = "gantry-system"

	now := time.Unix(1_000_000, 0)
	// Renewed moments ago: late at worst, certainly not abandoned.
	objects := abandonedChairObjects(ns, now.Add(-time.Second), 0)

	client := fake.NewClientset(objects...)
	store := chairs.NewStore(client.CoordinationV1().Leases(ns))
	manager := chairs.NewManager(chairs.ManagerOptions{
		Store:               store,
		Self:                chairs.Holder{PeerID: "greedy-node", P2PAddrs: []string{"/ip4/10.0.0.2/tcp/4001"}, TransferAddr: "10.0.0.2:5001"},
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

	time.Sleep(250 * time.Millisecond)

	_, held := manager.Held()

	cancel()
	<-done

	if held {
		t.Fatal("manager took over a chair whose holder is still renewing")
	}
}
