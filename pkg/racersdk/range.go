// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import "github.com/Azure/unbounded/pkg/racersdk/internal/wire"

// PageSize is the v1 whole-page origin transfer unit.
const PageSize ByteLength = wire.PageSize

// Range is an inclusive closed byte range. Its zero value is absent.
// Origin callbacks receive whole-page ranges; client continuations are private.
type Range struct {
	present     bool
	first, last uint64
}

// Bounds returns the inclusive, unresolved wire bounds and whether a range is
// present. Origin adapters should use Resolve to validate against object size.
func (r Range) Bounds() (first, last ByteOffset, present bool) {
	return ByteOffset(r.first), ByteOffset(r.last), r.present
}

func ClosedRange(first, last ByteOffset) (Range, error) {
	r, err := wire.ClosedRange(uint64(first), uint64(last))
	return fromWireRange(r), fromWireError(err)
}

func (r Range) resolve(size ByteLength) (ByteOffset, ByteOffset, error) {
	first, last, err := r.wire().Resolve(uint64(size))
	return ByteOffset(first), ByteOffset(last), fromWireError(err)
}

// Resolve validates a whole origin page and returns its inclusive bounds in the
// selected immutable version. The final page is shortened at EOF. An absent range,
// unaligned start, or partial nonfinal page is invalid; an empty object is unsatisfiable.
func (r Range) Resolve(size ByteLength) (ByteOffset, ByteOffset, error) {
	first, last, err := r.wire().ResolvePage(uint64(size))
	return ByteOffset(first), ByteOffset(last), fromWireError(err)
}
