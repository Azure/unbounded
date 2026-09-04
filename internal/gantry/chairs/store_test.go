// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package chairs_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Azure/unbounded/internal/gantry/chairs"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

func TestStoreClaimRequiresExpiryAndUnresponsiveHolder(t *testing.T) {
	now := time.Unix(1000, 0)
	oldHolder := "old-peer"
	duration := int32(30)
	renewTime := metav1.NewMicroTime(now.Add(-time.Minute))
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      chairs.ID(0).Name(),
			Namespace: "gantry-system",
			Labels:    map[string]string{chairs.LabelChair: "true"},
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &oldHolder,
			LeaseDurationSeconds: &duration,
			RenewTime:            &renewTime,
		},
	}
	client := fake.NewClientset(lease)
	store := chairs.NewStore(client.CoordinationV1().Leases("gantry-system"))
	holder := chairs.Holder{PeerID: "new-peer", P2PAddrs: []string{"/ip4/10.0.0.2/tcp/4001/p2p/new-peer"}, TransferAddr: "10.0.0.2:5001"}

	if _, err := store.Claim(context.Background(), 0, holder, 4, time.Minute, false, now); !errors.Is(err, chairs.ErrNotClaimable) {
		t.Fatalf("responsive expired claim error = %v, want ErrNotClaimable", err)
	}

	claimed, err := store.Claim(context.Background(), 0, holder, 4, time.Minute, true, now)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	if claimed.Holder.PeerID != holder.PeerID || claimed.Generation != 1 || claimed.AssignmentEpoch != 4 {
		t.Fatalf("claimed chair = %+v", claimed)
	}
}

func TestStoreClaimCreatesMissingChair(t *testing.T) {
	client := fake.NewClientset()
	store := chairs.NewStore(client.CoordinationV1().Leases("gantry-system"))
	holder := chairs.Holder{PeerID: "new-peer", TransferAddr: "10.0.0.2:5001"}

	claimed, err := store.Claim(context.Background(), 7, holder, 4, time.Minute, false, time.Unix(1000, 0))
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	if claimed.ID != 7 || claimed.Holder.PeerID != holder.PeerID || claimed.AssignmentEpoch != 4 {
		t.Fatalf("claimed chair = %+v", claimed)
	}

	lease, err := client.CoordinationV1().Leases("gantry-system").Get(context.Background(), chairs.ID(7).Name(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get created Lease: %v", err)
	}

	if lease.Labels[chairs.LabelChair] != "true" {
		t.Fatalf("created Lease labels = %v", lease.Labels)
	}
}

type countingReader struct {
	mu       sync.Mutex
	calls    int
	getCalls int
	snapshot chairs.Snapshot
	err      error
	block    chan struct{}
	getBlock chan struct{}
}

func (r *countingReader) Snapshot(_ context.Context, _ int64) (chairs.Snapshot, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()

	if r.block != nil {
		<-r.block
	}

	return r.snapshot, r.err
}

func (r *countingReader) Get(_ context.Context, id chairs.ID) (chairs.Chair, error) {
	r.mu.Lock()
	r.getCalls++
	snapshot := r.snapshot
	block := r.getBlock
	r.mu.Unlock()

	if block != nil {
		<-block
	}

	for _, chair := range snapshot.Chairs {
		if chair.ID == id {
			return chair, nil
		}
	}

	return chairs.Chair{}, fmt.Errorf("chair %s not found", id.Name())
}

func TestCacheCollapsesConcurrentTargetedRefresh(t *testing.T) {
	reader := &countingReader{snapshot: occupiedSnapshot(9), getBlock: make(chan struct{})}
	cache := chairs.NewCache(reader)

	const callers = 20

	var wg sync.WaitGroup
	wg.Add(callers)

	start := make(chan struct{})

	for range callers {
		go func() {
			defer wg.Done()

			<-start

			if _, err := cache.RefreshChair(context.Background(), 0); err != nil {
				t.Errorf("RefreshChair: %v", err)
			}
		}()
	}

	close(start)

	for {
		reader.mu.Lock()
		calls := reader.getCalls
		reader.mu.Unlock()

		if calls == 1 {
			break
		}
	}

	time.Sleep(20 * time.Millisecond)
	close(reader.getBlock)
	wg.Wait()

	reader.mu.Lock()
	defer reader.mu.Unlock()

	if reader.getCalls != 1 {
		t.Fatalf("targeted chair reads = %d, want 1", reader.getCalls)
	}
}

func TestCacheCollapsesConcurrentEpochRefresh(t *testing.T) {
	reader := &countingReader{block: make(chan struct{}), snapshot: occupiedSnapshot(9)}
	cache := chairs.NewCache(reader)

	const callers = 20

	var wg sync.WaitGroup
	wg.Add(callers)

	errs := make(chan error, callers)
	for range callers {
		go func() {
			defer wg.Done()

			_, err := cache.Snapshot(context.Background(), 9)
			errs <- err
		}()
	}

	for {
		reader.mu.Lock()
		calls := reader.calls
		reader.mu.Unlock()

		if calls == 1 {
			break
		}
	}

	close(reader.block)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
	}

	reader.mu.Lock()
	defer reader.mu.Unlock()

	if reader.calls != 1 {
		t.Fatalf("snapshot reads = %d, want 1", reader.calls)
	}
}

