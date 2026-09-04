// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package chairs_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Azure/unbounded/internal/gantry/chairs"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

type blockingLeaseClient struct{}

func (blockingLeaseClient) Create(ctx context.Context, _ *coordinationv1.Lease, _ metav1.CreateOptions) (*coordinationv1.Lease, error) {
	<-ctx.Done()

	return nil, ctx.Err()
}

func (blockingLeaseClient) Get(ctx context.Context, _ string, _ metav1.GetOptions) (*coordinationv1.Lease, error) {
	<-ctx.Done()

	return nil, ctx.Err()
}

func (blockingLeaseClient) List(ctx context.Context, _ metav1.ListOptions) (*coordinationv1.LeaseList, error) {
	<-ctx.Done()

	return nil, ctx.Err()
}

func (blockingLeaseClient) Update(ctx context.Context, _ *coordinationv1.Lease, _ metav1.UpdateOptions) (*coordinationv1.Lease, error) {
	<-ctx.Done()

	return nil, ctx.Err()
}

func TestManagerStartupClaimsAtMostOneChair(t *testing.T) {
	objects := emptyChairObjects("gantry-system")
	client := fake.NewClientset(objects...)
	store := chairs.NewStore(client.CoordinationV1().Leases("gantry-system"))
	manager := chairs.NewManager(chairs.ManagerOptions{
		Store:               store,
		Self:                chairs.Holder{PeerID: "self"},
		Now:                 func() time.Time { return time.Unix(0, 0) },
		StartupJitter:       time.Nanosecond,
		ClaimRoundPeriod:    time.Millisecond,
		ClaimJitter:         time.Nanosecond,
		ClaimInitialDivisor: 1,
		RenewPeriod:         time.Hour,
		RotationPeriod:      time.Hour,
	})

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)

		manager.Run(ctx)
	}()

	deadline := time.After(time.Second)

	for {
		if _, ok := manager.Held(); ok {
			break
		}

		select {
		case <-deadline:
			t.Fatal("manager did not claim a chair")
		case <-time.After(time.Millisecond):
		}
	}

	cancel()
	<-done

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
		t.Fatalf("chairs held by self = %d, want 1", held)
	}
}

func TestManagerInitializeBoundsLeaseAPI(t *testing.T) {
	manager := chairs.NewManager(chairs.ManagerOptions{
		Store:          chairs.NewStore(blockingLeaseClient{}),
		Self:           chairs.Holder{PeerID: "self"},
		APITimeout:     10 * time.Millisecond,
		RotationPeriod: time.Hour,
	})

	err := manager.Initialize(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Initialize error = %v, want DeadlineExceeded", err)
	}
}

func TestManagerReadyRequiresCurrentEpochSnapshot(t *testing.T) {
	clock := time.Unix(0, 0)

	objects := make([]runtime.Object, 0, chairs.SeedCount)
	for index := range chairs.SeedCount {
		holder := fmt.Sprintf("holder-%d", index)
		objects = append(objects, &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name:      chairs.ID(index).Name(),
				Namespace: "gantry-system",
				Labels:    map[string]string{chairs.LabelChair: "true"},
				Annotations: map[string]string{
					chairs.AnnotationEpoch:        "0",
					chairs.AnnotationP2PAddrs:     fmt.Sprintf(`["/ip4/10.0.0.%d/tcp/4001/p2p/%s"]`, index+1, holder),
					chairs.AnnotationTransferAddr: fmt.Sprintf("10.0.0.%d:5001", index+1),
				},
			},
			Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder},
		})
	}

	client := fake.NewClientset(objects...)

	manager := chairs.NewManager(chairs.ManagerOptions{
		Store:          chairs.NewStore(client.CoordinationV1().Leases("gantry-system")),
		Self:           chairs.Holder{PeerID: "self"},
		Now:            func() time.Time { return clock },
		RotationPeriod: time.Hour,
	})
	if err := manager.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	if !manager.Ready() {
		t.Fatal("manager is not ready with eight current chairs")
	}

	clock = clock.Add(time.Hour)

	if !manager.Ready() {
		t.Fatal("manager lost readiness during one-epoch rollover")
	}

	clock = clock.Add(time.Hour)

	if manager.Ready() {
		t.Fatal("manager remained ready with a snapshot older than one epoch")
	}
}

func TestManagerValidatesPreviousEpochUntilLeaseRenews(t *testing.T) {
	clock := time.Unix(0, 0)
	holder := "self"
	generation := int32(3)
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:        chairs.ID(3).Name(),
			Namespace:   "gantry-system",
			Labels:      map[string]string{chairs.LabelChair: "true"},
			Annotations: map[string]string{chairs.AnnotationEpoch: "0"},
		},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseTransitions: &generation},
	}
	client := fake.NewClientset(lease)

	manager := chairs.NewManager(chairs.ManagerOptions{
		Store: chairs.NewStore(client.CoordinationV1().Leases("gantry-system")), Self: chairs.Holder{PeerID: "self"},
		Now: func() time.Time { return clock }, RotationPeriod: time.Hour,
	})
	if err := manager.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	assignment := ifaces.ChairAssignment{ChairID: 3, Generation: 3, AssignmentEpoch: 0}
	clock = clock.Add(time.Hour)

	if !manager.ValidateChair(context.Background(), assignment) {
		t.Fatal("previous-epoch assignment was rejected before Lease renewal")
	}

	clock = clock.Add(time.Hour)

	if manager.ValidateChair(context.Background(), assignment) {
		t.Fatal("assignment older than one epoch was accepted")
	}
}

