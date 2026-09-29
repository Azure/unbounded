// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package chairs_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/chairs"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

func TestRankIsDeterministicAndUsesOccupiedBackups(t *testing.T) {
	snapshot := chairs.Snapshot{Epoch: 7}

	for index := range chairs.DefaultCount {
		if index%3 == 0 {
			continue
		}

		snapshot.Chairs = append(snapshot.Chairs, chairs.Chair{
			ID:              chairs.ID(index),
			Holder:          testHolder(ifaces.NodeID("peer-" + chairs.ID(index).Name())),
			AssignmentEpoch: 7,
		})
	}

	d := digest.MustParse("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	first := chairs.Rank(snapshot, d, chairs.DefaultCount)
	second := chairs.Rank(snapshot, d, chairs.DefaultCount)

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("rankings differ: first=%v second=%v", first, second)
	}

	if got, want := len(first), snapshot.OccupiedCount(); got != want {
		t.Fatalf("ranked chairs = %d, want %d occupied chairs", got, want)
	}

	for _, chair := range first {
		if chair.ID%3 == 0 {
			t.Fatalf("empty chair %s appeared in ranking", chair.ID.Name())
		}
	}

	if len(first) < chairs.SeedCount {
		t.Fatalf("ranked chairs = %d, need at least %d seeds", len(first), chairs.SeedCount)
	}
}

func TestRankChangesAcrossDigests(t *testing.T) {
	snapshot := chairs.Snapshot{}
	for index := range chairs.DefaultCount {
		snapshot.Chairs = append(snapshot.Chairs, chairs.Chair{
			ID:     chairs.ID(index),
			Holder: testHolder(ifaces.NodeID(chairs.ID(index).Name())),
		})
	}

	first := chairs.Rank(snapshot, digest.MustParse("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), chairs.DefaultCount)
	second := chairs.Rank(snapshot, digest.MustParse("sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"), chairs.DefaultCount)

	if reflect.DeepEqual(first[:chairs.SeedCount], second[:chairs.SeedCount]) {
		t.Fatalf("top seed chairs unexpectedly identical: %v", first[:chairs.SeedCount])
	}
}

func TestRankUsesConfiguredChairCount(t *testing.T) {
	const chairCount = 512

	snapshot := chairs.Snapshot{Epoch: 1}
	for index := range chairCount {
		snapshot.Chairs = append(snapshot.Chairs, chairs.Chair{
			ID:              chairs.ID(index),
			Holder:          testHolder(ifaces.NodeID(chairs.ID(index).Name())),
			AssignmentEpoch: 1,
		})
	}

	ranked := chairs.Rank(snapshot, digest.MustParse("sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"), chairCount)
	if len(ranked) != chairCount {
		t.Fatalf("ranked chairs = %d, want %d", len(ranked), chairCount)
	}

	foundHighID := false

	for _, chair := range ranked {
		if chair.ID > 255 {
			foundHighID = true
			break
		}
	}

	if !foundHighID {
		t.Fatal("ranking omitted chair IDs above 255")
	}
}

func TestParseNameHasNoArtificialMaximum(t *testing.T) {
	const want chairs.ID = 1_000_000

	got, err := chairs.ParseName(want.Name())
	if err != nil {
		t.Fatalf("ParseName: %v", err)
	}

	if got != want {
		t.Fatalf("ParseName(%q) = %d, want %d", want.Name(), got, want)
	}
}

func TestSnapshotCountsOnlyConfiguredChairs(t *testing.T) {
	snapshot := chairs.Snapshot{
		Epoch: 1,
		Chairs: []chairs.Chair{
			{ID: 0, Holder: testHolder("peer-0"), AssignmentEpoch: 1},
			{ID: 63, Holder: testHolder("peer-63"), AssignmentEpoch: 1},
			{ID: 127, Holder: testHolder("peer-127"), AssignmentEpoch: 1},
		},
	}

	if got := snapshot.OccupiedCountWithin(64); got != 2 {
		t.Fatalf("occupied chairs within 64 = %d, want 2", got)
	}

	if got := snapshot.SelectableCountWithin(64); got != 2 {
		t.Fatalf("selectable chairs within 64 = %d, want 2", got)
	}
}

func TestSnapshotActiveCountExcludesExpiredChairs(t *testing.T) {
	now := time.Unix(1_000, 0)
	snapshot := chairs.Snapshot{
		Epoch: 4,
		Chairs: []chairs.Chair{
			{
				ID:              0,
				Holder:          testHolder("live"),
				AssignmentEpoch: 4,
				RenewTime:       now.Add(-time.Minute),
				LeaseDuration:   5 * time.Minute,
			},
			{
				ID:              1,
				Holder:          testHolder("expired"),
				AssignmentEpoch: 4,
				RenewTime:       now.Add(-5 * time.Minute),
				LeaseDuration:   5 * time.Minute,
			},
		},
	}

	if got := snapshot.SelectableCountWithin(2); got != 2 {
		t.Fatalf("selectable chairs = %d, want 2", got)
	}

	if got := snapshot.ActiveCountWithin(2, now); got != 1 {
		t.Fatalf("active chairs = %d, want 1", got)
	}
}

func TestRankIncludesPreviousEpochDuringRollover(t *testing.T) {
	snapshot := chairs.Snapshot{Epoch: 8}

	for index := range chairs.SeedCount {
		epoch := int64(8)
		if index%2 == 0 {
			epoch = 7
		}

		snapshot.Chairs = append(snapshot.Chairs, chairs.Chair{
			ID:              chairs.ID(index),
			Holder:          testHolder(ifaces.NodeID(chairs.ID(index).Name())),
			AssignmentEpoch: epoch,
		})
	}

	d := digest.MustParse("sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	if got := len(chairs.Rank(snapshot, d, chairs.DefaultCount)); got != chairs.SeedCount {
		t.Fatalf("ranked chairs = %d, want %d across rollover", got, chairs.SeedCount)
	}
}

func TestRankSkipsIncompleteHolderEndpoint(t *testing.T) {
	snapshot := chairs.Snapshot{Epoch: 3}

	for index := range chairs.SeedCount + 1 {
		holder := testHolder(ifaces.NodeID(chairs.ID(index).Name()))
		if index == 0 {
			holder.P2PAddrs = nil
		}

		snapshot.Chairs = append(snapshot.Chairs, chairs.Chair{
			ID: chairs.ID(index), Holder: holder, AssignmentEpoch: 3,
		})
	}

	d := digest.MustParse("sha256:cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd")
	if got := len(chairs.Rank(snapshot, d, chairs.DefaultCount)); got != chairs.SeedCount {
		t.Fatalf("ranked chairs = %d, want %d complete endpoints", got, chairs.SeedCount)
	}
}

func testHolder(peerID ifaces.NodeID) chairs.Holder {
	return chairs.Holder{
		PeerID:       peerID,
		P2PAddrs:     []string{"/ip4/10.0.0.1/tcp/4001/p2p/" + string(peerID)},
		TransferAddr: "10.0.0.1:5001",
	}
}

func TestCurrentEpochUsesUnixOrigin(t *testing.T) {
	period := 6 * time.Hour
	now := time.Unix(0, 13*period.Nanoseconds()+period.Nanoseconds()/2)

	if got := chairs.CurrentEpoch(now, period); got != 13 {
		t.Fatalf("CurrentEpoch = %d, want 13", got)
	}
}
