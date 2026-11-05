// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package ublk is a minimal binding to the Linux ublk (userspace block device)
// driver that serves a read-only block device whose reads come from a Handler.
//
// Structure:
//
//   - uring_linux.go is a tiny io_uring wrapper built on raw syscalls. Rings are
//     created with IORING_SETUP_SQE128 because ublk URING_CMDs carry their
//     payload in the SQE command area.
//   - ctrl_linux.go drives /dev/ublk-control (ADD_DEV, SET_PARAMS, START_DEV,
//     STOP_DEV, DEL_DEV) using ioctl-encoded command opcodes.
//   - queue_linux.go runs one OS-thread-locked goroutine per hardware queue.
//     Each queue owns an io_uring, a read-only mmap of its io descriptor array
//     from /dev/ublkc<ID>, and one anonymous mmap IO buffer per tag. Reads are
//     handed to worker goroutines (bounded by the queue depth) that call the
//     Handler and signal completion through an eventfd that the queue ring
//     polls. The queue thread then batches COMMIT_AND_FETCH_REQ commands.
//   - device_linux.go ties it together: Start, Close, Delete and teardown.
//   - dispatch.go maps ublk operations to results and is kernel-independent.
//
// The minimum supported kernel is 6.8. The device is always read-only.
package ublk
