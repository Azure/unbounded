// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import "math"

// PageSize is the v1 whole-page origin transfer unit.
const PageSize ByteLength = 16 * 1024 * 1024

// Range is an inclusive closed byte range. Its zero value is absent.
// Origin callbacks receive whole-page ranges; client continuations are private.
type Range struct {
	present     bool
	first, last uint64
}

func ClosedRange(first, last ByteOffset) (Range, error) {
	if first > last || last > math.MaxInt64 {
		return Range{}, failure(ErrorInvalidArgument, "range", nil)
	}

	return Range{present: true, first: uint64(first), last: uint64(last)}, nil
}

func (r Range) resolve(size ByteLength) (first, last ByteOffset, err error) {
	if size > math.MaxInt64 || !r.present || r.first > r.last || r.last > math.MaxInt64 {
		return 0, 0, failure(ErrorInvalidArgument, "range", nil)
	}

	if r.first >= uint64(size) {
		return 0, 0, failure(ErrorUnsatisfiableRange, "range", nil)
	}

	return ByteOffset(r.first), ByteOffset(min(uint64(size)-1, r.last)), nil
}

// validatePageShape rejects shapes known to be invalid before version lookup.
// Short-final-page ends can only be checked once the selected size is known.
func validatePageShape(r Range) error {
	if !r.present || r.first > r.last || r.last > math.MaxInt64 || r.first%uint64(PageSize) != 0 || r.last > nominalPageEnd(r.first) {
		return failure(ErrorInvalidArgument, "origin range", nil)
	}

	return nil
}

func nominalPageEnd(first uint64) uint64 {
	return first + min(uint64(PageSize)-1, uint64(math.MaxInt64)-first)
}

// Resolve validates a whole origin page and returns its inclusive bounds in the
// selected immutable version. The final page is shortened at EOF. An absent range,
// unaligned start, or partial nonfinal page is invalid; an empty object is unsatisfiable.
func (r Range) Resolve(size ByteLength) (ByteOffset, ByteOffset, error) {
	if err := validatePageShape(r); err != nil {
		return 0, 0, err
	}

	first, last, err := r.resolve(size)
	if err != nil {
		return 0, 0, err
	}

	if r.last != nominalPageEnd(r.first) && r.last != uint64(size)-1 {
		return 0, 0, failure(ErrorInvalidArgument, "origin range", nil)
	}

	return first, last, nil
}
