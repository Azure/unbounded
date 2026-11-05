// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
)

// leaveStartup ends the startup phase of a test device.
func leaveStartup(t *testing.T, d *Device) {
	t.Helper()

	if err := d.runStartup(func() error { return nil }); err != nil {
		t.Fatalf("runStartup = %v", err)
	}
}

func TestFailRecordedBeforeTeardownIsScheduled(t *testing.T) {
	d, _, _ := newStartupDevice(t)
	leaveStartup(t, d)

	queueErr := errors.New("queue 0 died")
	scheduled := make(chan struct{})
	release := make(chan struct{})

	// Hold the failure's teardown so cancellation or Close wins it.
	d.stopAsync = func(error) {
		close(scheduled)
		<-release
	}

	go d.fail(queueErr)

	<-scheduled

	tests := []struct {
		name  string
		cause error
	}{
		{"close", nil},
		{"canceled", context.Canceled},
		{"deadline", context.DeadlineExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := d.stopCause(tt.cause); !errors.Is(got, queueErr) {
				t.Fatalf("stopCause(%v) = %v, want queue failure", tt.cause, got)
			}
		})
	}

	close(release)
}

func TestStopCauseKeepsFirstFailure(t *testing.T) {
	d, _, _ := newStartupDevice(t)
	leaveStartup(t, d)

	first := errors.New("queue 0 died")

	d.fail(first)
	// A failure reported while teardown is draining queues is late; the
	// first one stays the cause.
	d.closing.Store(true)
	d.fail(errors.New("queue 1 died"))

	if got := d.stopCause(nil); !errors.Is(got, first) {
		t.Fatalf("stopCause(nil) = %v, want first failure", got)
	}
}

func TestStopCauseLateFailureDuringClose(t *testing.T) {
	d, _, _ := newStartupDevice(t)
	leaveStartup(t, d)

	// Close starts teardown before any queue fails, then a queue fails
	// before it exits. The failure is still reported.
	d.closing.Store(true)

	late := errors.New("queue died during teardown")
	d.fail(late)

	if got := d.stopCause(nil); !errors.Is(got, late) {
		t.Fatalf("stopCause(nil) = %v, want late failure", got)
	}
}

func TestStopCauseKeepsNonContextCause(t *testing.T) {
	d, _, _ := newStartupDevice(t)
	leaveStartup(t, d)

	d.fail(errors.New("queue died"))

	if got := d.stopCause(errStoppedExternally); !errors.Is(got, errStoppedExternally) {
		t.Fatalf("stopCause = %v, want explicit cause", got)
	}
}

func TestStopCauseWithoutFailure(t *testing.T) {
	d, _, _ := newStartupDevice(t)
	leaveStartup(t, d)

	if got := d.stopCause(nil); got != nil {
		t.Fatalf("stopCause(nil) = %v, want nil", got)
	}

	if got := d.stopCause(context.Canceled); !errors.Is(got, context.Canceled) {
		t.Fatalf("stopCause = %v, want context.Canceled", got)
	}
}

func TestStartupFailureNotStopCause(t *testing.T) {
	d, _, _ := newStartupDevice(t)
	startErr := errors.New("queue died during startup")

	d.fail(startErr)
	_ = d.runStartup(func() error { return nil })

	// Startup failures are returned by Start, not recorded as failErr.
	if got := d.stopCause(nil); got != nil {
		t.Fatalf("stopCause(nil) = %v, want nil", got)
	}
}

func TestStopCauseJoinedContextKeepsFailure(t *testing.T) {
	d, _, _ := newStartupDevice(t)
	leaveStartup(t, d)

	queueErr := errors.New("queue died")
	extra := errors.New("handler failed")

	d.fail(queueErr)

	tests := []struct {
		name  string
		cause error
	}{
		{"joined", errors.Join(context.Canceled, extra)},
		{"wrapped", fmt.Errorf("stop: %w: %w", context.Canceled, extra)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := d.stopCause(tt.cause)
			if !errors.Is(got, queueErr) || !errors.Is(got, extra) {
				t.Fatalf("stopCause = %v, want queue failure and %v", got, extra)
			}
		})
	}
}

