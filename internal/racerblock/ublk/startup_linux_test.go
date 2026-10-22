// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"errors"
	"sync"
	"testing"
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
// thread before later queues launch, concurrently with the launch loop,
// and checks every queue is launched and no teardown happens until
// startup returns. Run with -race to check the startup fields.
func TestEarlyQueueErrorDuringLaunch(t *testing.T) {
	d, stops, mu := newStartupDevice(t)
	for i := range 4 {
		d.queues = append(d.queues, &queue{dev: d, id: i, exited: make(chan struct{})})
	}

	boom := errors.New("fetch failed")

	var wg sync.WaitGroup

	launched := 0
	d.launch = func(q *queue, ready chan<- error) {
		launched++

		wg.Add(1)

		go func() {
			defer wg.Done()
			defer close(q.exited)

			if q.id == 0 {
				d.fail(boom)

				ready <- boom

				return
			}

			ready <- nil
		}()
	}

	err := d.runStartup(func() error {
		ready := make(chan error, len(d.queues))
		for _, q := range d.queues {
			q.running = true
			d.launch(q, ready)
		}

		for range d.queues {
			<-ready
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
