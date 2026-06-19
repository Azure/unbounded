// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package coldstart

import (
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
)

func TestHonorWindowSweep_EvictsExpiredEntriesWhenDifferentDigestTouched(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	r := &Resolver{
		opts: Options{
			Now:                  func() time.Time { return now },
			TransientCooldownCap: 30 * time.Second,
			HonorSweepInterval:   time.Second,
		},
		honorUntil: make(map[digest.Digest]time.Time),
	}

	d1 := digest.MustParse("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	d2 := digest.MustParse("sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	r.recordHonorWindow(d1, now.Add(10*time.Second))

	if got := len(r.honorUntil); got != 1 {
		t.Fatalf("len after first record = %d, want 1", got)
	}

	now = now.Add(11 * time.Second)
	r.recordHonorWindow(d2, now.Add(20*time.Second))

	if _, ok := r.honorUntil[d1]; ok {
		t.Fatal("expired honor window still present after unrelated mutation")
	}

	if _, ok := r.honorUntil[d2]; !ok {
		t.Fatal("new honor window missing after sweep")
	}

	if got := len(r.honorUntil); got != 1 {
		t.Fatalf("len after sweep+new record = %d, want 1", got)
	}
}
