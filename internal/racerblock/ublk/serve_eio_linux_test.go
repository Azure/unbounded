// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"math"
	"strings"
	"testing"
	"unsafe"
)

// TestServeEIORejectsOutOfRangeTag feeds serveEIO a completion queue in
// local memory whose only entry carries a tag outside the queue depth.
// serveEIO must consume it and fail before touching the submission
// queue, which this fake ring does not have: any SQE or io_uring_enter
// use would dereference nil fields and panic. The large values check
// that the bound is applied to the full UserData before it is narrowed
// to a uint16 tag.
func TestServeEIORejectsOutOfRangeTag(t *testing.T) {
	const depth = 4

	for _, tag := range []uint64{depth, depth + 1, math.MaxUint16, math.MaxUint16 + 1, math.MaxUint16 + depth - 1} {
		var (
			cqes       [4]cqe
			head, tail uint32
		)

		cqes[0] = cqe{UserData: tag}
		tail = 1

		r := &uring{
			cqHead: &head,
			cqTail: &tail,
			cqMask: uint32(len(cqes) - 1),
			cqes:   unsafe.Pointer(&cqes[0]),
		}

		d, _, _ := newStartupDevice(t)
		q := &queue{dev: d, id: 0, depth: depth, ring: r}

		err := q.serveEIO(d.startDone)
		if err == nil || !strings.Contains(err.Error(), "out-of-range tag") {
			t.Fatalf("tag %d: serveEIO = %v, want out-of-range tag error", tag, err)
		}

		if head != 1 {
			t.Fatalf("tag %d: CQ head = %d, want the entry consumed", tag, head)
		}
	}
}
