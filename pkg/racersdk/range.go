// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import "math"

// PageSize is the v1 whole-page origin transfer unit.
const PageSize ByteLength = 16 * 1024 * 1024

// RangeKind distinguishes the inclusive closed, open-ended, and suffix forms.
// Zero represents an unspecified range.
type RangeKind uint8

const (
	RangeClosed RangeKind = iota + 1
	RangeFrom
	RangeSuffix
)

// Range is a validated, unresolved single byte range. Its zero value is absent.
type Range struct {
	kind        RangeKind
	first, last uint64
}

func ClosedRange(first, last ByteOffset) (Range, error) {
	if first > last || last > math.MaxInt64 {
		return Range{}, failure(ErrorInvalidArgument, "range", nil)
	}

	return Range{kind: RangeClosed, first: uint64(first), last: uint64(last)}, nil
}

func FromRange(first ByteOffset) (Range, error) {
	if first > math.MaxInt64 {
		return Range{}, failure(ErrorInvalidArgument, "range", nil)
	}

	return Range{kind: RangeFrom, first: uint64(first)}, nil
}

// SuffixRange accepts zero count as valid syntax; Resolve then reports 416.
func SuffixRange(count ByteLength) (Range, error) {
	if count > math.MaxInt64 {
		return Range{}, failure(ErrorInvalidArgument, "range", nil)
	}

	return Range{kind: RangeSuffix, last: uint64(count)}, nil
}

func (r Range) Kind() RangeKind { return r.kind }
func (r Range) First() (ByteOffset, bool) {
	return ByteOffset(r.first), r.kind == RangeClosed || r.kind == RangeFrom
}
func (r Range) Last() (ByteOffset, bool)         { return ByteOffset(r.last), r.kind == RangeClosed }
func (r Range) SuffixLength() (ByteLength, bool) { return ByteLength(r.last), r.kind == RangeSuffix }

// Resolve returns inclusive bounds in the selected immutable version. No page
// descriptors or object-sized buffers are allocated. An absent range is invalid.
func (r Range) Resolve(size ByteLength) (first, last ByteOffset, err error) {
	if size > math.MaxInt64 || r.kind < RangeClosed || r.kind > RangeSuffix {
		return 0, 0, failure(ErrorInvalidArgument, "range", nil)
	}

	if size == 0 {
		return 0, 0, failure(ErrorUnsatisfiableRange, "range", nil)
	}

	end := uint64(size) - 1

	start := r.first
	switch r.kind {
	case RangeClosed:
		end = min(end, r.last)
	case RangeSuffix:
		if r.last == 0 {
			return 0, 0, failure(ErrorUnsatisfiableRange, "range", nil)
		}

		start = uint64(size) - min(uint64(size), r.last)
	}

	if start >= uint64(size) {
		return 0, 0, failure(ErrorUnsatisfiableRange, "range", nil)
	}

	return ByteOffset(start), ByteOffset(end), nil
}

// validatePageShape rejects shapes known to be invalid before version lookup.
// Short-final-page ends can only be checked once the selected size is known.
func validatePageShape(r Range) error {
	if r.kind != RangeClosed || r.first%uint64(PageSize) != 0 || r.last > nominalPageEnd(r.first) {
		return failure(ErrorInvalidArgument, "origin range", nil)
	}

	return nil
}

func nominalPageEnd(first uint64) uint64 {
	return first + min(uint64(PageSize)-1, uint64(math.MaxInt64)-first)
}

func resolveOriginRange(r Range, size ByteLength) (ByteOffset, ByteOffset, error) {
	if err := validatePageShape(r); err != nil {
		return 0, 0, err
	}

	first, last, err := r.Resolve(size)
	if err != nil {
		return 0, 0, err
	}

	if r.last != nominalPageEnd(r.first) && r.last != uint64(size)-1 {
		return 0, 0, failure(ErrorInvalidArgument, "origin range", nil)
	}

	return first, last, nil
}
