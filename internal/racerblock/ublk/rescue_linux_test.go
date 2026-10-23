// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"errors"
	"slices"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// noRescue makes every rescue fail on entry, so a test reaches the
// broken-ring path behind it.
func noRescue(t *testing.T) {
	t.Helper()

	testHooks.rescue = func(*queue) error { return errors.New("rescue disabled by test") }

	t.Cleanup(func() { testHooks.rescue = nil })
}

// preparedIO is an io command prepared on a ring but not yet submitted.
type preparedIO struct {
	op  uint32
	tag uint16
	res int32
}

// prepared returns the io commands prepared on r that the kernel has
// not consumed.
func prepared(r *uring) []preparedIO {
	var out []preparedIO

	for i := atomic.LoadUint32(r.sqHead); i != r.sqLocalTail; i++ {
		idx := i & r.sqMask
		s := (*sqe)(unsafe.Pointer(&r.sqes[int(idx)*sqe128Size]))

		if s.Opcode != ioringOpURingCmd {
			continue
		}

		cmd := (*ioCmd)(unsafe.Pointer(&cmdArea(s)[0]))
		out = append(out, preparedIO{op: uint32(s.Off), tag: cmd.Tag, res: cmd.Result})
	}

	return out
}

// postCQE appends a fake completion to r's completion queue.
func postCQE(r *uring, c cqe) {
	tail := atomic.LoadUint32(r.cqTail)
	*(*cqe)(unsafe.Add(r.cqes, uintptr(tail&r.cqMask)*cqeSize)) = c
	atomic.StoreUint32(r.cqTail, tail+1)
}

// TestRescueSweepSkipsBusyTags checks that a rescue commits EIO for
// every tag except those a handler is still serving, whose real result
// drain commits later.
func TestRescueSweepSkipsBusyTags(t *testing.T) {
	q := newEIOQueue(t)
	q.busy = make([]bool, q.depth)
	q.busy[1] = true

	if err := q.sweep(-int32(syscall.EIO)); err != nil {
		t.Fatal(err)
	}

	var tags []uint16

	for _, p := range prepared(q.ring) {
		if p.op != uIOCommitAndFetchReq || p.res != -int32(syscall.EIO) {
			t.Fatalf("prepared %+v, want COMMIT_AND_FETCH_REQ with EIO", p)
		}

		tags = append(tags, p.tag)
	}

	if want := []uint16{0, 2, 3}; !slices.Equal(tags, want) {
		t.Fatalf("swept tags %v, want %v", tags, want)
	}
}

// TestRescueAnswersOldRingDeliveries checks that a request the kernel
// hands to the replaced ring is answered through the rescue ring, and
// that polls, aborts and errors there are ignored.
func TestRescueAnswersOldRingDeliveries(t *testing.T) {
	q := newEIOQueue(t)
	q.oldRing = newTestRing(t, uint32(q.depth+1))

	postCQE(q.oldRing, cqe{UserData: pollUserData, Res: 1})
	postCQE(q.oldRing, cqe{UserData: 2, Res: 0})
	postCQE(q.oldRing, cqe{UserData: 3, Res: ublkIOResAbort})
	postCQE(q.oldRing, cqe{UserData: 1, Res: -int32(syscall.EBUSY)})
	postCQE(q.oldRing, cqe{UserData: 99, Res: 0})

	if err := q.harvestOld(-int32(syscall.EIO)); err != nil {
		t.Fatal(err)
	}

	got := prepared(q.ring)
	want := []preparedIO{{op: uIOCommitAndFetchReq, tag: 2, res: -int32(syscall.EIO)}}

	if !slices.Equal(got, want) {
		t.Fatalf("prepared %+v, want %+v", got, want)
	}

	if _, ok := q.oldRing.peekCQE(); ok {
		t.Fatal("replaced ring still has completions")
	}

	if len(prepared(q.oldRing)) != 0 {
		t.Fatal("rescue prepared a command on the replaced ring")
	}
}

