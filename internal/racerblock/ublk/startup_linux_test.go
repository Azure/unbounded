// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newStartupDevice returns a Device in the startup phase whose teardown
// is recorded instead of touching the kernel.
func newStartupDevice(t *testing.T) (*Device, *[]error, *sync.Mutex) {
	t.Helper()

	d := &Device{charFd: -1, done: make(chan struct{})}
	d.init()

	var (
		mu    sync.Mutex
		stops []error
	)

	d.stopAsync = func(err error) {
		mu.Lock()

		stops = append(stops, err)
		mu.Unlock()
	}

	return d, &stops, &mu
}

func TestFailDuringStartupDefersTeardown(t *testing.T) {
	d, stops, mu := newStartupDevice(t)
	first := errors.New("queue 0 died")

	d.fail(first)
	d.fail(errors.New("queue 1 died"))

	mu.Lock()
	n := len(*stops)
	mu.Unlock()

	if n != 0 {
		t.Fatalf("teardown ran %d times during startup, want 0", n)
	}

	if err := d.runStartup(func() error { return nil }); !errors.Is(err, first) {
		t.Fatalf("runStartup = %v, want first recorded error", err)
	}

	if d.starting {
		t.Fatal("still in startup phase")
	}
}

func TestStartupErrorPrefersStartError(t *testing.T) {
	d, _, _ := newStartupDevice(t)
	startErr := errors.New("START_DEV failed")

	err := d.runStartup(func() error {
		d.fail(errors.New("queue died during START_DEV"))
		return startErr
	})
	if !errors.Is(err, startErr) {
		t.Fatalf("runStartup = %v, want start error", err)
	}
}

func TestFailAfterStartupTearsDown(t *testing.T) {
	d, stops, mu := newStartupDevice(t)

	if err := d.runStartup(func() error { return nil }); err != nil {
		t.Fatalf("runStartup: %v", err)
	}

	cause := errors.New("late queue error")
	d.fail(cause)

	mu.Lock()
	defer mu.Unlock()

	if len(*stops) != 1 || !errors.Is((*stops)[0], cause) {
		t.Fatalf("stops = %v, want [%v]", *stops, cause)
	}
}

// TestEarlyQueueErrorDuringLaunch injects a failure from the first queue
// thread while launchQueues, the real launch loop, is still launching
// later queues. Every queue must be launched and no teardown may happen
// until startup returns. Run with -race to check the startup fields.
func TestEarlyQueueErrorDuringLaunch(t *testing.T) {
	d, stops, mu := newStartupDevice(t)
	for i := range 4 {
		d.queues = append(d.queues, &queue{dev: d, id: i, exited: make(chan struct{})})
	}

	boom := errors.New("queue died after fetch")

	var (
		wg       sync.WaitGroup
		launched int
	)

	d.launch = func(q *queue, ready chan<- error) {
		launched++

		if q.id == 0 {
			// Fail synchronously so the error is recorded before the
			// remaining queues launch.
			d.fail(boom)
		}

		wg.Add(1)

		go func() {
			defer wg.Done()
			defer close(q.exited)

			ready <- nil
		}()
	}

	err := d.runStartup(func() error {
		if err := d.launchQueues(); err != nil {
			return err
		}

		return d.startupErr()
	})

	wg.Wait()

	if !errors.Is(err, boom) {
		t.Fatalf("runStartup = %v, want %v", err, boom)
	}

	if launched != len(d.queues) {
		t.Fatalf("launched %d queues, want %d", launched, len(d.queues))
	}

	for _, q := range d.queues {
		if !q.running {
			t.Fatalf("queue %d not marked running", q.id)
		}
	}

	mu.Lock()
	defer mu.Unlock()

	if len(*stops) != 0 {
		t.Fatalf("async teardown ran during startup: %v", *stops)
	}
}

func TestLaunchQueuesJoinsFetchErrors(t *testing.T) {
	d, _, _ := newStartupDevice(t)
	for i := range 3 {
		d.queues = append(d.queues, &queue{dev: d, id: i})
	}

	rejected := errors.New("FETCH_REQ rejected")
	d.launch = func(q *queue, ready chan<- error) {
		go func() {
			if q.id == 1 {
				ready <- rejected
				return
			}

			ready <- nil
		}()
	}

	if err := d.launchQueues(); !errors.Is(err, rejected) {
		t.Fatalf("launchQueues = %v, want %v", err, rejected)
	}
}

func TestRunStartupResolvesAfterStart(t *testing.T) {
	d, _, _ := newStartupDevice(t)

	err := d.runStartup(func() error {
		if isClosed(d.startDone) {
			t.Error("startup resolved while start was running")
		}

		d.fail(errors.New("queue died"))

		return nil
	})
	if err == nil {
		t.Fatal("runStartup lost the recorded error")
	}

	if !isClosed(d.startDone) {
		t.Fatal("startup not resolved after start returned")
	}
}

func TestHoldUntilSafeReturnsAfterStartup(t *testing.T) {
	d, _, _ := newStartupDevice(t)
	close(d.startDone)

	q := &queue{dev: d}

	finished := make(chan struct{})

	go func() {
		q.holdUntilSafe()
		close(finished)
	}()

	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("holdUntilSafe blocked after startup was resolved")
	}
}

func TestWaitStartupOrLive(t *testing.T) {
	never := func() (bool, error) { return false, nil }
	noWarn := func(string, ...any) {}

	t.Run("resolved", func(t *testing.T) {
		done := make(chan struct{})

		go func() {
			time.Sleep(20 * time.Millisecond)
			close(done)
		}()

		if got := waitStartupOrLive(done, never, time.Minute, noWarn); got != "resolved" {
			t.Fatalf("got %q, want resolved", got)
		}
	})

	t.Run("live", func(t *testing.T) {
		var calls atomic.Int32

		live := func() (bool, error) { return calls.Add(1) >= 3, nil }

		if got := waitStartupOrLive(make(chan struct{}), live, time.Minute, noWarn); got != "live" {
			t.Fatalf("got %q, want live", got)
		}
	})

	t.Run("probe errors do not end the wait early", func(t *testing.T) {
		done := make(chan struct{})
		probeErr := errors.New("GET_DEV_INFO failed")

		var calls atomic.Int32

		live := func() (bool, error) {
			if calls.Add(1) == 3 {
				close(done)
			}

			return false, probeErr
		}

		if got := waitStartupOrLive(done, live, time.Minute, noWarn); got != "resolved" {
			t.Fatalf("got %q, want resolved", got)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		warned := false
		warn := func(string, ...any) { warned = true }

		if got := waitStartupOrLive(make(chan struct{}), never, 30*time.Millisecond, warn); got != "timeout" {
			t.Fatalf("got %q, want timeout", got)
		}

		if !warned {
			t.Fatal("timeout was not logged")
		}
	})
}
