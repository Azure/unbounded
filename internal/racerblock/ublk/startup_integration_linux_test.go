// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// startupTimeout bounds every faulty Start. Without the startup checks
// START_DEV would block forever on a queue that never becomes ready.
const startupTimeout = 30 * time.Second

func ublkNodes(t *testing.T) []string {
	t.Helper()

	m, err := filepath.Glob("/dev/ublk[bc]*")
	if err != nil {
		t.Fatal(err)
	}

	slices.Sort(m)

	return m
}

// startBounded runs Start with the given number of queues in the
// background and fails the test if it does not return within
// startupTimeout.
func startBounded(t *testing.T, queues int) (*Device, error) {
	t.Helper()

	type result struct {
		d   *Device
		err error
	}

	res := make(chan result, 1)

	go func() {
		d, err := Start(context.Background(), Config{ID: -1, Size: 1 << 20, Queues: queues, Depth: 4}, newPatternHandler(1<<20))
		res <- result{d, err}
	}()

	select {
	case r := <-res:
		return r.d, r.err
	case <-time.After(startupTimeout):
		abortHung(t, "Start did not return; see the stacks for where it is blocked")
		return nil, nil
	}
}

// checkNoLeak waits briefly for the kernel to remove nodes and compares
// the ublk device nodes with before.
func checkNoLeak(t *testing.T, before []string) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for {
		after := ublkNodes(t)
		if slices.Equal(before, after) {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("ublk nodes leaked: before %v, after %v", before, after)
		}

		time.Sleep(20 * time.Millisecond)
	}
}

func setHooks(t *testing.T, fetchTag func(qid, tag int) int, beforeStartDev func(*Device)) {
	t.Helper()

	testHooks.fetchTag = fetchTag
	testHooks.beforeStartDev = beforeStartDev

	t.Cleanup(func() {
		// A hung device still runs queue goroutines that read the
		// hooks; see abortHung.
		if hung.Load() {
			return
		}

		testHooks.fetchTag = nil
		testHooks.beforeStartDev = nil
		testHooks.beforeStartCmd = nil
		testHooks.failedRingClose = nil
		testHooks.serveEIO = nil
		testHooks.eioAnswered = nil
		testHooks.delivered = nil
		testHooks.rescue = nil
		testHooks.rescued = nil
		testHooks.beforeStop = nil
	})
}

// requireFaultVM skips startup fault tests unless RACER_BLOCK_TEST_VM=1.
// Killing a queue around START_DEV has oopsed Linux 7.0 hosts, so these
// tests only run in a disposable VM.
func requireFaultVM(t *testing.T) {
	t.Helper()
	requireUblk(t)

	if os.Getenv("RACER_BLOCK_TEST_VM") != "1" {
		t.Skip("startup fault tests need RACER_BLOCK_TEST_VM=1 in a disposable VM")
	}
}

// injectFault makes q fail through its real error path.
func injectFault(q *queue, err error) {
	q.fault.Store(&err)
	q.wake()
}

// waitRecorded waits until a queue error has been recorded for startup.
func waitRecorded(t *testing.T, d *Device) {
	t.Helper()

	deadline := time.Now().Add(startupTimeout)
	for d.startupErr() == nil {
		if time.Now().After(deadline) {
			t.Error("queue failure was not recorded")
			return
		}

		time.Sleep(time.Millisecond)
	}
}

// recordRingCloses records why each failed queue stopped holding its
// ring.
func recordRingCloses(t *testing.T) func() []string {
	t.Helper()

	var (
		mu     sync.Mutex
		closes []string
	)

	testHooks.failedRingClose = func(_ *queue, reason string) {
		mu.Lock()

		closes = append(closes, reason)
		mu.Unlock()
	}

	return func() []string {
		mu.Lock()
		defer mu.Unlock()

		return slices.Clone(closes)
	}
}

// checkClosedSafely checks that every failed queue closed its ring for
// one of the allowed reasons.
func checkClosedSafely(t *testing.T, closes []string, allowed ...string) {
	t.Helper()

	if len(closes) == 0 {
		t.Fatal("failed queue never closed its ring")
	}

	for i, reason := range closes {
		if !slices.Contains(allowed, reason) {
			t.Errorf("failed queue ring %d closed with reason %q, want one of %v", i, reason, allowed)
		}
	}
}

