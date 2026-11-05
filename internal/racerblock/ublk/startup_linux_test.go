// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
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

// TestHoldUntilSafeReturnsAfterStartup checks that a queue that cannot
// serve does not hold its ring once startup is resolved. A servable
// failure instead serves until stopped; see
// TestHoldUntilSafeServesUntilStopped.
func TestHoldUntilSafeReturnsAfterStartup(t *testing.T) {
	d, _, _ := newStartupDevice(t)
	close(d.startDone)

	q := &queue{dev: d}

	noRescue(t)

	finished := make(chan string, 1)

	go func() {
		finished <- q.holdUntilSafe(errors.New("x"))
	}()

	select {
	case got := <-finished:
		if got != holdResolved {
			t.Fatalf("holdUntilSafe = %q, want %q", got, holdResolved)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("holdUntilSafe blocked after startup was resolved")
	}
}

func TestWaitSafe(t *testing.T) {
	never := func() (bool, error) { return false, nil }
	noWarn := func(string, ...any) {}

	t.Run("resolved", func(t *testing.T) {
		done := make(chan struct{})

		go func() {
			time.Sleep(20 * time.Millisecond)
			close(done)
		}()

		if got := waitSafe(done, never, time.Minute, noWarn); got != holdResolved {
			t.Fatalf("got %q, want %q", got, holdResolved)
		}
	})

	t.Run("published", func(t *testing.T) {
		var calls atomic.Int32

		probe := func() (bool, error) { return calls.Add(1) >= 3, nil }

		if got := waitSafe(make(chan struct{}), probe, time.Minute, noWarn); got != holdPublished {
			t.Fatalf("got %q, want %q", got, holdPublished)
		}
	})

	t.Run("probe errors do not end the wait early", func(t *testing.T) {
		done := make(chan struct{})
		probeErr := errors.New("GET_DEV_INFO failed")

		var calls atomic.Int32

		probe := func() (bool, error) {
			if calls.Add(1) == 3 {
				close(done)
			}

			return false, probeErr
		}

		if got := waitSafe(done, probe, time.Minute, noWarn); got != holdResolved {
			t.Fatalf("got %q, want %q", got, holdResolved)
		}

		if calls.Load() < 3 {
			t.Fatalf("returned after %d probes, before startup resolved", calls.Load())
		}
	})

	t.Run("timeout warns and keeps holding", func(t *testing.T) {
		done := make(chan struct{})
		warned := make(chan struct{}, 4)
		warn := func(string, ...any) { warned <- struct{}{} }
		result := make(chan string, 1)

		go func() { result <- waitSafe(done, never, 20*time.Millisecond, warn) }()

		select {
		case <-warned:
		case <-time.After(5 * time.Second):
			t.Fatal("no warning after the warn interval")
		}

		select {
		case got := <-result:
			t.Fatalf("waitSafe released the ring after the warning: %q", got)
		case <-time.After(100 * time.Millisecond):
		}

		if len(warned) != 0 {
			t.Fatal("warned more than once")
		}

		close(done)

		select {
		case got := <-result:
			if got != holdResolved {
				t.Fatalf("got %q, want %q", got, holdResolved)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("waitSafe did not return after startup resolved")
		}
	})
}

func TestDiskPublished(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "ublkb7")

	if err := os.Mkdir(present, 0o755); err != nil {
		t.Fatal(err)
	}

	missing := filepath.Join(dir, "ublkb8")

	for _, tc := range []struct {
		name  string
		state uint16
		path  string
		want  bool
	}{
		{"live and registered", ublkSDevLive, present, true},
		{"live without block device", ublkSDevLive, missing, false},
		{"registered but not live", 0, present, false},
		{"neither", 0, missing, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := diskPublished(tc.state, tc.path)
			if err != nil {
				t.Fatal(err)
			}

			if got != tc.want {
				t.Fatalf("diskPublished = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("stat error is not published", func(t *testing.T) {
		// A path through a regular file fails with ENOTDIR, not ENOENT.
		file := filepath.Join(dir, "file")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}

		got, err := diskPublished(ublkSDevLive, filepath.Join(file, "x"))
		if err == nil || got {
			t.Fatalf("diskPublished = %v, %v, want false and an error", got, err)
		}
	})
}

// TestHoldUntilSafeBrokenWaitsForPublication checks that a failure which
// may have lost a request skips the EIO loop and holds until the probe
// reports the disk published, ignoring error and LIVE-only results.
func TestHoldUntilSafeBrokenWaitsForPublication(t *testing.T) {
	d, _, _ := newStartupDevice(t)
	d.cfg.Logger = slog.New(slog.DiscardHandler)
	q := &queue{dev: d}

	noRescue(t)

	var calls atomic.Int32

	testHooks.published = func() (bool, error) {
		switch calls.Add(1) {
		case 1:
			return false, errors.New("control device unavailable")
		case 2, 3:
			return false, nil // LIVE without a registered block device
		default:
			return true, nil
		}
	}
	testHooks.serveEIO = func(*queue) error {
		t.Error("EIO loop used after a failure that may have lost a request")
		return nil
	}

	t.Cleanup(func() {
		testHooks.published = nil
		testHooks.serveEIO = nil
	})

	if got := q.holdUntilSafe(errors.New("commit failed")); got != holdPublished {
		t.Fatalf("holdUntilSafe = %q, want %q", got, holdPublished)
	}

	if calls.Load() < 4 {
		t.Fatalf("released after %d probes, before publication", calls.Load())
	}

	if isClosed(d.startDone) {
		t.Fatal("startup unexpectedly resolved")
	}
}

// TestHoldUntilSafeServableFallsBackWhenBroken checks that a servable
// failure whose EIO loop fails takes the broken path and still waits
// for publication.
func TestHoldUntilSafeServableFallsBackWhenBroken(t *testing.T) {
	d, _, _ := newStartupDevice(t)
	d.cfg.Logger = slog.New(slog.DiscardHandler)
	q := &queue{dev: d}

	noRescue(t)

	var (
		eioCalls atomic.Int32
		probes   atomic.Int32
	)

	testHooks.serveEIO = func(*queue) error {
		eioCalls.Add(1)
		return errors.New("ring broken")
	}
	testHooks.published = func() (bool, error) { return probes.Add(1) >= 2, nil }

	t.Cleanup(func() {
		testHooks.published = nil
		testHooks.serveEIO = nil
	})

	if got := q.holdUntilSafe(errServable{errors.New("fault")}); got != holdPublished {
		t.Fatalf("holdUntilSafe = %q, want %q", got, holdPublished)
	}

	if eioCalls.Load() != 1 || probes.Load() < 2 {
		t.Fatalf("eio=%d probes=%d, want EIO attempt then probing", eioCalls.Load(), probes.Load())
	}
}