func TestCacheUsesOldSnapshotWhenAPIUnavailable(t *testing.T) {
	reader := &countingReader{snapshot: occupiedSnapshot(2)}

	cache := chairs.NewCache(reader)
	if _, err := cache.Snapshot(context.Background(), 2); err != nil {
		t.Fatalf("prime Snapshot: %v", err)
	}

	reader.err = errors.New("apiserver unavailable")

	snapshot, err := cache.Snapshot(context.Background(), 3)
	if err != nil {
		t.Fatalf("stale Snapshot: %v", err)
	}

	if !snapshot.Stale || snapshot.Epoch != 2 {
		t.Fatalf("snapshot = %+v, want stale epoch 2", snapshot)
	}
}

func TestCacheCollapsesConcurrentFailedRefresh(t *testing.T) {
	reader := &countingReader{snapshot: occupiedSnapshot(2)}

	cache := chairs.NewCache(reader)
	if _, err := cache.Snapshot(context.Background(), 2); err != nil {
		t.Fatalf("prime Snapshot: %v", err)
	}

	reader.err = errors.New("apiserver unavailable")
	reader.block = make(chan struct{})

	const callers = 20

	var wg sync.WaitGroup
	wg.Add(callers)
	results := make(chan chairs.Snapshot, callers)
	start := make(chan struct{})

	for range callers {
		go func() {
			defer wg.Done()

			<-start

			snapshot, err := cache.Snapshot(context.Background(), 3)
			if err != nil {
				t.Errorf("Snapshot: %v", err)
				return
			}

			results <- snapshot
		}()
	}

	close(start)

	for {
		reader.mu.Lock()
		calls := reader.calls
		reader.mu.Unlock()

		if calls == 2 {
			break
		}
	}

	time.Sleep(20 * time.Millisecond)

	close(reader.block)
	wg.Wait()
	close(results)

	for snapshot := range results {
		if !snapshot.Stale || snapshot.Epoch != 2 {
			t.Errorf("snapshot = %+v, want stale epoch 2", snapshot)
		}
	}

	reader.mu.Lock()
	defer reader.mu.Unlock()

	if reader.calls != 2 {
		t.Fatalf("snapshot reads = %d, want 2 including prime", reader.calls)
	}
}

func occupiedSnapshot(epoch int64) chairs.Snapshot {
	snapshot := chairs.Snapshot{Epoch: epoch}
	for index := range chairs.SeedCount {
		snapshot.Chairs = append(snapshot.Chairs, chairs.Chair{
			ID:              chairs.ID(index),
			AssignmentEpoch: epoch,
			Holder: chairs.Holder{
				PeerID:       ifaces.NodeID(fmt.Sprintf("peer-%d", index)),
				P2PAddrs:     []string{fmt.Sprintf("/ip4/10.0.0.%d/tcp/4001/p2p/peer-%d", index+1, index)},
				TransferAddr: fmt.Sprintf("10.0.0.%d:5001", index+1),
			},
		})
	}

	return snapshot
}
