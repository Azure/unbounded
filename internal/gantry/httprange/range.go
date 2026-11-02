// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package httprange parses and validates exact HTTP byte ranges.
package httprange

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

var (
	ErrInvalid       = errors.New("invalid byte range")
	ErrUnsatisfiable = errors.New("unsatisfiable byte range")
)

// Range is an exact inclusive byte range.
type Range struct {
	Start int64
	End   int64
}

// New constructs a validated exact range.
func New(start, end int64) (Range, error) {
	if start < 0 || end < start || end-start == math.MaxInt64 {
		return Range{}, fmt.Errorf("%w: %d-%d", ErrInvalid, start, end)
	}

	return Range{Start: start, End: end}, nil
}

// ParseExact parses a single bytes=N-M range. Open-ended, suffix, and
// multipart ranges are deliberately rejected.
func ParseExact(value string) (Range, error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "bytes=") {
		return Range{}, fmt.Errorf("%w: expected bytes=N-M", ErrInvalid)
	}

	spec := strings.TrimPrefix(value, "bytes=")
	if strings.Contains(spec, ",") {
		return Range{}, fmt.Errorf("%w: multipart ranges are unsupported", ErrInvalid)
	}

	startText, endText, ok := strings.Cut(spec, "-")
	if !ok || startText == "" || endText == "" || strings.Contains(endText, "-") {
		return Range{}, fmt.Errorf("%w: expected bytes=N-M", ErrInvalid)
	}

	start, err := strconv.ParseInt(startText, 10, 64)
	if err != nil {
		return Range{}, fmt.Errorf("%w: start: %v", ErrInvalid, err)
	}

	end, err := strconv.ParseInt(endText, 10, 64)
	if err != nil {
		return Range{}, fmt.Errorf("%w: end: %v", ErrInvalid, err)
	}

	return New(start, end)
}

// Length returns the number of bytes in the range.
func (r Range) Length() int64 { return r.End - r.Start + 1 }

// HeaderValue returns the HTTP Range header value.
func (r Range) HeaderValue() string {
	return fmt.Sprintf("bytes=%d-%d", r.Start, r.End)
}

// ValidateSize verifies that the entire range is within an object of size.
func (r Range) ValidateSize(size int64) error {
	if size < 0 {
		return fmt.Errorf("%w: negative object size %d", ErrInvalid, size)
	}

	if r.Start >= size || r.End >= size {
		return fmt.Errorf("%w: range %d-%d, size %d", ErrUnsatisfiable, r.Start, r.End, size)
	}

	return nil
}

// ContentRange is a satisfied HTTP Content-Range value.
type ContentRange struct {
	Range Range
	Size  int64
}

// ParseContentRange parses Content-Range: bytes N-M/TOTAL.
func ParseContentRange(value string) (ContentRange, error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "bytes ") {
		return ContentRange{}, fmt.Errorf("%w: expected bytes N-M/TOTAL", ErrInvalid)
	}

	boundsText, sizeText, ok := strings.Cut(strings.TrimPrefix(value, "bytes "), "/")
	if !ok || sizeText == "" || sizeText == "*" || strings.Contains(sizeText, "/") {
		return ContentRange{}, fmt.Errorf("%w: expected known total size", ErrInvalid)
	}

	r, err := ParseExact("bytes=" + boundsText)
	if err != nil {
		return ContentRange{}, err
	}

	size, err := strconv.ParseInt(sizeText, 10, 64)
	if err != nil || size <= 0 {
		return ContentRange{}, fmt.Errorf("%w: invalid total size %q", ErrInvalid, sizeText)
	}

	if err := r.ValidateSize(size); err != nil {
		return ContentRange{}, err
	}

	return ContentRange{Range: r, Size: size}, nil
}

// ValidateResponse verifies that a partial response exactly matches the
// requested range and body length.
func ValidateResponse(requested Range, value string, contentLength int64) (int64, error) {
	got, err := ParseContentRange(value)
	if err != nil {
		return 0, err
	}

	if got.Range != requested {
		return 0, fmt.Errorf("%w: got %d-%d, want %d-%d", ErrInvalid,
			got.Range.Start, got.Range.End, requested.Start, requested.End)
	}

	if contentLength >= 0 && contentLength != requested.Length() {
		return 0, fmt.Errorf("%w: content length %d, want %d", ErrInvalid,
			contentLength, requested.Length())
	}

	return got.Size, nil
}

// UnsatisfiedContentRange returns the Content-Range value for a 416 response.
func UnsatisfiedContentRange(size int64) string { return fmt.Sprintf("bytes */%d", size) }
