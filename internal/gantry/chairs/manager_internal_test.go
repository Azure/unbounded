// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package chairs

import (
	"context"
	"fmt"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

type rotationStub struct {
	endpoint ifaces.PeerEndpoint
	calls    []ifaces.ChairAssignment
}

func (stub *rotationStub) OfferChair(_ context.Context, _ ifaces.NodeID, assignment ifaces.ChairAssignment) (ifaces.PeerEndpoint, bool, error) {
	stub.calls = append(stub.calls, assignment)
	return stub.endpoint, true, nil
}

func TestClaimEligibilityOnlyWidens(t *testing.T) {
	for index := range 10_000 {
		peerID := ifaces.NodeID(fmt.Sprintf("peer-%05d", index))
		eligible := false

		for round := uint64(0); round < 12; round++ {
			current := claimEligible(peerID, 7, round, 2048)
			if eligible && !current {
				t.Fatalf("peer %s became ineligible at round %d", peerID, round)
			}

			eligible = current
		}
	}
}

func TestClaimEligibilityEventuallyIncludesEntireCluster(t *testing.T) {
	const clusterSize = 100_000

	countAtRoundZero := 0
	countAtRoundHundred := 0

	for index := range clusterSize {
		peerID := ifaces.NodeID(fmt.Sprintf("peer-%06d", index))
		if claimEligible(peerID, 11, 0, 2048) {
			countAtRoundZero++
		}

		if claimEligible(peerID, 11, 100, 2048) {
			countAtRoundHundred++
		}
	}

	if countAtRoundZero < 25 || countAtRoundZero > 75 {
		t.Fatalf("initial eligible pool = %d, want approximately 49", countAtRoundZero)
	}

	if countAtRoundHundred != clusterSize {
		t.Fatalf("fully widened eligible pool = %d, want %d", countAtRoundHundred, clusterSize)
	}
}

func TestManagerScalesObservationCadence(t *testing.T) {
	client := fake.NewClientset()
	manager := NewManager(ManagerOptions{
		Store:               NewStore(client.CoordinationV1().Leases("gantry-system")),
		Self:                Holder{PeerID: "self"},
		ClusterSizeEstimate: 100_000,
	})

	if manager.observationRounds != 200 {
		t.Fatalf("observation rounds = %d, want 200", manager.observationRounds)
	}
}

func TestManagerRetriesDuplicateVacate(t *testing.T) {
	clock := time.Unix(0, 0)
	holder := "self"
	duration := int32(60)
	renewTime := metav1.NewMicroTime(clock)

	objects := make([]runtime.Object, 0, 2)
	for index := range 2 {
		objects = append(objects, &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name:        ID(index).Name(),
				Namespace:   "gantry-system",
				Labels:      map[string]string{LabelChair: "true"},
				Annotations: map[string]string{AnnotationEpoch: "0"},
			},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       &holder,
				LeaseDurationSeconds: &duration,
				RenewTime:            &renewTime,
			},
		})
	}

	client := fake.NewClientset(objects...)
	failOnce := true

	client.PrependReactor("update", "leases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		update := action.(k8stesting.UpdateAction).GetObject().(*coordinationv1.Lease)
		if failOnce && update.Name == ID(1).Name() && update.Spec.HolderIdentity == nil {
			failOnce = false
			return true, nil, fmt.Errorf("transient update failure")
		}

		return false, nil, nil
	})

	store := NewStore(client.CoordinationV1().Leases("gantry-system"))

	manager := NewManager(ManagerOptions{
		Store: store, Self: Holder{PeerID: "self"}, Now: func() time.Time { return clock },
		LeaseDuration: time.Minute, RotationPeriod: time.Hour,
	})
	if err := manager.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	if len(manager.duplicateChairs) != 1 {
		t.Fatalf("queued duplicates = %v, want chair 1", manager.duplicateChairs)
	}

	manager.maintain(context.Background())

	if len(manager.duplicateChairs) != 0 {
		t.Fatalf("queued duplicates after retry = %v, want empty", manager.duplicateChairs)
	}

	snapshot, err := store.Snapshot(context.Background(), 0)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	held := 0

	for _, chair := range snapshot.Chairs {
		if chair.Holder.PeerID == "self" {
			held++
		}
	}

	if held != 1 {
		t.Fatalf("chairs held after retry = %d, want 1", held)
	}
}

