// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/rangecache"
)

type stubInventory struct {
	digests []digest.Digest
	err     error
}

func (s stubInventory) Inventory(context.Context) ([]digest.Digest, error) {
	return s.digests, s.err
}

func (s stubInventory) Open(_ context.Context, d digest.Digest) (io.ReadCloser, int64, error) {
	for _, have := range s.digests {
		if have == d {
			return io.NopCloser(bytes.NewReader([]byte("x"))), 1, nil
		}
	}

	return nil, 0, &ifaces.ErrNotFound{Digest: d}
}

func advDigest(t *testing.T, s string) digest.Digest {
	t.Helper()

	sum := sha256.Sum256([]byte(s))

	return digest.MustParse("sha256:" + hex.EncodeToString(sum[:]))
}

func TestRangeAwareInventoryAdvertisesPartiallyHeldBlobs(t *testing.T) {
	complete := advDigest(t, "complete")
	partial := advDigest(t, "partial")

	cache, err := rangecache.New(t.TempDir()+"/ranges", 0)
	if err != nil {
		t.Fatalf("rangecache.New: %v", err)
	}

	t.Cleanup(func() { _ = cache.Close() })

	if _, err := cache.Put(partial, 1<<30, 0, bytes.NewReader([]byte("idx"))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	base := stubInventory{digests: []digest.Digest{complete}}
	inv := rangeAwareInventory{inv: base, open: base, cache: cache}

	// Open is how the advertiser proves a digest is serveable before
	// announcing it; the content store alone would reject the partial blob.
	if _, _, err := inv.Open(context.Background(), partial); err != nil {
		t.Errorf("Open(partial) = %v, want nil so the digest can be announced", err)
	}

	if _, _, err := inv.Open(context.Background(), advDigest(t, "absent")); err == nil {
		t.Error("Open(absent) succeeded; only cached or complete blobs are serveable")
	}

	// Reconcile withdraws anything missing from the inventory, so the partial
	// blob has to appear here or its announcement is revoked a minute later.
	listed, err := inv.Inventory(context.Background())
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}

	found := map[digest.Digest]bool{}
	for _, d := range listed {
		found[d] = true
	}

	if !found[complete] || !found[partial] {
		t.Errorf("Inventory = %v, want both the complete and partially cached digests", listed)
	}
}

func TestRangeAwareInventoryPropagatesBackendFailure(t *testing.T) {
	cache, err := rangecache.New(t.TempDir()+"/ranges", 0)
	if err != nil {
		t.Fatalf("rangecache.New: %v", err)
	}

	t.Cleanup(func() { _ = cache.Close() })

	want := &ifaces.ErrUnavailable{}
	inv := rangeAwareInventory{inv: stubInventory{err: want}, cache: cache}

	// The advertiser pauses rather than withdrawing everything when the
	// backend is down, so this error must not be swallowed.
	if _, err := inv.Inventory(context.Background()); err == nil {
		t.Fatal("Inventory returned nil error; reconcile would withdraw the whole announced set")
	}
}
