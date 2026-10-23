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
