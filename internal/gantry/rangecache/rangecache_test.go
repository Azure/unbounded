// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package rangecache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/httprange"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

func testDigest(t *testing.T, s string) digest.Digest {
	t.Helper()

	sum := sha256.Sum256([]byte(s))

	return digest.MustParse("sha256:" + hex.EncodeToString(sum[:]))
}

func mustRange(t *testing.T, start, end int64) httprange.Range {
	t.Helper()

	r, err := httprange.New(start, end)
	if err != nil {
		t.Fatalf("httprange.New(%d,%d): %v", start, end, err)
	}

	return r
}

func newTestCache(t *testing.T, maxBytes int64) *Cache {
	t.Helper()

	c, err := New(t.TempDir()+"/ranges", maxBytes)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(func() { _ = c.Close() })

	return c
}

func TestPutThenOpenRangeReturnsExactBytes(t *testing.T) {
	c := newTestCache(t, 0)
	d := testDigest(t, "layer")

	// A tail read, which is where OverlayBD keeps its index.
	if _, err := c.Put(d, 4096, 1000, bytes.NewReader([]byte("INDEXDATA"))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	body, total, err := c.OpenRange(context.Background(), d, mustRange(t, 1002, 1006))
	if err != nil {
		t.Fatalf("OpenRange: %v", err)
	}

	defer func() { _ = body.Close() }() //nolint:errcheck // test cleanup

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	if string(got) != "DEXDA" {
		t.Errorf("body = %q, want DEXDA", got)
	}

	if total != 4096 {
		t.Errorf("total = %d, want 4096", total)
	}
}

func TestOpenRangeMissesWhenOnlyPartiallyCovered(t *testing.T) {
	c := newTestCache(t, 0)
	d := testDigest(t, "layer")

	if _, err := c.Put(d, 100, 10, bytes.NewReader([]byte("abcde"))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// 10-14 is cached; 12-20 runs past the end and must not yield a short body.
	_, _, err := c.OpenRange(context.Background(), d, mustRange(t, 12, 20))

	var notFound *ifaces.ErrNotFound
	if !errors.As(err, &notFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}

	if c.Has(d, mustRange(t, 12, 20)) {
		t.Error("Has reported a partially covered range")
	}
}

func TestAdjacentPutsMergeIntoOneSpan(t *testing.T) {
	c := newTestCache(t, 0)
	d := testDigest(t, "layer")

	if _, err := c.Put(d, 100, 0, bytes.NewReader([]byte("aaaa"))); err != nil {
		t.Fatalf("Put low: %v", err)
	}

	if _, err := c.Put(d, 100, 4, bytes.NewReader([]byte("bbbb"))); err != nil {
		t.Fatalf("Put high: %v", err)
	}

	if !c.Has(d, mustRange(t, 1, 6)) {
		t.Fatal("adjacent writes did not merge into a coverable span")
	}

	body, _, err := c.OpenRange(context.Background(), d, mustRange(t, 2, 5))
	if err != nil {
		t.Fatalf("OpenRange: %v", err)
	}

	defer func() { _ = body.Close() }() //nolint:errcheck // test cleanup

	got, _ := io.ReadAll(body) //nolint:errcheck // body is a section reader
	if string(got) != "aabb" {
		t.Errorf("body = %q, want aabb", got)
	}
}

func TestCacheStopsAdmittingNewBlobsAtCapacity(t *testing.T) {
	c := newTestCache(t, 8)
	first := testDigest(t, "first")
	second := testDigest(t, "second")

	if _, err := c.Put(first, 64, 0, bytes.NewReader([]byte("12345678"))); err != nil {
		t.Fatalf("Put first: %v", err)
	}

	n, err := c.Put(second, 64, 0, bytes.NewReader([]byte("xxxx")))
	if err != nil {
		t.Fatalf("Put second: %v", err)
	}

	if n != 0 {
		t.Errorf("stored %d bytes past capacity, want 0", n)
	}

	if !c.Full() {
		t.Error("Full() = false after hitting the cap")
	}

	// The already-admitted blob keeps working.
	if !c.Has(first, mustRange(t, 0, 7)) {
		t.Error("existing entry lost after the cache filled")
	}
}

func TestSpansInsertOverlapping(t *testing.T) {
	var s spans

	s = s.insert(10, 20)
	s = s.insert(5, 12)
	s = s.insert(30, 40)
	s = s.insert(21, 29)

	if !s.covers(5, 40) {
		t.Fatalf("spans did not merge into a single run: %+v", s)
	}

	if len(s) != 1 {
		t.Errorf("len(spans) = %d, want 1: %+v", len(s), s)
	}
}
