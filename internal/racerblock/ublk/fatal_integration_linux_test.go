// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// fatalTimeout bounds each wait in the fatal-ring tests. It only
// detects a hang; nothing is released when it fires.
const fatalTimeout = 30 * time.Second

// readAsync starts an O_DIRECT read of the first 4 KiB of path, so the
// kernel must hand a request to a queue, and returns its result.
func readAsync(t *testing.T, fd int) <-chan error {
	t.Helper()

	res := make(chan error, 1)

	go func() {
		_, err := unix.Pread(fd, alignedBuf(4096), 0)
		res <- err
	}()

	return res
}

// waitEIO waits for a read started by readAsync to fail with EIO.
func waitEIO(t *testing.T, res <-chan error) {
	t.Helper()

	select {
	case err := <-res:
		if !errors.Is(err, syscall.EIO) {
			t.Fatalf("read = %v, want EIO", err)
		}
	case <-time.After(fatalTimeout):
		abortHung(t, "the read handed to the failed queue was never answered")
	}
}

// waitSignal waits for c to be closed.
func waitSignal(t *testing.T, c <-chan struct{}, what string) {
	t.Helper()

	select {
	case <-c:
	case <-time.After(fatalTimeout):
		abortHung(t, what)
	}
}

// closeBounded closes d and fails the test if teardown blocks.
func closeBounded(t *testing.T, d *Device) error {
	t.Helper()

	res := make(chan error, 1)

	go func() { res <- d.Close() }()

	select {
	case err := <-res:
		return err
	case <-time.After(fatalTimeout):
		abortHung(t, "teardown blocked")
		return nil
	}
}

// inDelGendisk reports whether a thread of this process is in
// del_gendisk, which is where STOP_DEV waits for outstanding requests.
func inDelGendisk() bool {
	tasks, _ := filepath.Glob("/proc/self/task/[0-9]*/stack")
	for _, task := range tasks {
		stack, err := os.ReadFile(task)
		if err == nil && strings.Contains(string(stack), "del_gendisk") {
			return true
		}
	}

	return false
}

// TestIntegrationFatalRingWithDeliveredRequest fails a queue's ring
// right after the kernel handed it a read, so the request is owned by
// this process and was never answered. The hook runs only for a
// completion that delivered a request, so the request is handed to
// userspace before the failure by construction. Linux 7.0 does not
// abort such a request when the ring is closed, so STOP_DEV would wait
// for it forever; the rescue ring must answer it with EIO, after which
// the device must tear down and be deleted as usual.
func TestIntegrationFatalRingWithDeliveredRequest(t *testing.T) {
	requireFaultVM(t)

	before := ublkNodes(t)
	boom := errors.New("injected fatal ring with a delivered request")

	setHooks(t, nil, nil)

	var armed, fired atomic.Bool

	delivered := make(chan struct{})
	rescued := make(chan struct{})

	testHooks.delivered = func(_ *queue, _ uint16, eio bool) error {
		if eio || !armed.Load() || fired.Swap(true) {
			return nil
		}

		close(delivered)

		return boom
	}
	testHooks.rescued = func(*queue) { close(rescued) }

	closes := recordRingCloses(t)

	d, err := startBounded(t, 1)
	if err != nil {
		t.Fatal(err)
	}

	fd, err := unix.Open(d.Path(), unix.O_RDONLY|unix.O_DIRECT|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = d.Close()

		t.Fatal(err)
	}

	armed.Store(true)

	res := readAsync(t, fd)

	waitSignal(t, delivered, "no request was handed to the queue")
	waitSignal(t, rescued, "the failed queue never started a rescue")
	waitEIO(t, res)

	if err := unix.Close(fd); err != nil {
		t.Error(err)
	}

	waitSignal(t, d.Done(), "the failed device never stopped")

	if !errors.Is(d.Err(), boom) {
		t.Fatalf("Err = %v, want %v", d.Err(), boom)
	}

	if err := closeBounded(t, d); err != nil {
		t.Fatalf("Close = %v", err)
	}

	checkClosedSafely(t, closes(), holdRescued)
	checkNoLeak(t, before)
}