// TestIntegrationStartupFetchRejected makes the kernel reject one
// FETCH_REQ of the last queue by fetching tag 0 twice, so that queue
// never becomes ready. Start must fail instead of issuing START_DEV,
// which would otherwise wait for that queue forever.
func TestIntegrationStartupFetchRejected(t *testing.T) {
	requireFaultVM(t)

	before := ublkNodes(t)

	setHooks(t, func(qid, tag int) int {
		if qid == 1 && tag == 3 {
			return 0
		}

		return tag
	}, func(*Device) {
		t.Error("reached START_DEV with a rejected FETCH_REQ")
	})

	d, err := startBounded(t, 2)
	if err == nil {
		_ = d.Close()

		t.Fatal("Start succeeded with a rejected FETCH_REQ")
	}

	// Linux 6.8 rejects a repeated FETCH_REQ with EBUSY, newer kernels
	// with EINVAL.
	if !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("Start = %v, want FETCH_REQ rejection", err)
	}

	checkNoLeak(t, before)
}

// TestIntegrationStartupQueueFailsBeforeStartDev fails a ready queue
// through its real error path before the last startup check. Start must
// skip START_DEV, and the failed queue must keep its ring until
// teardown stops it.
func TestIntegrationStartupQueueFailsBeforeStartDev(t *testing.T) {
	requireFaultVM(t)

	before := ublkNodes(t)
	boom := errors.New("injected queue failure")

	setHooks(t, nil, func(d *Device) {
		injectFault(d.queues[0], boom)
		// The queue now holds its ring until teardown stops it, so wait
		// for the recorded error rather than for the queue to exit.
		waitRecorded(t, d)
	})

	testHooks.beforeStartCmd = func(*Device) {
		t.Error("START_DEV issued after a queue failed")
	}

	closes := recordRingCloses(t)

	d, err := startBounded(t, 2)
	if err == nil {
		_ = d.Close()

		t.Fatal("Start succeeded with a dead queue")
	}

	if !errors.Is(err, boom) {
		t.Fatalf("Start = %v, want %v", err, boom)
	}

	checkClosedSafely(t, closes(), holdStopped)
	checkNoLeak(t, before)
}

// TestIntegrationStartupQueueFailsAtStartDev fails a ready queue after
// the last startup check, just before START_DEV is sent, so START_DEV
// runs with a failed queue. The device has a single queue so all
// startup I/O, including a partition scan, reaches the failed one. The
// queue must answer that I/O with EIO until teardown stops it, which is
// after START_DEV and STOP_DEV return, and Start must return the
// recorded error.
func TestIntegrationStartupQueueFailsAtStartDev(t *testing.T) {
	requireFaultVM(t)

	before := ublkNodes(t)
	boom := errors.New("injected queue failure at START_DEV")

	setHooks(t, nil, nil)

	testHooks.beforeStartCmd = func(d *Device) {
		injectFault(d.queues[0], boom)
		waitRecorded(t, d)
	}

	closes := recordRingCloses(t)

	d, err := startBounded(t, 1)
	if err == nil {
		_ = d.Close()

		t.Fatal("Start succeeded with a failed queue")
	}

	if !errors.Is(err, boom) {
		t.Fatalf("Start = %v, want %v", err, boom)
	}

	got := closes()
	checkClosedSafely(t, got, holdStopped)
	t.Logf("failed queue released its ring because: %v", got)
	checkNoLeak(t, before)
}

