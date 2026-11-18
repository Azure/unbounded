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

func TestCoverageUnionsRangesAndIngestPrefix(t *testing.T) {
	cache := newTestCache(t, 0)

	d := testDigest(t, "coverage")
	for _, write := range []struct {
		start int64
		bytes string
	}{{10, "abcdefghij"}, {15, "abcdefghij"}, {40, "abcde"}, {10, "abcdefghij"}} {
		if _, err := cache.Put(d, 100, write.start, bytes.NewBufferString(write.bytes)); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct{ prefix, want int64 }{{0, 20}, {15, 30}, {45, 45}} {
		got, total := cache.Coverage(d, tc.prefix)
		if got != tc.want || total != 100 {
			t.Errorf("Coverage(prefix=%d) = %d/%d, want %d/100", tc.prefix, got, total, tc.want)
		}
	}

	if got, total := cache.Coverage(testDigest(t, "absent"), 8); got != 8 || total != -1 {
		t.Fatalf("missing range cache should preserve ingest prefix, got %d/%d", got, total)
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

	reader := bytes.NewReader([]byte("xxxx"))

	n, err := c.Put(second, 64, 0, reader)
	if !errors.Is(err, ErrFull) {
		t.Fatalf("Put second: %v, want ErrFull", err)
	}

	if n != 0 {
		t.Errorf("stored %d bytes past capacity, want 0", n)
	}

	if !c.Full() {
		t.Error("Full() = false after hitting the cap")
	}

	if reader.Len() != 4 {
		t.Error("full cache consumed the rejected reader")
	}

	if c.HasAny(second) {
		t.Error("full cache admitted a new digest")
	}

	// The already-admitted blob keeps working.
	if !c.Has(first, mustRange(t, 0, 7)) {
		t.Error("existing entry lost after the cache filled")
	}

	if n, err := c.Put(first, 64, 8, bytes.NewReader([]byte("more"))); err != nil || n != 4 {
		t.Fatalf("Put existing: (%d, %v), want (4, nil)", n, err)
	}

	if !c.Has(first, mustRange(t, 0, 11)) {
		t.Error("existing entry did not accept another range after the cache filled")
	}
}

type failingReader struct{ err error }

func (r failingReader) Read(p []byte) (int, error) {
	return copy(p, "ab"), r.err
}

func TestPutPreservesReaderFailure(t *testing.T) {
	c := newTestCache(t, 0)
	d := testDigest(t, "layer")
	readErr := errors.New("remote read failed")

	n, err := c.Put(d, 64, 10, failingReader{err: readErr})
	if n != 2 || !errors.Is(err, readErr) || errors.Is(err, ErrFull) {
		t.Fatalf("Put = (%d, %v), want (2, remote read failed)", n, err)
	}

	if !c.Has(d, mustRange(t, 10, 11)) || c.Has(d, mustRange(t, 10, 13)) {
		t.Error("failed read did not preserve exactly the bytes received")
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