// TestIntegrationFatalRingDuringStopDev fails a queue's EIO loop while
// it owns a request and STOP_DEV is already waiting for that request in
// del_gendisk. The queue first fails servably and serves EIO, so
// teardown issues STOP_DEV. The hooks hold STOP_DEV until a read has
// been handed to the EIO loop, then keep that request unanswered until
// a thread of this process is seen in del_gendisk, and only then break
// the loop. The rescue ring must answer the request so STOP_DEV, and
// teardown, finish.
func TestIntegrationFatalRingDuringStopDev(t *testing.T) {
	requireFaultVM(t)

	before := ublkNodes(t)
	fault := errors.New("injected servable queue failure")
	boom := errors.New("injected EIO loop failure during STOP_DEV")

	setHooks(t, nil, nil)

	var fired atomic.Bool

	delivered := make(chan struct{})
	stopping := make(chan struct{})
	sawStop := make(chan bool, 1)

	testHooks.delivered = func(_ *queue, _ uint16, eio bool) error {
		if !eio || fired.Swap(true) {
			return nil
		}

		close(delivered)

		// Teardown sends STOP_DEV once delivered is closed. Wait until
		// it is blocked on this request before failing.
		<-stopping

		deadline := time.Now().Add(fatalTimeout)
		for !inDelGendisk() && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}

		sawStop <- inDelGendisk()

		return boom
	}
	testHooks.beforeStop = func(*Device) {
		<-delivered
		close(stopping)
	}

	d, err := startBounded(t, 1)
	if err != nil {
		t.Fatal(err)
	}

	fd, err := unix.Open(d.Path(), unix.O_RDONLY|unix.O_DIRECT|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = d.Close()

		t.Fatal(err)
	}

	injectFault(d.queues[0], fault)
	waitSignal(t, d.queues[0].serving, "the failed queue never served EIO")

	res := readAsync(t, fd)

	waitSignal(t, delivered, "no request was handed to the EIO loop")
	waitEIO(t, res)

	if err := unix.Close(fd); err != nil {
		t.Error(err)
	}

	select {
	case ok := <-sawStop:
		if !ok {
			t.Error("STOP_DEV was never seen waiting in del_gendisk")
		}
	case <-time.After(fatalTimeout):
		abortHung(t, "the EIO loop was never failed")
	}

	waitSignal(t, d.Done(), "teardown never finished")

	if err := closeBounded(t, d); err != nil {
		t.Fatalf("Close = %v", err)
	}

	if !errors.Is(d.Err(), fault) {
		t.Fatalf("Err = %v, want %v", d.Err(), fault)
	}

	checkNoLeak(t, before)
}

// TestIntegrationFatalRingDuringTeardown fails a healthy queue's ring
// after Close began, while the queue owns a read and STOP_DEV is seen
// waiting for it in del_gendisk. The error arrives after teardown
// marked the device closing, so it is not a startup or runtime failure,
// but the queue must still answer the read through a rescue ring so
// STOP_DEV and Close finish.
func TestIntegrationFatalRingDuringTeardown(t *testing.T) {
	requireFaultVM(t)

	before := ublkNodes(t)
	boom := errors.New("injected ring failure during teardown")

	setHooks(t, nil, nil)

	var armed, fired atomic.Bool

	tearing := make(chan struct{})
	delivered := make(chan struct{})
	sawStop := make(chan bool, 1)

	testHooks.beforeStop = func(*Device) {
		armed.Store(true)
		close(tearing)
		<-delivered
	}
	testHooks.delivered = func(q *queue, _ uint16, eio bool) error {
		if eio || !armed.Load() || fired.Swap(true) {
			return nil
		}

		if !q.dev.closing.Load() {
			t.Error("request delivered before teardown began")
		}

		close(delivered)

		deadline := time.Now().Add(fatalTimeout)
		for !inDelGendisk() && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}

		sawStop <- inDelGendisk()

		return boom
	}

	d, err := startBounded(t, 1)
	if err != nil {
		t.Fatal(err)
	}

	fd, err := unix.Open(d.Path(), unix.O_RDONLY|unix.O_DIRECT|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = d.Close()

		t.Fatal(err)
	}

	closed := make(chan error, 1)

	go func() { closed <- d.Close() }()

	waitSignal(t, tearing, "teardown never reached STOP_DEV")

	res := readAsync(t, fd)

	waitSignal(t, delivered, "no request was handed to the queue during teardown")
	waitEIO(t, res)

	if err := unix.Close(fd); err != nil {
		t.Error(err)
	}

	select {
	case ok := <-sawStop:
		if !ok {
			t.Error("STOP_DEV was never seen waiting in del_gendisk")
		}
	case <-time.After(fatalTimeout):
		abortHung(t, "the ring was never failed")
	}

	select {
	case err := <-closed:
		// Whether the late error is reported is decided by the queue
		// error recording, not by this test.
		t.Logf("Close = %v", err)
	case <-time.After(fatalTimeout):
		abortHung(t, "Close blocked after a ring failure during teardown")
	}

	checkNoLeak(t, before)
}

// TestIntegrationStartFailsWithoutSpareRing checks that Start fails
// before START_DEV, and leaves no device behind, when a queue cannot
// reserve its rescue ring.
func TestIntegrationStartFailsWithoutSpareRing(t *testing.T) {
	requireFaultVM(t)

	before := ublkNodes(t)
	boom := errors.New("injected rescue ring reservation failure")

	setHooks(t, nil, nil)

	testHooks.spareRing = func(q *queue) error {
		if q.id == 1 {
			return boom
		}

		return nil
	}
	testHooks.beforeStartCmd = func(*Device) {
		t.Error("START_DEV sent without every rescue ring reserved")
	}

	d, err := startBounded(t, 2)
	if err == nil {
		_ = d.Close()

		t.Fatal("Start succeeded without a rescue ring")
	}

	if !errors.Is(err, boom) {
		t.Fatalf("Start = %v, want %v", err, boom)
	}

	checkNoLeak(t, before)
}
