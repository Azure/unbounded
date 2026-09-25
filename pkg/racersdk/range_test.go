// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"math"
	"testing"
)

func TestRangeResolution(t *testing.T) {
	for _, tt := range []struct {
		wire        string
		size        ByteLength
		first, last ByteOffset
		kind        ErrorKind
	}{
		{"bytes=0-0", 1, 0, 0, 0},
		{"bytes=0-99", 2, 0, 1, 0},
		{"bytes=1-", 2, 1, 1, 0},
		{"bytes=-99", 2, 0, 1, 0},
		{"bytes=-1", 2, 1, 1, 0},
		{"bytes=-0", 2, 0, 0, ErrorUnsatisfiableRange},
		{"bytes=0-0", 0, 0, 0, ErrorUnsatisfiableRange},
		{"bytes=2-", 2, 0, 0, ErrorUnsatisfiableRange},
		{"bytes=0-9223372036854775807", math.MaxInt64, 0, math.MaxInt64 - 1, 0},
		{"bytes=9223372036854775807-", math.MaxInt64, 0, 0, ErrorUnsatisfiableRange},
		{"bytes=-9223372036854775807", math.MaxInt64, 0, math.MaxInt64 - 1, 0},
		{"bytes=0-0", math.MaxInt64 + 1, 0, 0, ErrorInvalidArgument},
	} {
		r, err := parseRange(tt.wire)
		if err != nil {
			t.Fatal(err)
		}

		if rangeValue(r) != tt.wire {
			t.Fatal("range normalized")
		}

		first, last, err := r.Resolve(tt.size)
		if tt.kind != 0 {
			assertKind(t, err, tt.kind)
		} else if err != nil || first != tt.first || last != tt.last {
			t.Fatalf("%s/%d: %d-%d %v", tt.wire, tt.size, first, last, err)
		}

		if r.Kind() == RangeClosed {
			if _, ok := r.First(); !ok {
				t.Fatal("first absent")
			}

			if _, ok := r.Last(); !ok {
				t.Fatal("last absent")
			}
		}

		if r.Kind() == RangeSuffix {
			if _, ok := r.SuffixLength(); !ok {
				t.Fatal("suffix absent")
			}
		}
	}

	for _, s := range []string{"", "bytes=-", "bytes=1-0", "bytes=00-1", "bytes=0-01", "bytes=+1-", "bytes=0-1,2-3", "bytes=0- 1", "bytes=0-9223372036854775808", "Bytes=0-1"} {
		_, err := parseRange(s)
		assertKind(t, err, ErrorInvalidArgument)
	}

	_, err := FromRange(math.MaxInt64 + 1)
	assertKind(t, err, ErrorInvalidArgument)
	_, err = SuffixRange(math.MaxInt64 + 1)
	assertKind(t, err, ErrorInvalidArgument)
	_, err = ClosedRange(2, 1)
	assertKind(t, err, ErrorInvalidArgument)
}

func TestWholePages(t *testing.T) {
	p := ByteOffset(PageSize)
	for _, size := range []ByteLength{1, PageSize - 1, PageSize, PageSize + 1, math.MaxInt64} {
		start := (uint64(size) - 1) / uint64(PageSize) * uint64(PageSize)
		for _, end := range []uint64{uint64(size) - 1, nominalPageEnd(start)} {
			r, err := ClosedRange(ByteOffset(start), ByteOffset(end))
			if err != nil {
				t.Fatal(err)
			}

			first, last, err := resolveOriginRange(r, size)
			if err != nil || uint64(first) != start || last != ByteOffset(size)-1 {
				t.Fatalf("page %d: %d-%d %v", size, first, last, err)
			}
		}
	}

	for _, tt := range []struct {
		first, last ByteOffset
		size        ByteLength
		kind        ErrorKind
	}{
		{1, p - 1, PageSize, ErrorInvalidArgument}, {0, p, PageSize + 1, ErrorInvalidArgument}, {0, p - 2, PageSize, ErrorInvalidArgument}, {p, 2*p - 1, PageSize, ErrorUnsatisfiableRange}, {0, p - 1, 0, ErrorUnsatisfiableRange},
	} {
		r, err := ClosedRange(tt.first, tt.last)
		if err != nil {
			t.Fatal(err)
		}

		_, _, err = resolveOriginRange(r, tt.size)
		assertKind(t, err, tt.kind)
	}
}

func FuzzRange(f *testing.F) {
	for _, s := range []string{"bytes=0-0", "bytes=-0", "bytes=16777216-", "bytes=0-9223372036854775807", "bytes=9223372036854775807-9223372036854775807"} {
		f.Add(s, uint64(math.MaxInt64))
	}

	f.Fuzz(func(t *testing.T, s string, n uint64) {
		r, err := parseRange(s)
		if err != nil {
			return
		}

		if rangeValue(r) != s {
			t.Fatal("range round trip")
		}

		first, last, err := r.Resolve(ByteLength(n))
		if err == nil && (first > last || uint64(last) >= n || uint64(last-first)+1 > math.MaxInt64) {
			t.Fatal("invalid resolved bounds")
		}
	})
}
