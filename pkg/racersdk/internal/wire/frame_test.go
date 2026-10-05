// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bytes"
	"testing"
)

func TestPageStreamIntervalBound(t *testing.T) {
	tracking := &Sequence{}
	for n := uint64(0); n < 8192; n += 2 {
		if !tracking.record(n) {
			t.Fatal("interval rejected", n)
		}
	}

	if tracking.record(8192) || tracking.record(0) {
		t.Fatal("unbounded or duplicate interval accepted")
	}

	for n := uint64(1); n < 8192; n += 2 {
		if !tracking.record(n) {
			t.Fatal("merge rejected", n)
		}
	}

	if len(tracking.intervals) != 1 || tracking.intervals[0] != (pageInterval{0, 8192}) {
		t.Fatal("intervals failed to compact")
	}
}

func TestFrameAndCreditBytes(t *testing.T) {
	f := Frame{Kind: PageFrame, Number: 0x0102030405060708, Offset: 0x1112131415161718, Length: 0x21222324}
	want := []byte{1, 1, 2, 3, 4, 5, 6, 7, 8, 17, 18, 19, 20, 21, 22, 23, 24, 33, 34, 35, 36}

	b := f.Encode()
	if !bytes.Equal(b[:], want) || DecodeFrame(b) != f {
		t.Fatal("frame bytes changed")
	}

	var out bytes.Buffer
	if err := WriteFrame(&out, f); err != nil || !bytes.Equal(out.Bytes(), want) {
		t.Fatal("frame write", err)
	}

	c := Credit{Number: f.Number, Length: f.Length}

	release := c.Encode()
	if !bytes.Equal(release[:], append(want[1:9:9], want[17:]...)) || DecodeCredit(release) != c {
		t.Fatal("credit bytes changed")
	}
}

func TestSequence(t *testing.T) {
	first, end := uint64(PageSize-2), uint64(PageSize+3)
	a := Frame{Kind: PageFrame, Number: 0, Offset: first, Length: 2}
	b := Frame{Kind: PageFrame, Number: 1, Offset: PageSize, Length: 3}
	complete := Frame{Kind: CompleteFrame, Number: 2, Offset: 5}

	for _, ordered := range []bool{false, true} {
		s := NewSequence(first, end, ordered)

		frames := []Frame{a, b, complete}
		if !ordered {
			frames[0], frames[1] = b, a
		}

		for _, f := range frames {
			if err := s.Accept(f, "subscription frame"); err != nil {
				t.Fatal(err)
			}
		}

		if s.Accept(complete, "subscription frame") == nil {
			t.Fatal("accepted frame after completion")
		}
	}

	for _, f := range []Frame{b, complete, {Kind: 3}, {Kind: PageFrame, Offset: first, Length: 1}, {Kind: PageFrame, Offset: first + 1, Length: 2}, {Kind: PageFrame, Number: 1, Offset: first, Length: 2}} {
		if NewSequence(first, end, true).Accept(f, "subscription frame") == nil {
			t.Fatal("accepted malformed frame", f)
		}
	}

	s := NewSequence(first, end, false)
	if s.Accept(a, "subscription frame") != nil || s.Accept(a, "subscription frame") == nil {
		t.Fatal("duplicate page")
	}

	if err := NewSequence(0, 0, true).Accept(Frame{Kind: CompleteFrame}, "subscription complete"); err != nil {
		t.Fatal("empty completion", err)
	}

	for _, f := range []Frame{{Kind: CompleteFrame, Number: 1}, {Kind: CompleteFrame, Offset: 1}, {Kind: CompleteFrame, Length: 1}} {
		if NewSequence(0, 0, true).Accept(f, "subscription complete") == nil {
			t.Fatal("invalid empty completion")
		}
	}
}
