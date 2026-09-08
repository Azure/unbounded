// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package coldstart_test

import (
	"context"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/chairs"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

// TestChairResolverWaitsOutHealthyLongPull covers a seed cohort that keeps
// reporting work in flight for longer than the patience limit.
//
// Production mirror calls pass size zero, which yields short blob poll windows,
// so a large layer or a queued job routinely outlives several of them.
// Escalating there recruits another full cohort against work that is already
// running, and each extra cohort adds origin fetchers and lengthens the
// backlog that caused the delay.
func TestChairResolverWaitsOutHealthyLongPull(t *testing.T) {
	d := digest.MustParse("sha256:" + repeatHex('d'))
	snapshot := fullChairSnapshot(5)
	ranked := chairs.Rank(snapshot, d)

	seeds := make(map[uint32]struct{}, chairs.SeedCount)
	for _, chair := range ranked[:chairs.SeedCount] {
		seeds[uint32(chair.ID)] = struct{}{}
	}

	// Every seed reports that the pull is already under way, forever.
	coord := &chairCoordStub{outcomes: map[uint32]ifaces.PleasePullOutcome{}}
	for id := range seeds {
		coord.outcomes[id] = ifaces.PleasePullOutcome{Outcome: ifaces.PleasePullAlreadyPulling}
	}

	// The DHT never publishes, so the resolver keeps polling until the caller
	// gives up. That is the correct outcome: waiting beats duplicating.
	disco := &stubDisco{}

	resolver := newTestChairResolver(&chairSnapshotStub{snapshot: snapshot}, coord, disco)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := resolver.Resolve(ctx, d, ifaces.KindBlob, "registry.example.com", "repo/image", 0)
	if err == nil {
		t.Fatal("Resolve returned a provider although the DHT never published one")
	}

	coord.mu.Lock()
	defer coord.mu.Unlock()

	// Only the seed cohort may ever have been asked. Any chair outside it means
	// healthy in-flight work was mistaken for a stall and duplicated.
	for _, call := range coord.calls {
		if _, ok := seeds[call.ChairID]; !ok {
			t.Fatalf("chair %d outside the seed cohort was recruited while seeds reported work in flight", call.ChairID)
		}
	}
}

func repeatHex(c byte) string {
	out := make([]byte, 64)
	for i := range out {
		out[i] = c
	}

	return string(out)
}
