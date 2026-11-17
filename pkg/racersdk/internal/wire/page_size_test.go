// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bufio"
	"bytes"
	"math"
	"net/http"
	"strconv"
	"testing"
)

func TestPageCountUsesPageSize(t *testing.T) {
	const page = 4 << 10

	for _, tc := range []struct {
		first, end uint64
		want       uint64
	}{
		{0, 0, 0},
		{0, 1, 1},
		{0, page, 1},
		{0, page + 1, 2},
		{page - 1, page + 1, 2},
		{page, 2 * page, 1},
		{page, 2*page + 1, 2},
	} {
		if got := PageCount(tc.first, tc.end, page); got != tc.want {
			t.Errorf("PageCount(%d, %d, %d) = %d; want %d", tc.first, tc.end, page, got, tc.want)
		}
	}

	if got := PageCount(0, 2*page, 0); got != 1 {
		t.Errorf("zero page size did not use the default: %d", got)
	}
}

func TestPageRangeAlignmentUsesPageSize(t *testing.T) {
	const page = 4 << 10

	if got := BootstrapRange(page); got != (Range{Present: true, Last: page - 1}) {
		t.Fatalf("BootstrapRange(%d) = %+v", page, got)
	}

	rangePage, err := ClosedRange(page, 2*page-1)
	if err != nil {
		t.Fatal(err)
	}

	if err := ValidatePageShape(rangePage, page); err != nil {
		t.Fatal(err)
	}

	if err := ValidatePageShape(rangePage, PageSize); err == nil {
		t.Fatal("default page size accepted a smaller page")
	}

	if _, last, err := rangePage.ResolvePage(3*page, page); err != nil || last != 2*page-1 {
		t.Fatalf("ResolvePage = %d, %v", last, err)
	}

	bootstrap := rawRequest("GET", "Range: bytes=0-4095\r\n")
	if _, err := ParseRequestHead(bootstrap, true, page); err != nil {
		t.Fatalf("small bootstrap rejected: %v", err)
	}

	if _, err := ParseRequestHead(bootstrap, true, PageSize); err == nil {
		t.Fatal("default page size accepted a small bootstrap")
	}

	pinned := rawRequest("GET", "Range: bytes=4096-8191\r\nIf-Match: \"v\"\r\n")
	if _, err := ParseRequestHead(pinned, true, page); err != nil {
		t.Fatalf("small pinned page rejected: %v", err)
	}

	if _, err := ParseRequestHead(pinned, true, PageSize); err == nil {
		t.Fatal("default page size accepted a small pinned page")
	}
}

func TestSequenceUsesPageSize(t *testing.T) {
	const page = 4 << 10

	sequence := NewSequence(0, 2*page, page)
	for number, offset := range []uint64{0, page} {
		if err := sequence.Accept(Frame{Kind: PageFrame, Number: offset / page, Offset: offset, Length: page}, "test"); err != nil {
			t.Fatalf("page %d rejected: %v", number, err)
		}
	}

	if err := sequence.Accept(Frame{Kind: CompleteFrame, Number: 2, Offset: 2 * page}, "test"); err != nil {
		t.Fatal(err)
	}

	if err := NewSequence(0, 2*page, page).Accept(Frame{Kind: PageFrame, Number: 1, Offset: page, Length: page}, "test"); err == nil {
		t.Fatal("accepted an out-of-order page")
	}

	if err := NewSequence(0, page, page).Accept(Frame{Kind: PageFrame, Number: 0, Offset: 0, Length: page + 1}, "test"); err == nil {
		t.Fatal("accepted an overlong page")
	}
}

func TestSubscriptionCreditBoundsUsePageSize(t *testing.T) {
	const page = 64 << 10

	request := Request{Operation: OperationHead, Pin: `"v"`}
	head, err := SubscriptionHead(request, SubscriptionOptions{
		Offset: 1, Length: 10, PageCredits: 2, ByteCredits: 2 * page,
	})
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(head)))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := ParseSubscriptionRequest(req, page); err != nil {
		t.Fatalf("valid credits rejected: %v", err)
	}

	for _, tc := range []struct {
		credits uint64
		ok      bool
	}{
		{page - 1, false},
		{page, true},
		{64 * page, true},
		{64*page + 1, false},
		{math.MaxInt64, false},
	} {
		clone := req.Clone(t.Context())
		clone.Header.Set("Racer-Byte-Credits", strconv.FormatUint(tc.credits, 10))

		_, err := ParseSubscriptionRequest(clone, page)
		if tc.ok && err != nil {
			t.Errorf("credits %d rejected: %v", tc.credits, err)
		}

		if !tc.ok && err == nil {
			t.Errorf("credits %d accepted", tc.credits)
		}
	}
}
