// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package streaming

import (
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/httprange"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

func TestProviderFailuresAmortizeCleanupWithoutExtendingSuppression(t *testing.T) {
	d := digest.MustParse("sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	peer := ifaces.Provider{NodeID: "partial", Addr: "partial"}
	requested := httprange.Range{Start: 0, End: 3}
	now := time.Unix(100, 0)
	failures := newProviderFailures()
	failures.now = func() time.Time { return now }
	failures.record(d, requested, peer, &ifaces.ErrNotFound{Digest: d})
	key := providerDigestKey{digest: d, nodeID: peer.NodeID, addr: peer.Addr}
	failures.stale[providerRangeKey{providerDigestKey: key, requested: requested}] = now.Add(-time.Second)
	failures.suspicious[key] = now.Add(-time.Second)

	failures.unavailable[peer.Addr] = now.Add(-time.Second)
	for range 100 {
		if got := failures.filter(d, requested, []ifaces.Provider{peer}, "self"); len(got) != 1 {
			t.Fatalf("expired history still suppressed peer: %v", got)
		}

		failures.record(d, httprange.Range{Start: 4, End: 7}, peer, &ifaces.ErrNotFound{Digest: d})
	}

	if len(failures.stale) != 2 || len(failures.suspicious) != 1 || len(failures.unavailable) != 1 {
		t.Fatal("filter or record swept history before the cleanup interval")
	}

	now = now.Add(providerFailureSweepInterval)

	if got := failures.filter(d, requested, []ifaces.Provider{peer}, "self"); len(got) != 1 {
		t.Fatalf("cleanup suppressed expired peer: %v", got)
	}

	if len(failures.stale) != 1 || len(failures.suspicious) != 0 || len(failures.unavailable) != 0 {
		t.Fatal("periodic sweep did not remove expired entries or removed a live entry")
	}

	if got := failures.filter(d, httprange.Range{Start: 4, End: 7}, []ifaces.Provider{peer}, "self"); len(got) != 0 {
		t.Fatal("periodic sweep removed unexpired suppression")
	}
}
