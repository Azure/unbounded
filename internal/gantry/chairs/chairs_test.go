// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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

	for index := range chairs.Count {
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
	first := chairs.Rank(snapshot, d)
	second := chairs.Rank(snapshot, d)

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
	for index := range chairs.Count {
		snapshot.Chairs = append(snapshot.Chairs, chairs.Chair{
			ID:     chairs.ID(index),
			Holder: testHolder(ifaces.NodeID(chairs.ID(index).Name())),
		})
	}

	first := chairs.Rank(snapshot, digest.MustParse("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	second := chairs.Rank(snapshot, digest.MustParse("sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"))

	if reflect.DeepEqual(first[:chairs.SeedCount], second[:chairs.SeedCount]) {
		t.Fatalf("top seed chairs unexpectedly identical: %v", first[:chairs.SeedCount])
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
	if got := len(chairs.Rank(snapshot, d)); got != chairs.SeedCount {
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
	if got := len(chairs.Rank(snapshot, d)); got != chairs.SeedCount {
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
