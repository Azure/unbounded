// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"testing"
	"time"
)

func TestUntilStart(t *testing.T) {
	now := time.Unix(1000, 0)

	for name, tc := range map[string]struct {
		startAt int64
		want    time.Duration
	}{
		"unset":  {0, 0},
		"past":   {900, 0},
		"now":    {1000, 0},
		"future": {1030, 30 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			if got := untilStart(tc.startAt, now); got != tc.want {
				t.Fatalf("untilStart(%d) = %v, want %v", tc.startAt, got, tc.want)
			}
		})
	}
}

func TestParseRejectsNegativeStartAt(t *testing.T) {
	if _, err := parseOptions([]string{"--start-at=-1"}, io.Discard); err == nil {
		t.Fatal("expected an error for a negative start-at")
	}
}

func TestTrustedBlobDigestMatchesHash(t *testing.T) {
	ctx := t.Context()

	hashed, err := newBlobCatalogWithWorkers(ctx, "benchmark/image", "seed", 2, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}

	want := hashed.batches[0].blobs[0].descriptor.Digest

	trusted, err := newBlobCatalogWithWorkers(ctx, "benchmark/image", "seed", 2, 1<<20, 1, want)
	if err != nil {
		t.Fatal(err)
	}

	if got := trusted.batches[0].blobs[0].descriptor.Digest; got != want {
		t.Fatalf("trusted digest = %s, want %s", got, want)
	}

	if _, err := newBlobCatalogWithWorkers(ctx, "benchmark/image", "seed", 1, 1<<20, 1, "bogus"); err == nil {
		t.Fatal("expected an invalid trusted digest to fail")
	}
}

func TestSplitDigests(t *testing.T) {
	if got := splitDigests(""); len(got) != 0 {
		t.Fatalf("empty list = %v", got)
	}

	if got := splitDigests(" a , ,b"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("split = %v", got)
	}
}
