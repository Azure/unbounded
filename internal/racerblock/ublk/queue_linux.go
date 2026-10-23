// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ublkIOResAbort is UBLK_IO_RES_ABORT (-ENODEV).
const ublkIOResAbort = -19

// errStoppedExternally reports that the kernel aborted every io command
// while the device was not being torn down locally, for example because
// another process issued STOP_DEV or DEL_DEV for it.
var errStoppedExternally = fmt.Errorf("device stopped externally: %w", syscall.ENODEV)

// pollUserData tags the eventfd poll completion; io commands use their tag.
const pollUserData = 1 << 63

// completion is a finished handler call waiting to be committed.
type completion struct {
	tag uint16
	res int32
}

// queue serves one ublk hardware queue.
type queue struct {
	dev    *Device
	id     int
	depth  int
	charFd int

	ring *uring
	// spare is the rescue ring, set up with the queue so a rescue never
	// needs a new kernel resource after a failure. A rescue takes it.
	spare *uring
	// oldRing is the ring a rescue replaced. It stays open, and is
	// never entered again, until the queue thread exits. See rescue.
	oldRing *uring
	desc    []byte   // read-only mmap of the io descriptor array
	bufs    [][]byte // per-tag anonymous mmap IO buffers
	efd     int      // eventfd signaled by workers
	pollP   bool     // poll is armed (queue thread only)

	mu      sync.Mutex
	pending []completion

	// busy marks tags whose request a handler goroutine is serving. The
	// queue thread owns it. A rescue never answers a busy tag itself;
	// the handler's result is committed through drain instead.
	busy []bool

	running  bool // set before run starts; read after
	stopping atomic.Bool
	fault    atomic.Pointer[error] // test-injected queue error
	failed   atomic.Bool           // run failed on its own
	exited   chan struct{}

	// serving is closed when a failed queue answers I/O with EIO until
	// it is stopped, so teardown need not wait for it to exit first.
	serving     chan struct{}
	servingOnce sync.Once
}

// queueRingEntries is the size of a queue's ring: one io command per tag
// and the eventfd poll. The queue never has more in flight, which
// harvestOld relies on.
func queueRingEntries(depth int) uint32 {
	return uint32(depth + 1) //nolint:gosec // depth is at most MaxQueueDepth
}

