// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"errors"
	"log/slog"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
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

		err := q.serveEIO()
		if err == nil || !strings.Contains(err.Error(), "out-of-range tag") {
			t.Fatalf("tag %d: serveEIO = %v, want out-of-range tag error", tag, err)
		}

		if head != 1 {
			t.Fatalf("tag %d: CQ head = %d, want the entry consumed", tag, head)
		}
	}
}

// newEIOQueue returns a queue with a real ring and eventfd whose io
// commands go to /dev/null, so they complete with an error instead of
// reaching a ublk device.
func newEIOQueue(t *testing.T) *queue {
	t.Helper()

	d, _, _ := newStartupDevice(t)
	d.cfg.Logger = slog.New(slog.DiscardHandler)

	null, err := unix.Open("/dev/null", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = unix.Close(null) })

	efd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = unix.Close(efd) })

	const depth = 4

	q := &queue{
		dev:     d,
		depth:   depth,
		charFd:  null,
		ring:    newTestRing(t, depth+1),
		spare:   newTestRing(t, rescueRingEntries(depth)),
		efd:     efd,
		bufs:    make([][]byte, depth),
		exited:  make(chan struct{}),
		serving: make(chan struct{}),
	}

	for i := range q.bufs {
		q.bufs[i] = make([]byte, 512)
	}

	return q
}

// TestServeEIOServesAfterStartup checks that a failed queue keeps
// answering I/O after startup resolves, until it is stopped. Linux 7.0
// can still send a partition scan read then, and a request left with
// the queue blocks STOP_DEV.
func TestServeEIOServesAfterStartup(t *testing.T) {
	q := newEIOQueue(t)
	close(q.dev.startDone)

	res := make(chan error, 1)

	go func() { res <- q.serveEIO() }()

	select {
	case err := <-res:
		t.Fatalf("serveEIO returned %v after startup resolved, want it to serve until stopped", err)
	case <-time.After(200 * time.Millisecond):
	}

	q.stop()

	select {
	case err := <-res:
		if err != nil {
			t.Fatalf("serveEIO = %v after stop, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveEIO did not return after stop")
	}
}

// TestServeEIOSubmitsAnswerBeforeStopping delivers a request to a queue
// that is already stopping. serveEIO must submit the EIO answer before
// it returns: an answer left in the SQ when the ring closes never
// reaches the kernel, and the request stays with the queue.
func TestServeEIOSubmitsAnswerBeforeStopping(t *testing.T) {
	q := newEIOQueue(t)
	r := q.ring

	// A NOP completes with result 0 and our user data, which serveEIO
	// cannot tell from a request delivered for tag 1.
	s := r.getSQE()
	s.Opcode = ioringOpNop
	s.UserData = 1

	if err := r.submitAndWait(1); err != nil {
		t.Fatal(err)
	}

	q.stopping.Store(true)

	if err := q.serveEIO(); err != nil {
		t.Fatalf("serveEIO = %v", err)
	}

	if head := atomic.LoadUint32(r.sqHead); head != r.sqLocalTail {
		t.Fatalf("SQ head %d, local tail %d: the EIO answer was not submitted", head, r.sqLocalTail)
	}

	c, err := r.waitCQE()
	if err != nil {
		t.Fatal(err)
	}

	if c.UserData != 1 {
		t.Fatalf("completion user data %d, want the answer for tag 1", c.UserData)
	}
}

// TestHoldUntilSafeServesUntilStopped checks that a servable failure
// tells teardown it is serving and holds its ring until stopped, even
// after startup has resolved.
func TestHoldUntilSafeServesUntilStopped(t *testing.T) {
	q := newEIOQueue(t)
	close(q.dev.startDone)

	res := make(chan string, 1)

	go func() { res <- q.holdUntilSafe(errServable{errors.New("fault")}) }()

	select {
	case <-q.serving:
	case <-time.After(5 * time.Second):
		t.Fatal("queue never reported that it serves")
	}

	select {
	case got := <-res:
		t.Fatalf("holdUntilSafe = %q before stop", got)
	case <-time.After(100 * time.Millisecond):
	}

	q.stop()

	select {
	case got := <-res:
		if got != holdStopped {
			t.Fatalf("holdUntilSafe = %q, want %q", got, holdStopped)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("holdUntilSafe did not return after stop")
	}
}

// TestHoldUntilSafeBrokenEntryDoesNotReportServing checks that a queue
// whose EIO loop fails on entry does not tell teardown it serves.
// Teardown must then wait for it to close its ring before STOP_DEV.
func TestHoldUntilSafeBrokenEntryDoesNotReportServing(t *testing.T) {
	q := newEIOQueue(t)
	close(q.dev.startDone)
	noRescue(t)

	testHooks.serveEIO = func(*queue) error { return errors.New("ring broken") }

	t.Cleanup(func() { testHooks.serveEIO = nil })

	if got := q.holdUntilSafe(errServable{errors.New("fault")}); got != holdResolved {
		t.Fatalf("holdUntilSafe = %q, want %q", got, holdResolved)
	}

	if isClosed(q.serving) {
		t.Fatal("a queue that cannot serve reported that it serves")
	}
}

// TestServeEIOSeesStopConsumedByDrain stops the queue just before its
// EIO loop drains the eventfd, so drain consumes the stop's only wake.
// serveEIO must still notice the stop instead of arming its poll and
// waiting for a wake that never comes.
func TestServeEIOSeesStopConsumedByDrain(t *testing.T) {
	q := newEIOQueue(t)

	var once atomic.Bool

	testHooks.eioBeforeDrain = func(q *queue) {
		if !once.Swap(true) {
			q.stop()
		}
	}

	t.Cleanup(func() { testHooks.eioBeforeDrain = nil })

	res := make(chan error, 1)

	go func() { res <- q.serveEIO() }()

	select {
	case err := <-res:
		if err != nil {
			t.Fatalf("serveEIO = %v", err)
		}
	case <-time.After(5 * time.Second):
		// The goroutine stays blocked; wake it so the test can finish.
		q.wake()
		t.Fatal("serveEIO lost the stop wake and blocked")
	}
}