func TestManagerClaimsExpiredUnresponsiveChairOnDemand(t *testing.T) {
	now := time.Unix(5000, 0)
	oldHolder := "old"
	duration := int32(10)
	renewTime := metav1.NewMicroTime(now.Add(-time.Minute))
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: chairs.ID(9).Name(), Namespace: "gantry-system", Labels: map[string]string{chairs.LabelChair: "true"}},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &oldHolder,
			LeaseDurationSeconds: &duration,
			RenewTime:            &renewTime,
		},
	}
	client := fake.NewClientset(lease)
	store := chairs.NewStore(client.CoordinationV1().Leases("gantry-system"))

	manager := chairs.NewManager(chairs.ManagerOptions{
		Store:          store,
		Self:           chairs.Holder{PeerID: "self"},
		Now:            func() time.Time { return now },
		ClaimJitter:    time.Nanosecond,
		RotationPeriod: time.Hour,
	})
	if err := manager.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	stale, err := store.Get(context.Background(), 9)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	claimed, ok, err := manager.ClaimFailed(context.Background(), stale)
	if err != nil {
		t.Fatalf("ClaimFailed: %v", err)
	}

	if !ok || claimed.Holder.PeerID != "self" {
		t.Fatalf("claimed = %+v ok=%t", claimed, ok)
	}
}

func TestManagerSerializesConcurrentDeadChairClaims(t *testing.T) {
	now := time.Unix(7000, 0)
	duration := int32(10)
	renewTime := metav1.NewMicroTime(now.Add(-time.Minute))
	objects := make([]runtime.Object, 0, 2)

	for index := range 2 {
		holder := fmt.Sprintf("dead-%d", index)
		objects = append(objects, &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name:      chairs.ID(index).Name(),
				Namespace: "gantry-system",
				Labels:    map[string]string{chairs.LabelChair: "true"},
			},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       &holder,
				LeaseDurationSeconds: &duration,
				RenewTime:            &renewTime,
			},
		})
	}

	client := fake.NewClientset(objects...)
	updateStarted := make(chan struct{}, 1)
	releaseUpdate := make(chan struct{})

	client.PrependReactor("update", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		select {
		case updateStarted <- struct{}{}:
		default:
		}

		<-releaseUpdate

		return false, nil, nil
	})

	store := chairs.NewStore(client.CoordinationV1().Leases("gantry-system"))

	manager := chairs.NewManager(chairs.ManagerOptions{
		Store:          store,
		Self:           chairs.Holder{PeerID: "self"},
		Now:            func() time.Time { return now },
		ClaimJitter:    time.Nanosecond,
		RotationPeriod: time.Hour,
	})
	if err := manager.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	first, _ := store.Get(context.Background(), 0)
	second, _ := store.Get(context.Background(), 1)
	firstDone := make(chan error, 1)

	go func() {
		_, _, err := manager.ClaimFailed(context.Background(), first)
		firstDone <- err
	}()

	<-updateStarted

	if _, claimed, err := manager.ClaimFailed(context.Background(), second); err != nil || claimed {
		t.Fatalf("second concurrent claim = claimed:%t err:%v", claimed, err)
	}

	close(releaseUpdate)

	if err := <-firstDone; err != nil {
		t.Fatalf("first claim: %v", err)
	}

	snapshot, err := store.Snapshot(context.Background(), manager.CurrentEpoch())
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
		t.Fatalf("chairs held by self = %d, want 1", held)
	}
}

func TestManagerAcceptsOnlyOneSuccessorReservation(t *testing.T) {
	client := fake.NewClientset(emptyChairObjects("gantry-system")...)

	store := chairs.NewStore(client.CoordinationV1().Leases("gantry-system"))
	if _, err := store.Claim(context.Background(), 1, chairs.Holder{PeerID: "holder"}, 0, time.Minute, false, time.Unix(0, 0)); err != nil {
		t.Fatalf("seed holder Claim: %v", err)
	}

	manager := chairs.NewManager(chairs.ManagerOptions{
		Store:          store,
		Self:           chairs.Holder{PeerID: "self", TransferAddr: "10.0.0.1:5001"},
		Now:            func() time.Time { return time.Unix(0, 0) },
		RotationPeriod: time.Hour,
	})
	if err := manager.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	first := ifaces.ChairAssignment{ChairID: 1, Generation: 2, AssignmentEpoch: 1}
	if _, accepted := manager.AcceptChair(context.Background(), "attacker", first); accepted {
		t.Fatal("rotation offer from non-holder was accepted")
	}

	endpoint, accepted := manager.AcceptChair(context.Background(), "holder", first)
	if !accepted || endpoint.PeerID != "self" {
		t.Fatalf("first offer accepted=%t endpoint=%+v", accepted, endpoint)
	}

	second := ifaces.ChairAssignment{ChairID: 2, Generation: 2, AssignmentEpoch: 1}
	if _, accepted := manager.AcceptChair(context.Background(), "holder", second); accepted {
		t.Fatal("second distinct chair reservation was accepted")
	}

	expired := chairs.Chair{
		ID:            3,
		Holder:        chairs.Holder{PeerID: "dead"},
		RenewTime:     time.Unix(-100, 0),
		LeaseDuration: time.Second,
	}
	if _, claimed, err := manager.ClaimFailed(context.Background(), expired); err != nil || claimed {
		t.Fatalf("reserved successor dead-chair claim = claimed:%t err:%v", claimed, err)
	}
}

