// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

func TestProviderFailureSweep_EvictsExpiredEntriesWhenProvidersFiltered(t *testing.T) {
	now := time.Now()
	d1 := digest.MustParse("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	d2 := digest.MustParse("sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	expiredKey := providerDigestKey{digest: d1, nodeID: "expired", addr: "expired:5001"}
	freshKey := providerDigestKey{digest: d1, nodeID: "fresh", addr: "fresh:5001"}

	s := &Server{
		staleProviders: map[providerDigestKey]time.Time{
			expiredKey: now.Add(-time.Second),
			freshKey:   now.Add(time.Minute),
		},
		suspiciousProviders: map[providerDigestKey]time.Time{
			expiredKey: now.Add(-time.Second),
			freshKey:   now.Add(time.Minute),
		},
		unavailableProviders: map[string]time.Time{
			"expired:5001": now.Add(-time.Second),
			"fresh:5001":   now.Add(time.Minute),
		},
	}

	providers, summary := s.filterProvidersForDigest(d2, []ifaces.Provider{{NodeID: "other", Addr: "other:5001"}})
	if len(providers) != 1 || providers[0].Addr != "other:5001" {
		t.Fatalf("providers = %+v, want unrelated provider preserved", providers)
	}

	if summary != (peerAttemptSummary{}) {
		t.Fatalf("summary = %+v, want zero", summary)
	}

	if _, ok := s.staleProviders[expiredKey]; ok {
		t.Fatal("expired stale provider entry still present")
	}

	if _, ok := s.suspiciousProviders[expiredKey]; ok {
		t.Fatal("expired suspicious provider entry still present")
	}

	if _, ok := s.unavailableProviders["expired:5001"]; ok {
		t.Fatal("expired unavailable provider entry still present")
	}

	if _, ok := s.staleProviders[freshKey]; !ok {
		t.Fatal("fresh stale provider entry was removed")
	}

	if _, ok := s.suspiciousProviders[freshKey]; !ok {
		t.Fatal("fresh suspicious provider entry was removed")
	}

	if _, ok := s.unavailableProviders["fresh:5001"]; !ok {
		t.Fatal("fresh unavailable provider entry was removed")
	}
}