func TestStopCauseWrappedContextWithoutFailure(t *testing.T) {
	d, _, _ := newStartupDevice(t)
	leaveStartup(t, d)

	extra := errors.New("handler failed")
	cause := errors.Join(context.Canceled, extra)

	if got := d.stopCause(cause); !errors.Is(got, extra) {
		t.Fatalf("stopCause = %v, want %v kept", got, extra)
	}
}

func TestStopCauseSecondFailureWinsTeardown(t *testing.T) {
	d, _, _ := newStartupDevice(t)
	leaveStartup(t, d)

	first := errors.New("queue 0 died")
	second := errors.New("queue 1 died")

	d.fail(first)
	d.fail(second)

	// The second failure's teardown runs first; both are kept and the
	// first reported failure leads.
	got := d.stopCause(second)
	if !errors.Is(got, first) || !errors.Is(got, second) {
		t.Fatalf("stopCause(second) = %v, want both failures", got)
	}

	if got := d.stopCause(first); got != first { //nolint:errorlint // identity is the contract
		t.Fatalf("stopCause(first) = %v, want first unchanged", got)
	}
}

// newTeardownDevice returns a started-up Device whose control channel is
// already closed, so teardown runs end to end without kernel access:
// control commands fail before any syscall and no queues exist.
func newTeardownDevice(t *testing.T, ctx context.Context) (*Device, chan struct{}, chan struct{}) {
	t.Helper()

	d := &Device{
		cfg:    Config{Logger: slog.New(slog.DiscardHandler)},
		id:     7,
		ctrl:   &ctrl{fd: -1},
		charFd: -1,
		done:   make(chan struct{}),
	}
	d.init()
	d.hctx, d.hcancel = context.WithCancel(ctx)
	leaveStartup(t, d)

	scheduled := make(chan struct{}, 4)
	release := make(chan struct{})

	// Hold every failure's teardown until the test releases it.
	d.stopAsync = func(err error) {
		scheduled <- struct{}{}

		go func() {
			<-release
			d.teardown(err)
		}()
	}

	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

	return d, scheduled, release
}

func TestCloseWinsTeardownKeepsFailure(t *testing.T) {
	d, scheduled, release := newTeardownDevice(t, context.Background())
	queueErr := errors.New("queue 0 died")

	d.fail(queueErr)
	<-scheduled

	closeErr := d.Close()
	// The fixture never opened a char device, so teardown leaves the
	// id alone and reports that.
	if !errors.Is(closeErr, errUnpinnedLeft) {
		t.Fatalf("Close = %v, want fixture device left in place", closeErr)
	}

	if err := d.Err(); !errors.Is(err, queueErr) {
		t.Fatalf("Err = %v, want queue failure", err)
	}

	close(release)

	if err := d.Err(); !errors.Is(err, queueErr) {
		t.Fatalf("Err after delayed teardown = %v, want queue failure", err)
	}
}

func TestCancelWinsTeardownKeepsFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d, scheduled, _ := newTeardownDevice(t, ctx)
	queueErr := errors.New("queue 0 died")

	d.fail(queueErr)
	<-scheduled

	go d.watch(ctx)

	cancel()
	<-d.Done()

	if err := d.Err(); !errors.Is(err, queueErr) || errors.Is(err, context.Canceled) {
		t.Fatalf("Err = %v, want queue failure only", err)
	}
}

func TestLateFailureDuringTeardownKept(t *testing.T) {
	d, scheduled, _ := newTeardownDevice(t, context.Background())
	late := errors.New("queue died during teardown")

	// A handler that fails only once teardown cancels it. teardown
	// waits for it in workers.Wait before it records the cause.
	d.workers.Add(1)

	go func() {
		defer d.workers.Done()

		<-d.hctx.Done()
		d.fail(late)
	}()

	_ = d.Close()

	<-scheduled

	if err := d.Err(); !errors.Is(err, late) {
		t.Fatalf("Err = %v, want late failure", err)
	}
}
