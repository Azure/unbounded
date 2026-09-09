// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
)

type benchmarkCommitSource struct {
	inventory []digest.Digest
	present   map[string]struct{}
	checks    int64
}

func (s *benchmarkCommitSource) Inventory(_ context.Context) ([]digest.Digest, error) {
	s.checks += int64(len(s.inventory))

	out := make([]digest.Digest, len(s.inventory))
	copy(out, s.inventory)

	return out, nil
}

func (s *benchmarkCommitSource) Openable(_ context.Context, d digest.Digest) (bool, error) {
	s.checks++
	_, ok := s.present[d.String()]

	return ok, nil
}

func BenchmarkStreamCommitTrackerProbe(b *testing.B) {
	for _, tc := range []struct {
		totalInventory int
		uniquePending  int
		duplicates     int
	}{
		{totalInventory: 1_000, uniquePending: 1, duplicates: 1},
		{totalInventory: 100_000, uniquePending: 1, duplicates: 1},
		{totalInventory: 500_000, uniquePending: 1, duplicates: 1},
		{totalInventory: 500_000, uniquePending: 100, duplicates: 1},
		{totalInventory: 500_000, uniquePending: 1_000, duplicates: 1},
		{totalInventory: 500_000, uniquePending: 100, duplicates: 10},
	} {
		name := fmt.Sprintf("inventory=%d/pending=%d/duplicates=%d", tc.totalInventory, tc.uniquePending, tc.duplicates)
		b.Run(name, func(b *testing.B) {
			inventory := make([]digest.Digest, tc.totalInventory)
			for i := range inventory {
				inventory[i] = trackerDigestOf([]byte(fmt.Sprintf("inventory-%d", i)))
			}

			pending := make([]digest.Digest, tc.uniquePending)
			for i := range pending {
				pending[i] = trackerDigestOf([]byte(fmt.Sprintf("pending-%d", i)))
			}

			source := &benchmarkCommitSource{
				inventory: inventory,
				present:   map[string]struct{}{},
			}
			tracker := newStreamCommitTracker(source, nil, nil, nil, nil)
			deadline := time.Now().Add(time.Hour)

			for _, d := range pending {
				commits := make([]pendingStreamCommit, tc.duplicates)
				for i := range commits {
					commits[i] = pendingStreamCommit{deadline: deadline}
				}

				tracker.pending[d.String()] = commits
			}

			b.ReportAllocs()
			b.ResetTimer()

			for range b.N {
				tracker.probe(context.Background())
			}

			b.StopTimer()

			b.ReportMetric(float64(source.checks)/float64(b.N), "store_entries/op")
		})
	}
}