func TestManagerRecoversSuccessorReservationAfterRestart(t *testing.T) {
	now := time.Unix(8000, 0)
	holder := "holder"
	deadHolder := "dead"
	duration := int32(10)
	generation := int32(4)
	renewed := metav1.NewMicroTime(now)
	expired := metav1.NewMicroTime(now.Add(-time.Minute))
	client := fake.NewClientset(
		&coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name:      chairs.ID(1).Name(),
				Namespace: "gantry-system",
				Labels:    map[string]string{chairs.LabelChair: "true"},
				Annotations: map[string]string{
					chairs.AnnotationEpoch:      "2",
					chairs.AnnotationNextPeerID: "self",
				},
			},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       &holder,
				LeaseTransitions:     &generation,
				LeaseDurationSeconds: &duration,
				RenewTime:            &renewed,
			},
		},
		&coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: chairs.ID(2).Name(), Namespace: "gantry-system", Labels: map[string]string{chairs.LabelChair: "true"}},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       &deadHolder,
				LeaseDurationSeconds: &duration,
				RenewTime:            &expired,
			},
		},
	)
	store := chairs.NewStore(client.CoordinationV1().Leases("gantry-system"))

	manager := chairs.NewManager(chairs.ManagerOptions{
		Store:          store,
		Self:           chairs.Holder{PeerID: "self"},
		Now:            func() time.Time { return now },
		ClaimJitter:    time.Nanosecond,
		RotationPeriod: time.Hour,
	})
	if err := manager.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	dead, err := store.Get(context.Background(), 2)
	if err != nil {
		t.Fatalf("Get dead chair: %v", err)
	}

	if _, claimed, err := manager.ClaimFailed(context.Background(), dead); err != nil || claimed {
		t.Fatalf("reserved successor claimed another chair: claimed=%t err=%v", claimed, err)
	}
}

func TestStoreRenewPreservesSuccessorAndRotateIncrementsGeneration(t *testing.T) {
	now := time.Unix(9000, 0)
	holderID := "holder"
	durationSeconds := int32(60)
	generation := int32(3)
	renewTime := metav1.NewMicroTime(now)
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:        chairs.ID(3).Name(),
			Namespace:   "gantry-system",
			Labels:      map[string]string{chairs.LabelChair: "true"},
			Annotations: map[string]string{chairs.AnnotationEpoch: "7"},
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &holderID,
			LeaseDurationSeconds: &durationSeconds,
			LeaseTransitions:     &generation,
			RenewTime:            &renewTime,
		},
	}
	client := fake.NewClientset(lease)
	store := chairs.NewStore(client.CoordinationV1().Leases("gantry-system"))
	next := chairs.Holder{PeerID: "next", P2PAddrs: []string{"/ip4/10.0.0.2/tcp/4001/p2p/next"}, TransferAddr: "10.0.0.2:5001"}

	if _, err := store.SetNextHolder(context.Background(), 3, "holder", 3, next); err != nil {
		t.Fatalf("SetNextHolder: %v", err)
	}

	renewed, err := store.Renew(context.Background(), 3, chairs.Holder{PeerID: "holder"}, 7, time.Minute, now.Add(20*time.Second))
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}

	if renewed.NextHolder.PeerID != "next" {
		t.Fatalf("renewed successor = %+v, want next", renewed.NextHolder)
	}

	rotated, err := store.Rotate(context.Background(), 3, "holder", 3, 8, time.Minute, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	if rotated.Holder.PeerID != "next" || rotated.Generation != 4 || rotated.AssignmentEpoch != 8 {
		t.Fatalf("rotated chair = %+v", rotated)
	}
}

func emptyChairObjects(namespace string) []runtime.Object {
	objects := make([]runtime.Object, 0, chairs.Count)
	for index := range chairs.Count {
		objects = append(objects, &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name:      chairs.ID(index).Name(),
				Namespace: namespace,
				Labels:    map[string]string{chairs.LabelChair: "true"},
				Annotations: map[string]string{
					chairs.AnnotationEpoch: fmt.Sprint(0),
				},
			},
		})
	}

	return objects
}