func newQueue(d *Device, id int) (*queue, error) {
	cfg := d.cfg
	q := &queue{
		dev:     d,
		id:      id,
		depth:   cfg.Depth,
		charFd:  d.charFd,
		efd:     -1,
		busy:    make([]bool, cfg.Depth),
		exited:  make(chan struct{}),
		serving: make(chan struct{}),
	}

	var err error

	q.ring, err = newURing(queueRingEntries(cfg.Depth))
	if err != nil {
		return q.failNew(fmt.Errorf("ublk: queue %d ring: %w", id, err))
	}

	q.spare, err = newRescueRing(q)
	if err != nil {
		return q.failNew(fmt.Errorf("ublk: queue %d rescue ring: %w", id, err))
	}

	page := unix.Getpagesize()
	sz := descArraySize(cfg.Depth, page)

	q.desc, err = unix.Mmap(d.charFd, descArrayOffset(id, page), sz, unix.PROT_READ, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		return q.failNew(fmt.Errorf("ublk: queue %d mmap descriptors: %w", id, err))
	}

	q.bufs = make([][]byte, cfg.Depth)
	for t := range q.bufs {
		q.bufs[t], err = unix.Mmap(-1, 0, cfg.MaxIOBytes, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
		if err != nil {
			return q.failNew(fmt.Errorf("ublk: queue %d mmap io buffer: %w", id, err))
		}
	}

	q.efd, err = unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if err != nil {
		return q.failNew(fmt.Errorf("ublk: queue %d eventfd: %w", id, err))
	}

	return q, nil
}

// release frees queue resources. The ring must not be in use.
func (q *queue) release() error {
	var errs []error

	if q.ring != nil {
		errs = append(errs, q.ring.close())
		q.ring = nil
	}

	if q.oldRing != nil {
		errs = append(errs, q.oldRing.close())
		q.oldRing = nil
	}

	if q.spare != nil {
		errs = append(errs, q.spare.close())
		q.spare = nil
	}

	for i, b := range q.bufs {
		if b != nil {
			errs = append(errs, unix.Munmap(b))
			q.bufs[i] = nil
		}
	}

	if q.desc != nil {
		errs = append(errs, unix.Munmap(q.desc))
		q.desc = nil
	}

	if q.efd >= 0 {
		errs = append(errs, unix.Close(q.efd))
		q.efd = -1
	}

	return errors.Join(errs...)
}

// failNew releases a partially built queue and returns err.
func (q *queue) failNew(err error) (*queue, error) {
	return nil, errors.Join(err, q.release())
}

// wake interrupts the queue thread.
func (q *queue) wake() {
	var one [8]byte

	*(*uint64)(unsafe.Pointer(&one[0])) = 1
	// EAGAIN means the counter is saturated, so the queue is already awake.
	if _, err := unix.Write(q.efd, one[:]); err != nil && !errors.Is(err, unix.EAGAIN) {
		q.dev.cfg.Logger.Warn("ublk: eventfd write failed", "queue", q.id, "err", err)
	}
}

// localStop reports whether this process is tearing the queue down.
func (q *queue) localStop() bool {
	return q.stopping.Load() || q.dev.closing.Load()
}

// stop asks the queue thread to exit.
func (q *queue) stop() {
	q.stopping.Store(true)
	q.wake()
}

// complete is called by workers to hand a result back to the queue thread.
func (q *queue) complete(tag uint16, res int32) {
	q.mu.Lock()
	q.pending = append(q.pending, completion{tag: tag, res: res})
	q.mu.Unlock()
	q.wake()
}

func (q *queue) descAt(tag uint16) ioDesc {
	return *(*ioDesc)(unsafe.Pointer(&q.desc[int(tag)*ioDescSize]))
}

// sqe returns a free SQE, flushing the ring to the kernel if it is full.
func (q *queue) sqe() (*sqe, error) {
	if s := q.ring.getSQE(); s != nil {
		return s, nil
	}

	if err := q.ring.submitAndWait(0); err != nil {
		return nil, err
	}

	if s := q.ring.getSQE(); s != nil {
		return s, nil
	}

	return nil, errRingFull
}

func (q *queue) prepIO(op uint32, tag uint16, res int32) error {
	s, err := q.sqe()
	if err != nil {
		return err
	}

	s.Opcode = ioringOpURingCmd
	s.Fd = int32(q.charFd)
	s.Off = uint64(op)
	s.UserData = uint64(tag)

	cmd := (*ioCmd)(unsafe.Pointer(&cmdArea(s)[0]))
	cmd.QID = uint16(q.id)
	cmd.Tag = tag
	cmd.Result = res
	cmd.Addr = uint64(uintptr(unsafe.Pointer(&q.bufs[tag][0])))

	return nil
}

func (q *queue) armPoll() error {
	s, err := q.sqe()
	if err != nil {
		return err
	}

	s.Opcode = ioringOpPollAdd
	s.Fd = int32(q.efd)
	s.OpFlags = unix.POLLIN
	s.UserData = pollUserData
	q.pollP = true

	return nil
}

// run is the queue thread. ready receives the result of the initial
// FETCH_REQ submission. The ring is closed when run returns.
func (q *queue) run(ready chan<- error) {
	runtime.LockOSThread()
	// The thread is intentionally never unlocked: it exits with the
	// goroutine so the kernel sees the queue daemon task go away.
	defer close(q.exited)

	err := q.serve(ready)

	if testHooks.afterServe != nil {
		testHooks.afterServe(q, err)
	}

	// serve returns nil when this process stops the queue, so any error
	// is a real failure and is reported even if teardown started after
	// serve returned. Only the decision to hold the ring depends on
	// whether the stop is local.
	// Decide whether to hold the ring before reporting the failure:
	// fail schedules teardown, which sets closing and then checks failed
	// to wait for this queue before STOP_DEV. Rereading localStop after
	// fail would see our own teardown and skip the hold.
	hold := err != nil && !q.localStop()
	if hold {
		q.failed.Store(true)
	}

	if err != nil {
		q.dev.fail(fmt.Errorf("ublk: queue %d: %w", q.id, err))
	}

	if hold {
		reason := q.holdUntilSafe(err)
		if testHooks.failedRingClose != nil {
			testHooks.failedRingClose(q, reason)
		}
	} else if err != nil {
		q.rescueDuringTeardown(err)
	}

	if cerr := q.ring.close(); cerr != nil {
		q.dev.cfg.Logger.Warn("ublk: closing queue ring", "queue", q.id, "err", cerr)
	}

	q.ring = nil

	if q.oldRing != nil {
		if cerr := q.oldRing.close(); cerr != nil {
			q.dev.cfg.Logger.Warn("ublk: closing replaced queue ring", "queue", q.id, "err", cerr)
		}

		q.oldRing = nil
	}
}

// rescueDuringTeardown handles a queue whose loop failed after local
// teardown began. Until teardown stops the queue it may still be waiting
// in STOP_DEV for a request this queue owns, which on Linux 7.0 closing
// the ring would strand (see rescue). The queue therefore answers I/O
// through a rescue ring until it is stopped. Once stopped, STOP_DEV has
// returned or was never sent, so the ring can be closed.
func (q *queue) rescueDuringTeardown(err error) {
	if q.stopping.Load() {
		return
	}

	if rerr := q.rescue(); rerr != nil {
		q.dev.cfg.Logger.Error("ublk: queue failed during teardown and cannot answer I/O; STOP_DEV may block",
			"queue", q.id, "err", err, "rescueErr", rerr)
	}
}

func (q *queue) serve(ready chan<- error) error {
	err := q.fetchAll()
	ready <- err

	if err != nil {
		// START_DEV is never sent without every queue ready, so no
		// request can reach this queue.
		return errServable{err}
	}

	live := q.depth

	for {
		for {
			c, ok := q.ring.peekCQE()
			if !ok {
				break
			}

			q.ring.advanceCQ()

			if c.UserData == pollUserData {
				q.pollP = false
				continue
			}

			tag := uint16(c.UserData)

			switch c.Res {
			case 0:
				if err := q.delivered(tag, false); err != nil {
					return err
				}

				if err := q.handle(tag); err != nil {
					return err
				}
			case ublkIOResAbort:
				live--
			default:
				if q.localStop() {
					live--
					continue
				}

				// The command completed, so no request is pending on it.
				return errServable{fmt.Errorf("io command tag %d: %w", tag, syscall.Errno(-c.Res))}
			}
		}

		if q.stopping.Load() {
			return nil
		}

		if f := q.fault.Load(); f != nil {
			return errServable{*f}
		}

		if live == 0 {
			if q.localStop() {
				return nil
			}

			return errStoppedExternally
		}

		if err := q.drain(); err != nil {
			return err
		}

		if !q.pollP {
			if err := q.armPoll(); err != nil {
				return err
			}
		}

		if err := q.ring.submitAndWait(1); err != nil {
			return err
		}
	}
}

// fetchAll submits a FETCH_REQ for every tag and reports whether the
// kernel accepted all of them. An accepted FETCH_REQ stays pending until
// an I/O arrives, while a rejected one completes with an error during
// submission. Without DEFER_TASKRUN, io_uring_enter posts such inline
// completions before it returns, so any rejection is already in the CQ
// here. A rejected tag leaves the queue not ready and START_DEV would
// wait for it forever, so it must be reported now.
func (q *queue) fetchAll() error {
	for t := range q.depth {
		tag := t
		if testHooks.fetchTag != nil {
			tag = testHooks.fetchTag(q.id, t)
		}

		if err := q.prepIO(uIOFetchReq, uint16(tag), -1); err != nil {
			return err
		}
	}

	if err := q.armPoll(); err != nil {
		return err
	}

	if err := q.ring.submitAndWait(0); err != nil {
		return err
	}

	return q.checkFetched()
}

// checkFetched consumes completions posted while submitting FETCH_REQs.
// Only the eventfd poll may complete successfully this early.
func (q *queue) checkFetched() error {
	for {
		c, ok := q.ring.peekCQE()
		if !ok {
			return nil
		}

		q.ring.advanceCQ()

		if c.UserData == pollUserData {
			q.pollP = false
			continue
		}

		if c.Res < 0 {
			return fmt.Errorf("FETCH_REQ tag %d: %w", c.UserData, syscall.Errno(-c.Res))
		}

		return fmt.Errorf("FETCH_REQ tag %d: unexpected early completion %d", c.UserData, c.Res)
	}
}

// drain resets the eventfd and commits finished handler results.
func (q *queue) drain() error {
	var b [8]byte

	// EAGAIN just means nothing was signaled since the last drain.
	if _, err := unix.Read(q.efd, b[:]); err != nil && !errors.Is(err, unix.EAGAIN) {
		return fmt.Errorf("eventfd read: %w", err)
	}

	q.mu.Lock()
	done := q.pending
	q.pending = nil
	q.mu.Unlock()

	for i, c := range done {
		if err := q.prepIO(uIOCommitAndFetchReq, c.tag, c.res); err != nil {
			// Put back every result not yet prepared, and keep its tag
			// busy, so a rescue commits it instead of losing it.
			q.mu.Lock()
			q.pending = append(done[i:], q.pending...)
			q.mu.Unlock()

			return err
		}

		q.setBusy(c.tag, false)
	}

	return nil
}

// delivered runs the test hook for a request the kernel just handed to
// the queue. An error leaves the request unanswered.
func (q *queue) delivered(tag uint16, eio bool) error {
	if testHooks.delivered != nil {
		return testHooks.delivered(q, tag, eio)
	}

	return nil
}

// setBusy records whether a handler serves tag. Queues built without a
// busy table, as in some tests, do not track it.
func (q *queue) setBusy(tag uint16, busy bool) {
	if int(tag) < len(q.busy) {
		q.busy[tag] = busy
	}
}

// handle processes a request the kernel delivered for tag.
func (q *queue) handle(tag uint16) error {
	if int(tag) >= q.depth {
		return fmt.Errorf("kernel returned out-of-range tag %d", tag)
	}

	d := q.descAt(tag)

	action, res := dispatch(d.OpFlags)
	if action == actionComplete {
		return q.prepIO(uIOCommitAndFetchReq, tag, res)
	}

	n := int(d.NrSectors) << 9
	if n > len(q.bufs[tag]) {
		return q.prepIO(uIOCommitAndFetchReq, tag, -int32(syscall.EIO))
	}

	buf := q.bufs[tag][:n]
	off := int64(d.StartSector) << 9 //nolint:gosec // sector counts fit in int64

	q.setBusy(tag, true)
	q.dev.workers.Add(1)

	go func() {
		defer q.dev.workers.Done()

		res, err := serveRead(q.dev.hctx, q.dev.handler, buf, off)
		if err != nil && q.dev.hctx.Err() == nil {
			q.dev.cfg.Logger.Debug("ublk read failed", "dev", q.dev.id, "queue", q.id, "off", off, "len", n, "err", err)
		}

		q.complete(tag, res)
	}()

	return nil
}
