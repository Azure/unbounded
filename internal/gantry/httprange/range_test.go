// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package httprange_test

import (
	"errors"
	"math"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/httprange"
)

func TestParseExact(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  httprange.Range
	}{
		{name: "one byte", value: "bytes=0-0", want: httprange.Range{Start: 0, End: 0}},
		{name: "bounded", value: "bytes=456-990", want: httprange.Range{Start: 456, End: 990}},
		{name: "outer whitespace", value: " bytes=1-2 ", want: httprange.Range{Start: 1, End: 2}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := httprange.ParseExact(test.value)
			if err != nil {
				t.Fatal(err)
			}

			if got != test.want {
				t.Fatalf("range = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestParseExactRejectsUnsupportedRanges(t *testing.T) {
	t.Parallel()

	for _, value := range []string{
		"",
		"items=0-1",
		"bytes=-10",
		"bytes=10-",
		"bytes=0-1,4-5",
		"bytes=-1-2",
		"bytes=2-1",
		"bytes=0-9223372036854775807",
		"bytes=abc-2",
	} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()

			if _, err := httprange.ParseExact(value); !errors.Is(err, httprange.ErrInvalid) {
				t.Fatalf("error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestRangeValidateSize(t *testing.T) {
	t.Parallel()

	r := httprange.Range{Start: 9, End: 9}
	if err := r.ValidateSize(10); err != nil {
		t.Fatal(err)
	}

	if err := r.ValidateSize(9); !errors.Is(err, httprange.ErrUnsatisfiable) {
		t.Fatalf("error = %v, want ErrUnsatisfiable", err)
	}
}

func TestValidateResponse(t *testing.T) {
	t.Parallel()

	r := httprange.Range{Start: 10, End: 19}

	size, err := httprange.ValidateResponse(r, "bytes 10-19/100", 10)
	if err != nil {
		t.Fatal(err)
	}

	if size != 100 {
		t.Fatalf("size = %d, want 100", size)
	}

	for _, test := range []struct {
		name          string
		contentRange  string
		contentLength int64
	}{
		{name: "wrong start", contentRange: "bytes 11-19/100", contentLength: 9},
		{name: "wrong end", contentRange: "bytes 10-20/100", contentLength: 11},
		{name: "unknown size", contentRange: "bytes 10-19/*", contentLength: 10},
		{name: "range beyond size", contentRange: "bytes 10-19/19", contentLength: 10},
		{name: "wrong length", contentRange: "bytes 10-19/100", contentLength: 9},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if _, err := httprange.ValidateResponse(r, test.contentRange, test.contentLength); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestNewRejectsOverflowLength(t *testing.T) {
	t.Parallel()

	if _, err := httprange.New(0, math.MaxInt64); !errors.Is(err, httprange.ErrInvalid) {
		t.Fatalf("error = %v, want ErrInvalid", err)
	}
}
