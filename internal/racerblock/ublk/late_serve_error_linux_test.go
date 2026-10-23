// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"errors"
	"sync"
	"testing"
)

// runLateStop runs q.run on a queue whose io commands go to /dev/null,
// so serve fails on its initial FETCH_REQs. setStop runs after serve
// returns and before run checks for a local stop. It returns the errors
// passed to the device's teardown.
func runLateStop(t *testing.T, setStop func(q *queue)) []error {
	t.Helper()

	q := newEIOQueue(t)
	leaveStartup(t, q.dev)

	var (
		mu    sync.Mutex
		stops []error
	)

	q.dev.stopAsync = func(err error) {
		mu.Lock()

		stops = append(stops, err)
		mu.Unlock()
	}

	var serveErr error

	testHooks.afterServe = func(q *queue, err error) {
		serveErr = err

		setStop(q)
	}

	t.Cleanup(func() { testHooks.afterServe = nil })

	ready := make(chan error, 1)
	q.run(ready)

	if err := <-ready; err == nil {
		t.Fatal("FETCH_REQ on /dev/null succeeded, want an error")
	}

	if serveErr == nil {
		t.Fatal("serve returned nil, want the FETCH_REQ error")
	}

	if q.failed.Load() {
		t.Fatal("queue marked failed after a local stop, want no ring hold")
	}

	mu.Lock()
	defer mu.Unlock()

	return append([]error(nil), stops...)
}

func TestServeErrorRecordedWhenCloseWinsAfterServe(t *testing.T) {
	var q0 *queue

	// Closing alone leaves the queue unstopped, so run rescues it until
	// teardown's STOP_DEV lands. Simulate that STOP once the rescue has
	// answered its first sweep.
	rescued := false
	testHooks.rescued = func(q *queue) {
		rescued = true

		q.stop()
	}

	t.Cleanup(func() { testHooks.rescued = nil })

	defer func() {
		if !rescued {
			t.Error("queue was not rescued during teardown")
		}
	}()

	stops := runLateStop(t, func(q *queue) {
		q0 = q
		q.dev.closing.Store(true)
	})

	if len(stops) != 1 || stops[0] == nil {
		t.Fatalf("teardown causes = %v, want one serve failure", stops)
	}

	if got := q0.dev.stopCause(nil); got == nil || !errors.Is(got, stops[0]) {
		t.Fatalf("stopCause(nil) = %v, want serve failure", got)
	}
}

func TestServeErrorRecordedWhenStopWinsAfterServe(t *testing.T) {
	stops := runLateStop(t, func(q *queue) { q.stopping.Store(true) })

	if len(stops) != 1 || stops[0] == nil {
		t.Fatalf("teardown causes = %v, want one serve failure", stops)
	}
}

// TestServeErrorHoldDecidedBeforeTeardown checks that a queue's own
// failure marks it failed and holds its ring even when the teardown it
// schedules starts a local shutdown immediately.
func TestServeErrorHoldDecidedBeforeTeardown(t *testing.T) {
	q := newEIOQueue(t)
	leaveStartup(t, q.dev)

	var failedAtStop []bool

	q.dev.stopAsync = func(error) {
		failedAtStop = append(failedAtStop, q.failed.Load())
		// Teardown starts at once, as if it won the scheduler.
		q.dev.closing.Store(true)
	}

	var reasons []string

	testHooks.serveEIO = func(*queue) error { return errors.New("forced broken ring") }
	testHooks.failedRingClose = func(_ *queue, reason string) { reasons = append(reasons, reason) }
	// The held queue rescues its requests until teardown stops it.
	// Simulate the STOP that teardown sends once the rescue is serving.
	testHooks.rescued = func(q *queue) { q.stop() }

	t.Cleanup(func() {
		testHooks.serveEIO = nil
		testHooks.failedRingClose = nil
		testHooks.rescued = nil
	})

	ready := make(chan error, 1)
	q.run(ready)

	if err := <-ready; err == nil {
		t.Fatal("FETCH_REQ on /dev/null succeeded, want an error")
	}

	if len(failedAtStop) != 1 || !failedAtStop[0] {
		t.Fatalf("failed at teardown = %v, want [true]", failedAtStop)
	}

	if len(reasons) != 1 {
		t.Fatalf("held ring closes = %v, want one hold", reasons)
	}
}