// TestHoldUntilSafeRescuesUntilStopped checks that a queue that may have
// lost a request answers I/O through a rescue ring, reports that it
// serves so teardown can issue STOP_DEV, and keeps serving until it is
// stopped. The replaced ring stays open until the queue is released.
func TestHoldUntilSafeRescuesUntilStopped(t *testing.T) {
	q := newEIOQueue(t)
	old := q.ring

	var rescued atomic.Bool

	testHooks.rescued = func(*queue) { rescued.Store(true) }

	t.Cleanup(func() { testHooks.rescued = nil })

	res := make(chan string, 1)

	go func() { res <- q.holdUntilSafe(errors.New("commit failed")) }()

	select {
	case <-q.serving:
	case <-time.After(5 * time.Second):
		t.Fatal("rescued queue never reported that it serves")
	}

	select {
	case got := <-res:
		t.Fatalf("holdUntilSafe = %q before stop", got)
	case <-time.After(100 * time.Millisecond):
	}

	q.stop()

	select {
	case got := <-res:
		if got != holdRescued {
			t.Fatalf("holdUntilSafe = %q, want %q", got, holdRescued)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rescue did not return after stop")
	}

	if !rescued.Load() {
		t.Fatal("rescue hook not called")
	}

	if q.oldRing != old || q.ring == old {
		t.Fatal("rescue did not keep the replaced ring")
	}

	// The test queue's buffers and eventfd are not release's to free.
	q.bufs = nil
	q.efd = -1

	if err := q.release(); err != nil {
		t.Fatal(err)
	}

	if q.oldRing != nil || q.ring != nil {
		t.Fatal("release left a ring open")
	}
}

// TestHoldUntilSafeServableRescuesWhenEIOLoopFails checks that a
// servable failure whose EIO loop breaks, including after it already
// reported serving, falls back to a rescue ring instead of the broken
// path.
func TestHoldUntilSafeServableRescuesWhenEIOLoopFails(t *testing.T) {
	q := newEIOQueue(t)

	testHooks.serveEIO = func(*queue) error { return errors.New("ring broken") }
	testHooks.published = func() (bool, error) {
		t.Error("rescued queue fell back to the broken path")
		return true, nil
	}

	t.Cleanup(func() {
		testHooks.serveEIO = nil
		testHooks.published = nil
	})

	res := make(chan string, 1)

	go func() { res <- q.holdUntilSafe(errServable{errors.New("fault")}) }()

	select {
	case <-q.serving:
	case <-time.After(5 * time.Second):
		t.Fatal("queue never reported that it serves")
	}

	q.stop()

	select {
	case got := <-res:
		if got != holdRescued {
			t.Fatalf("holdUntilSafe = %q, want %q", got, holdRescued)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rescue did not return after stop")
	}
}

// TestRescueFailureFallsBackToBrokenPath checks that a failing rescue
// does not report serving and leaves the queue on the broken path.
func TestRescueFailureFallsBackToBrokenPath(t *testing.T) {
	q := newEIOQueue(t)
	close(q.dev.startDone)
	noRescue(t)

	if got := q.holdUntilSafe(errors.New("commit failed")); got != holdResolved {
		t.Fatalf("holdUntilSafe = %q, want %q", got, holdResolved)
	}

	if isClosed(q.serving) {
		t.Fatal("a queue whose rescue failed reported that it serves")
	}

	if q.oldRing != nil {
		t.Fatal("failed rescue replaced the ring")
	}
}

// TestRescueDuringTeardownServesUntilStopped checks that a queue whose
// loop fails after teardown began, but before teardown stopped it,
// answers I/O through a rescue ring until it is stopped, since STOP_DEV
// may be waiting for a request it owns.
func TestRescueDuringTeardownServesUntilStopped(t *testing.T) {
	q := newEIOQueue(t)
	q.dev.closing.Store(true)

	done := make(chan struct{})

	go func() {
		defer close(done)

		q.rescueDuringTeardown(errors.New("ring broken during STOP_DEV"))
	}()

	select {
	case <-q.serving:
	case <-time.After(5 * time.Second):
		t.Fatal("queue failing during teardown never served")
	}

	select {
	case <-done:
		t.Fatal("rescue returned before the queue was stopped")
	case <-time.After(100 * time.Millisecond):
	}

	q.stop()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("rescue did not return after stop")
	}

	if q.oldRing == nil {
		t.Fatal("no rescue ring was set up")
	}
}

// TestRescueDuringTeardownSkipsStoppedQueue checks that a queue that
// teardown already stopped closes its ring without a rescue.
func TestRescueDuringTeardownSkipsStoppedQueue(t *testing.T) {
	q := newEIOQueue(t)
	q.dev.closing.Store(true)
	q.stop()

	testHooks.rescue = func(*queue) error {
		t.Error("stopped queue started a rescue")
		return errors.New("unexpected")
	}

	t.Cleanup(func() { testHooks.rescue = nil })

	q.rescueDuringTeardown(errors.New("ring broken after stop"))

	if q.oldRing != nil || isClosed(q.serving) {
		t.Fatal("stopped queue was rescued")
	}
}

// TestDrainKeepsCompletionsAfterPrepFailure fills a tiny ring and makes
// its submission fail, so drain fails partway through several finished
// handler results. Every result it could not prepare must stay pending
// with its tag still busy, so a rescue commits the real result through
// drain instead of losing it, and the sweep must not answer those tags.
func TestDrainKeepsCompletionsAfterPrepFailure(t *testing.T) {
	q := newEIOQueue(t)
	q.busy = make([]bool, q.depth)

	// A two-entry ring holds two commits; the third needs a submit,
	// which fails on a bad fd.
	small := newTestRing(t, 2)
	fd := small.fd
	small.fd = -1

	t.Cleanup(func() { small.fd = fd })

	q.ring = small

	for tag := range uint16(q.depth) {
		q.busy[tag] = true
		q.complete(tag, int32(100+tag))
	}

	if err := q.drain(); err == nil {
		t.Fatal("drain succeeded on a ring that cannot submit")
	}

	var committed []uint16
	for _, p := range prepared(small) {
		committed = append(committed, p.tag)
	}

	if want := []uint16{0, 1}; !slices.Equal(committed, want) {
		t.Fatalf("committed %v before the failure, want %v", committed, want)
	}

	for tag := range uint16(q.depth) {
		want := !slices.Contains(committed, tag)
		if q.busy[tag] != want {
			t.Errorf("tag %d busy = %v, want %v", tag, q.busy[tag], want)
		}
	}

	// A rescue now sweeps and drains on a working ring.
	q.ring = newTestRing(t, uint32(2*q.depth+2))

	if err := q.sweep(-int32(syscall.EIO)); err != nil {
		t.Fatal(err)
	}

	if err := q.drain(); err != nil {
		t.Fatal(err)
	}

	results := map[uint16][]int32{}
	for _, p := range prepared(q.ring) {
		results[p.tag] = append(results[p.tag], p.res)
	}

	for tag := range uint16(q.depth) {
		want := []int32{-int32(syscall.EIO)}
		if !slices.Contains(committed, tag) {
			want = []int32{int32(100 + tag)}
		}

		if !slices.Equal(results[tag], want) {
			t.Errorf("tag %d committed %v by the rescue, want %v", tag, results[tag], want)
		}
	}
}