func TestManagerPlansAndCompletesRotation(t *testing.T) {
	clock := time.Unix(0, int64(55*time.Minute))
	holderID := "holder"
	durationSeconds := int32(60)
	generation := int32(3)
	renewTime := metav1.NewMicroTime(clock)
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:        ID(4).Name(),
			Namespace:   "gantry-system",
			Labels:      map[string]string{LabelChair: "true"},
			Annotations: map[string]string{AnnotationEpoch: "0"},
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &holderID,
			LeaseDurationSeconds: &durationSeconds,
			LeaseTransitions:     &generation,
			RenewTime:            &renewTime,
		},
	}
	client := fake.NewClientset(lease)
	store := NewStore(client.CoordinationV1().Leases("gantry-system"))
	next := Holder{PeerID: "next", P2PAddrs: []string{"/ip4/10.0.0.2/tcp/4001/p2p/next"}, TransferAddr: "10.0.0.2:5001"}
	rotation := &rotationStub{endpoint: next}
	manager := NewManager(ManagerOptions{
		Store:          store,
		Self:           Holder{PeerID: "holder"},
		Rotation:       rotation,
		Candidates:     func() []Holder { return []Holder{next} },
		Now:            func() time.Time { return clock },
		LeaseDuration:  time.Minute,
		RotationPeriod: time.Hour,
		RotationLead:   10 * time.Minute,
	})

	held, err := store.Get(context.Background(), 4)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	manager.setHeld(held)
	manager.prepareRotation(context.Background(), held)

	planned, err := store.Get(context.Background(), 4)
	if err != nil {
		t.Fatalf("Get planned: %v", err)
	}

	if planned.NextHolder.PeerID != "next" || len(rotation.calls) != 1 {
		t.Fatalf("planned chair = %+v calls=%v", planned, rotation.calls)
	}

	clock = time.Unix(0, int64(time.Hour))

	manager.maintain(context.Background())

	rotated, err := store.Get(context.Background(), 4)
	if err != nil {
		t.Fatalf("Get rotated: %v", err)
	}

	if rotated.Holder.PeerID != "next" || rotated.Generation != 4 || rotated.AssignmentEpoch != 1 {
		t.Fatalf("rotated chair = %+v", rotated)
	}

	if _, ok := manager.Held(); ok {
		t.Fatal("old holder still considers itself active after rotation")
	}
}

func TestSuccessorWaitsForHolderRotation(t *testing.T) {
	clock := time.Unix(0, int64(55*time.Minute))
	holderID := "holder"
	durationSeconds := int32(60)
	generation := int32(3)
	renewTime := metav1.NewMicroTime(clock)
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:        ID(5).Name(),
			Namespace:   "gantry-system",
			Labels:      map[string]string{LabelChair: "true"},
			Annotations: map[string]string{AnnotationEpoch: "0"},
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &holderID,
			LeaseDurationSeconds: &durationSeconds,
			LeaseTransitions:     &generation,
			RenewTime:            &renewTime,
		},
	}
	client := fake.NewClientset(lease)
	store := NewStore(client.CoordinationV1().Leases("gantry-system"))
	next := Holder{PeerID: "next", P2PAddrs: []string{"/ip4/10.0.0.2/tcp/4001/p2p/next"}}
	holder := NewManager(ManagerOptions{
		Store: store, Self: Holder{PeerID: "holder"}, Rotation: &rotationStub{endpoint: next},
		Candidates: func() []Holder { return []Holder{next} }, Now: func() time.Time { return clock },
		LeaseDuration: time.Minute, RotationPeriod: time.Hour, RotationLead: 10 * time.Minute,
	})
	successor := NewManager(ManagerOptions{
		Store: store, Self: next, Now: func() time.Time { return clock },
		LeaseDuration: time.Minute, RotationPeriod: time.Hour,
	})

	if err := holder.Initialize(context.Background()); err != nil {
		t.Fatalf("holder Initialize: %v", err)
	}

	held, _ := holder.Held()
	holder.prepareRotation(context.Background(), held)

	if err := successor.Initialize(context.Background()); err != nil {
		t.Fatalf("successor Initialize: %v", err)
	}

	clock = time.Unix(0, int64(time.Hour))

	successor.maintain(context.Background())

	if _, ok := successor.Held(); ok {
		t.Fatal("successor adopted before holder rotated")
	}

	holder.maintain(context.Background())
	successor.maintain(context.Background())

	adopted, ok := successor.Held()
	if !ok || adopted.Holder.PeerID != "next" {
		t.Fatalf("successor did not adopt delayed rotation: %+v, %t", adopted, ok)
	}
}

func TestHolderRefreshesSnapshotUntilBootstrapConnects(t *testing.T) {
	clock := time.Unix(0, 0)
	holderID := "self"
	durationSeconds := int32(60)
	renewTime := metav1.NewMicroTime(clock)
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:        ID(0).Name(),
			Namespace:   "gantry-system",
			Labels:      map[string]string{LabelChair: "true"},
			Annotations: map[string]string{AnnotationEpoch: "0"},
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &holderID,
			LeaseDurationSeconds: &durationSeconds,
			RenewTime:            &renewTime,
		},
	}
	client := fake.NewClientset(lease)
	connectCalls := 0

	manager := NewManager(ManagerOptions{
		Store: NewStore(client.CoordinationV1().Leases("gantry-system")),
		Self:  Holder{PeerID: "self"},
		Now:   func() time.Time { return clock },
		Connect: func(context.Context, []string) int {
			connectCalls++
			if connectCalls > 1 {
				return 1
			}

			return 0
		},
		LeaseDuration: time.Minute, RotationPeriod: time.Hour,
	})
	if err := manager.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	manager.maintain(context.Background())

	if connectCalls < 2 {
		t.Fatalf("bootstrap connect attempts = %d, want retry after renewal", connectCalls)
	}
}
