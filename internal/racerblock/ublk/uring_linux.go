// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"errors"
	"fmt"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// io_uring constants from <linux/io_uring.h>.
const (
	ioringOpNop      = 0
	ioringOpPollAdd  = 6
	ioringOpURingCmd = 46
	// ioringOpAsyncCancel is IORING_OP_ASYNC_CANCEL.
	ioringOpAsyncCancel = 14

	// iosqeAsync is IOSQE_ASYNC: always issue from an io-wq worker.
	iosqeAsync = 1 << 4

	ioringSetupSQE128 = 1 << 10

	ioringFeatSingleMmap = 1 << 0

	ioringOffSQRing = 0
	ioringOffCQRing = 0x8000000
	ioringOffSQEs   = 0x10000000

	ioringEnterGetEvents = 1 << 0

	sqe128Size = 128
	cqeSize    = 16
	// sqeCmdOffset is offsetof(struct io_uring_sqe, cmd).
	sqeCmdOffset = 48
	// sqeCmdLen is the size of the command area in a 128-byte SQE.
	sqeCmdLen = sqe128Size - sqeCmdOffset
)

type sqringOffsets struct {
	Head        uint32
	Tail        uint32
	RingMask    uint32
	RingEntries uint32
	Flags       uint32
	Dropped     uint32
	Array       uint32
	Resv1       uint32
	UserAddr    uint64
}

type cqringOffsets struct {
	Head        uint32
	Tail        uint32
	RingMask    uint32
	RingEntries uint32
	Overflow    uint32
	Cqes        uint32
	Flags       uint32
	Resv1       uint32
	UserAddr    uint64
}

type uringParams struct {
	SQEntries    uint32
	CQEntries    uint32
	Flags        uint32
	SQThreadCPU  uint32
	SQThreadIdle uint32
	Features     uint32
	WQFd         uint32
	Resv         [3]uint32
	SQOff        sqringOffsets
	CQOff        cqringOffsets
}

// sqe is the 64-byte head of a struct io_uring_sqe. With SQE128 each entry
// is followed by 64 more bytes that extend the cmd area.
type sqe struct {
	Opcode      uint8
	Flags       uint8
	IOPrio      uint16
	Fd          int32
	Off         uint64 // also cmd_op (low 32 bits) for URING_CMD
	Addr        uint64
	Len         uint32
	OpFlags     uint32 // poll32_events, uring_cmd_flags, ...
	UserData    uint64
	BufIndex    uint16
	Personality uint16
	SpliceFdIn  int32
	Addr3       uint64
	Pad2        uint64
}

type cqe struct {
	UserData uint64
	Res      int32
	Flags    uint32
}

// uring is a minimal single-threaded io_uring with 128-byte SQEs. A uring
// must only be used by one goroutine at a time.
type uring struct {
	fd int

	sqRing []byte
	cqRing []byte // aliases sqRing with IORING_FEAT_SINGLE_MMAP
	sqes   []byte

	sqHead    *uint32
	sqTail    *uint32
	sqMask    uint32
	sqEntries uint32
	sqArray   unsafe.Pointer

	cqHead *uint32
	cqTail *uint32
	cqMask uint32
	cqes   unsafe.Pointer

	// sqLocalTail counts SQEs prepared but not yet published.
	sqLocalTail uint32
	// toSubmit counts SQEs published but not yet consumed by the kernel.
	toSubmit uint32
}

// newURing creates an io_uring with at least entries submission entries.
func newURing(entries uint32) (*uring, error) {
	if testHooks.uringSetup != nil {
		testHooks.uringSetup()
	}

	var p uringParams

	p.Flags = ioringSetupSQE128

	fd, _, errno := unix.Syscall(unix.SYS_IO_URING_SETUP, uintptr(entries), uintptr(unsafe.Pointer(&p)), 0)
	if errno != 0 {
		return nil, fmt.Errorf("io_uring_setup: %w", errno)
	}

	r := &uring{fd: int(fd)}
	if err := r.mapRings(&p); err != nil {
		return nil, errors.Join(err, r.close())
	}

	return r, nil
}