// TestIntegrationStartupFailedQueueAnswersAfterStartup fails the only
// queue just before START_DEV and pauses it after it prepares its first
// EIO answer until startup resolves. Linux 7.0 scans partitions from a
// work item, so START_DEV returns while that read waits on the paused
// queue. The answer must still reach the kernel: a request left with
// the queue is only aborted when the char device is released, and
// teardown's STOP_DEV waits for it before that. Linux 6.8 scans inside
// START_DEV, so there the pause ends after holdPause instead.
func TestIntegrationStartupFailedQueueAnswersAfterStartup(t *testing.T) {
	requireFaultVM(t)

	const holdPause = time.Second

	before := ublkNodes(t)
	boom := errors.New("injected queue failure at START_DEV")

	setHooks(t, nil, nil)

	var paused atomic.Bool

	testHooks.eioAnswered = func(q *queue) {
		if paused.Swap(true) {
			return
		}

		select {
		case <-q.dev.startDone:
		case <-time.After(holdPause):
		}
	}
	testHooks.beforeStartCmd = func(d *Device) {
		injectFault(d.queues[0], boom)
		waitRecorded(t, d)
	}

	closes := recordRingCloses(t)

	d, err := startBounded(t, 1)
	if err == nil {
		_ = d.Close()

		t.Fatal("Start succeeded with a failed queue")
	}

	if !errors.Is(err, boom) {
		t.Fatalf("Start = %v, want %v", err, boom)
	}

	if !paused.Load() {
		t.Fatal("the failed queue answered no request")
	}

	got := closes()
	checkClosedSafely(t, got, holdStopped)
	t.Logf("failed queue released its ring because: %v", got)
	checkNoLeak(t, before)
}

// TestIntegrationStartupBrokenRingAtStartDev fails a ready queue just
// before START_DEV and makes its EIO loop fail on entry, so the queue
// cannot answer I/O on its own ring. The device has a single queue, so
// no healthy queue can serve the partition scan instead. The queue must
// answer the scan through a rescue ring: Linux 6.8 scans inside
// START_DEV, and Linux 7.0 scans from a work item whose read, once
// handed to the queue, STOP_DEV would otherwise wait for forever. The
// rescue keeps the original ring open until teardown stops the queue,
// so no ring is closed before the disk is published. The broken-ring
// fallback behind a failed rescue, and its publication probe, are
// covered by TestHoldUntilSafeBrokenWaitsForPublication,
// TestRescueFailureFallsBackToBrokenPath and
// TestIntegrationPublishedProbe.
func TestIntegrationStartupBrokenRingAtStartDev(t *testing.T) {
	requireFaultVM(t)

	before := ublkNodes(t)
	boom := errors.New("injected queue failure at START_DEV")
	broken := errors.New("injected broken ring")

	setHooks(t, nil, nil)

	testHooks.serveEIO = func(*queue) error { return broken }
	testHooks.beforeStartCmd = func(d *Device) {
		injectFault(d.queues[0], boom)
		waitRecorded(t, d)
	}

	closes := recordRingCloses(t)

	d, err := startBounded(t, 1)
	if err == nil {
		_ = d.Close()

		t.Fatal("Start succeeded with a failed queue")
	}

	if !errors.Is(err, boom) {
		t.Fatalf("Start = %v, want %v", err, boom)
	}

	got := closes()
	checkClosedSafely(t, got, holdRescued)
	t.Logf("failed queue released its ring because: %v", got)
	checkNoLeak(t, before)
}

// TestIntegrationPublishedProbe checks the publication probe that a
// queue whose rescue failed relies on before it closes its ring: it
// must not report a disk before START_DEV and must report it once Start
// has returned.
func TestIntegrationPublishedProbe(t *testing.T) {
	requireFaultVM(t)

	before := ublkNodes(t)

	setHooks(t, nil, nil)

	var early []bool

	testHooks.beforeStartCmd = func(d *Device) {
		probe, closeProbe := d.publishedProbe()
		defer closeProbe()

		ok, err := probe()
		if err != nil {
			t.Errorf("probe before START_DEV: %v", err)
		}

		early = append(early, ok)
	}

	d, err := startBounded(t, 1)
	if err != nil {
		t.Fatal(err)
	}

	probe, closeProbe := d.publishedProbe()

	ok, err := probe()

	closeProbe()

	if cerr := d.Close(); cerr != nil {
		t.Error(cerr)
	}

	if err != nil || !ok {
		t.Fatalf("probe after Start = %v, %v; want true", ok, err)
	}

	if len(early) != 1 || early[0] {
		t.Fatalf("probe before START_DEV = %v, want one false", early)
	}

	checkNoLeak(t, before)
}
