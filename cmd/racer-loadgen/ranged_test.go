// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
)

// fakeRange writes length bytes of a pattern starting at offset, or fails
// after failAfter bytes for the first few calls that cover failAt.
type fakeRange struct {
	data   []byte
	off    int64
	length int64
	fail   int64
}

func (f fakeRange) WriteTo(w io.Writer) (int64, error) {
	n := f.length
	if f.fail >= 0 && f.fail < n {
		n = f.fail
	}

	written, err := w.Write(f.data[f.off : f.off+n])
	if err != nil {
		return int64(written), err
	}

	if n < f.length {
		return int64(written), io.ErrUnexpectedEOF
	}

	return int64(written), nil
}

type positionalSink struct {
	mu  sync.Mutex
	got []byte
}

// Write records bytes; positions come from the getter, so tests track coverage there.
func (s *positionalSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.got = append(s.got, p...)

	return len(p), nil
}

func TestRangedBodyCoversEveryByteAndResumes(t *testing.T) {
	data := make([]byte, 1000)
	for i := range data {
		data[i] = byte(i)
	}

	var (
		mu       sync.Mutex
		covered  = make([]int, len(data))
		failures = 3
	)

	get := func(_ context.Context, offset, length int64) (io.WriterTo, func() error, error) {
		mu.Lock()
		defer mu.Unlock()

		fail := int64(-1)

		if failures > 0 && length > 10 {
			failures--
			fail = length / 2
		}

		end := offset + length
		if fail >= 0 {
			end = offset + fail
		}

		for i := offset; i < end; i++ {
			covered[i]++
		}

		return fakeRange{data: data, off: offset, length: length, fail: fail}, func() error { return nil }, nil
	}

	body := &rangedBody{ctx: t.Context(), get: get, size: int64(len(data)), rangeBytes: 64, parallel: 4, retries: 5}
	sink := &positionalSink{}

	n, err := io.Copy(sink, body)
	if err != nil {
		t.Fatal(err)
	}

	if n != int64(len(data)) || len(sink.got) != len(data) {
		t.Fatalf("copied %d bytes, sink %d, want %d", n, len(sink.got), len(data))
	}

	for i, c := range covered {
		if c != 1 {
			t.Fatalf("byte %d delivered %d times", i, c)
		}
	}

	if body.resumes.Load() != 3 {
		t.Fatalf("resumes = %d, want 3", body.resumes.Load())
	}
}

func TestRangedBodyGivesUpAfterRetries(t *testing.T) {
	boom := errors.New("boom")
	get := func(context.Context, int64, int64) (io.WriterTo, func() error, error) { return nil, nil, boom }
	body := &rangedBody{ctx: t.Context(), get: get, size: 100, rangeBytes: 10, parallel: 2, retries: 1}

	if _, err := body.WriteTo(io.Discard); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}

	if _, err := body.Read(make([]byte, 1)); err == nil {
		t.Fatal("Read must fail")
	}
}

func TestParseRangeFlags(t *testing.T) {
	if _, err := parseOptions([]string{"--backend=uds", "--cache=c", "--catalog-blobs=1", "--range-bytes=1024", "--verify=true"}, io.Discard); err == nil {
		t.Fatal("range-bytes with verify must fail")
	}

	if _, err := parseOptions([]string{"--range-parallel=-1"}, io.Discard); err == nil {
		t.Fatal("range-parallel=-1 must fail")
	}

	if _, err := parseOptions([]string{"--backend=uds", "--cache=c", "--catalog-blobs=1", "--range-bytes=1024", "--range-parallel=4"}, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestRangedBodyRotationStillCoversEveryRange(t *testing.T) {
	var (
		mu     sync.Mutex
		starts []int64
	)

	get := func(_ context.Context, offset, length int64) (io.WriterTo, func() error, error) {
		mu.Lock()

		starts = append(starts, offset)
		mu.Unlock()

		return fakeRange{data: make([]byte, 105), off: offset, length: length, fail: -1}, func() error { return nil }, nil
	}

	body := &rangedBody{ctx: t.Context(), get: get, size: 105, rangeBytes: 10, parallel: 1, rotate: 7}

	n, err := body.WriteTo(io.Discard)
	if err != nil || n != 105 {
		t.Fatalf("n=%d err=%v", n, err)
	}

	if len(starts) != 11 || starts[0] != 70 || starts[3] != 100 || starts[4] != 0 {
		t.Fatalf("starts = %v", starts)
	}
}

func TestTrailingNumber(t *testing.T) {
	for in, want := range map[string]int64{"node-0017": 17, "w9": 9} {
		if got, ok := trailingNumber(in); !ok || got != want {
			t.Fatalf("trailingNumber(%q) = %d, %v", in, got, ok)
		}
	}

	for _, in := range []string{"", "node", "n1234567890123456789"} {
		if _, ok := trailingNumber(in); ok {
			t.Fatalf("trailingNumber(%q) should fail", in)
		}
	}
}

func TestRangeCohortRotation(t *testing.T) {
	p := &puller{opts: pullOptions{RangeBytes: 10, RangeCohorts: 4, NodeName: "w0006"}}

	// 100 ranges, cohort 6 % 4 = 2, so start at range 50.
	if got := p.rangeRotation(1000); got != 50 {
		t.Fatalf("rotation = %d, want 50", got)
	}
}
