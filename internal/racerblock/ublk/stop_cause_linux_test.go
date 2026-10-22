// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"context"
	"errors"
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
