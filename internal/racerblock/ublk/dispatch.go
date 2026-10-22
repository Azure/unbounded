// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package ublk

import (
	"context"
	"errors"
	"fmt"
	"syscall"
)

// ublk io operations (UBLK_IO_OP_*).
const (
	opRead        = 0
	opWrite       = 1
	opFlush       = 2
	opDiscard     = 3
	opWriteSame   = 4
	opWriteZeroes = 5
)

// dispatchAction is what the queue thread does with a request.
type dispatchAction int

const (
	// actionComplete commits the request immediately with the given result.
	actionComplete dispatchAction = iota
	// actionRead hands the request to a worker that calls the Handler.
	actionRead
)

// dispatch maps a ublk descriptor operation to an action. For actionComplete
// the returned result is the value to commit (0 or -errno).
func dispatch(opFlags uint32) (dispatchAction, int32) {
	switch opFlags & 0xff {
	case opRead:
		return actionRead, 0
	case opWrite:
		return actionComplete, -int32(syscall.EROFS)
	case opFlush:
		return actionComplete, 0
	default:
		// DISCARD, WRITE_SAME, WRITE_ZEROES, zone ops and unknown ops.
		return actionComplete, -int32(syscall.EOPNOTSUPP)
	}
}

// serveRead calls h.ReadAt for a request and returns the result to commit:
// the byte count on success or -errno on failure. Handler panics become EIO.
func serveRead(ctx context.Context, h Handler, buf []byte, off int64) (res int32, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("ublk: handler panic: %v", r)
			res = -int32(syscall.EIO)
		}
	}()

	if err := h.ReadAt(ctx, buf, off); err != nil {
		return errnoResult(err), err
	}

	return int32(len(buf)), nil
}

// errnoResult converts a handler error to a negative errno result.
func errnoResult(err error) int32 {
	var errno syscall.Errno
	if errors.As(err, &errno) && errno != 0 {
		return -int32(errno)
	}

	return -int32(syscall.EIO)
}