func (r *uring) mapRings(p *uringParams) error {
	sqSize := int(p.SQOff.Array + p.SQEntries*4)
	cqSize := int(p.CQOff.Cqes + p.CQEntries*cqeSize)
	single := p.Features&ioringFeatSingleMmap != 0

	if single {
		sqSize = max(sqSize, cqSize)
	}

	var err error

	r.sqRing, err = unix.Mmap(r.fd, ioringOffSQRing, sqSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		return fmt.Errorf("mmap sq ring: %w", err)
	}

	if single {
		r.cqRing = r.sqRing
	} else {
		r.cqRing, err = unix.Mmap(r.fd, ioringOffCQRing, cqSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
		if err != nil {
			return fmt.Errorf("mmap cq ring: %w", err)
		}
	}

	r.sqes, err = unix.Mmap(r.fd, ioringOffSQEs, int(p.SQEntries)*sqe128Size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		return fmt.Errorf("mmap sqes: %w", err)
	}

	sq := unsafe.Pointer(&r.sqRing[0])
	cq := unsafe.Pointer(&r.cqRing[0])

	r.sqHead = (*uint32)(unsafe.Add(sq, p.SQOff.Head))
	r.sqTail = (*uint32)(unsafe.Add(sq, p.SQOff.Tail))
	r.sqMask = *(*uint32)(unsafe.Add(sq, p.SQOff.RingMask))
	r.sqEntries = *(*uint32)(unsafe.Add(sq, p.SQOff.RingEntries))
	r.sqArray = unsafe.Add(sq, p.SQOff.Array)
	r.cqHead = (*uint32)(unsafe.Add(cq, p.CQOff.Head))
	r.cqTail = (*uint32)(unsafe.Add(cq, p.CQOff.Tail))
	r.cqMask = *(*uint32)(unsafe.Add(cq, p.CQOff.RingMask))
	r.cqes = unsafe.Add(cq, p.CQOff.Cqes)
	r.sqLocalTail = atomic.LoadUint32(r.sqTail)

	return nil
}

// close releases the ring. It is safe to call on a partially set up ring
// and more than once.
func (r *uring) close() error {
	var errs []error

	if r.sqes != nil {
		errs = append(errs, unix.Munmap(r.sqes))
		r.sqes = nil
	}

	if r.cqRing != nil && len(r.sqRing) > 0 && &r.cqRing[0] != &r.sqRing[0] {
		errs = append(errs, unix.Munmap(r.cqRing))
	}

	r.cqRing = nil

	if r.sqRing != nil {
		errs = append(errs, unix.Munmap(r.sqRing))
		r.sqRing = nil
	}

	if r.fd >= 0 {
		errs = append(errs, unix.Close(r.fd))
		r.fd = -1
	}

	return errors.Join(errs...)
}

// getSQE returns a zeroed 128-byte SQE slot, or nil if the ring is full.
// The SQE is published by the next submit call.
func (r *uring) getSQE() *sqe {
	head := atomic.LoadUint32(r.sqHead)
	if r.sqLocalTail-head >= r.sqEntries {
		return nil
	}

	idx := r.sqLocalTail & r.sqMask
	r.sqLocalTail++

	slot := r.sqes[int(idx)*sqe128Size : int(idx+1)*sqe128Size]
	clear(slot)

	*(*uint32)(unsafe.Add(r.sqArray, idx*4)) = idx

	return (*sqe)(unsafe.Pointer(&slot[0]))
}

// cmdArea returns the 80-byte URING_CMD payload area of s.
func cmdArea(s *sqe) []byte {
	return unsafe.Slice((*byte)(unsafe.Add(unsafe.Pointer(s), sqeCmdOffset)), sqeCmdLen)
}

// flushSQ publishes prepared SQEs to the kernel-visible tail.
func (r *uring) flushSQ() {
	tail := atomic.LoadUint32(r.sqTail)
	if tail != r.sqLocalTail {
		r.toSubmit += r.sqLocalTail - tail
		atomic.StoreUint32(r.sqTail, r.sqLocalTail)
	}
}

// submitAndWait publishes prepared SQEs and calls io_uring_enter, waiting
// for at least waitNr completions. EINTR is retried.
func (r *uring) submitAndWait(waitNr uint32) error {
	r.flushSQ()

	var flags uintptr
	if waitNr > 0 {
		flags = ioringEnterGetEvents
	}

	for {
		n, _, errno := unix.Syscall6(unix.SYS_IO_URING_ENTER, uintptr(r.fd), uintptr(r.toSubmit), uintptr(waitNr), flags, 0, 0)
		if errno == syscall.EINTR {
			// Partial submissions are reflected in n only on success; on EINTR
			// nothing was consumed by this call.
			continue
		}

		if errno != 0 {
			return fmt.Errorf("io_uring_enter: %w", errno)
		}

		r.toSubmit -= uint32(n)

		if r.toSubmit > 0 && waitNr == 0 {
			continue
		}

		return nil
	}
}

// peekCQE returns the next completion without consuming it.
func (r *uring) peekCQE() (cqe, bool) {
	head := atomic.LoadUint32(r.cqHead)
	if head == atomic.LoadUint32(r.cqTail) {
		return cqe{}, false
	}

	c := *(*cqe)(unsafe.Add(r.cqes, uintptr(head&r.cqMask)*cqeSize))

	return c, true
}

// advanceCQ consumes one completion.
func (r *uring) advanceCQ() {
	atomic.StoreUint32(r.cqHead, atomic.LoadUint32(r.cqHead)+1)
}

// waitCQE blocks until a completion is available, consumes and returns it.
func (r *uring) waitCQE() (cqe, error) {
	for {
		if c, ok := r.peekCQE(); ok {
			r.advanceCQ()
			return c, nil
		}

		if err := r.submitAndWait(1); err != nil {
			return cqe{}, err
		}
	}
}

var errRingFull = errors.New("io_uring submission queue full")
