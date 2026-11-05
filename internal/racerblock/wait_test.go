// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racerblock

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

// fakeDevice is a runningDevice whose stop state is set by the test.
type fakeDevice struct {
	done     chan struct{}
	err      error
	closeErr error
	closed   int
	// onClose, if set, runs at the start of Close, before Close returns.
	onClose func(d *fakeDevice)
}

func newFakeDevice(stopped bool, err error) *fakeDevice {
	d := &fakeDevice{done: make(chan struct{}), err: err}
	if stopped {
		close(d.done)
	}

	return d
}

func (d *fakeDevice) Path() string          { return "/dev/ublkb0" }
func (d *fakeDevice) Done() <-chan struct{} { return d.done }
func (d *fakeDevice) Close() error {
	d.closed++
	if d.onClose != nil {
		d.onClose(d)
	}

	select {
	case <-d.done:
	default:
		close(d.done)
	}

	return d.closeErr
}

func (d *fakeDevice) Err() error {
	select {
	case <-d.done:
		return d.err
	default:
		return nil
	}
}

func TestWaitAndClose(t *testing.T) {
	errDevice := errors.New("queue failed")
	errClose := errors.New("delete failed")

	tests := []struct {
		name      string
		cancel    bool
		stopped   bool
		devErr    error
		closeErr  error
		wantNil   bool
		wantIs    error
		wantNotIs error
	}{
		{name: "cancel before device stops", cancel: true, wantNil: true},
		{name: "cancel and device stopped with ctx cause", cancel: true, stopped: true, devErr: context.Canceled, wantNil: true},
		{name: "cancel and device closed cleanly", cancel: true, stopped: true, wantNil: true},
		{name: "cancel and unrelated device failure", cancel: true, stopped: true, devErr: errDevice, wantIs: errDevice},
		{name: "cancel with close error", cancel: true, stopped: true, devErr: context.Canceled, closeErr: errClose, wantIs: errClose, wantNotIs: context.Canceled},
		{name: "device failure", stopped: true, devErr: errDevice, wantIs: errDevice},
		{name: "device failure wins over close error", stopped: true, devErr: errDevice, closeErr: errClose, wantIs: errDevice},
		{name: "device stopped without cause", stopped: true},
		{name: "cancel and joined ctx and device failure", cancel: true, stopped: true, devErr: errors.Join(context.Canceled, errDevice), wantIs: errDevice},
		{name: "device stopped with ctx error but ctx live", stopped: true, devErr: context.Canceled, wantIs: context.Canceled},
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			if tt.cancel {
				cancel()
			}

			dev := newFakeDevice(tt.stopped, tt.devErr)
			dev.closeErr = tt.closeErr

			err := waitAndClose(ctx, log, dev)
			if dev.closed != 1 {
				t.Fatalf("Close called %d times, want 1", dev.closed)
			}

			switch {
			case tt.wantNil:
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
			case err == nil:
				t.Fatal("err = nil, want error")
			}

			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Fatalf("err = %v, want %v", err, tt.wantIs)
			}

			if tt.wantNotIs != nil && errors.Is(err, tt.wantNotIs) {
				t.Fatalf("err = %v, must not wrap %v", err, tt.wantNotIs)
			}
		})
	}
}

func TestWaitAndCloseDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()

	<-ctx.Done()

	dev := newFakeDevice(true, context.DeadlineExceeded)
	if err := waitAndClose(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), dev); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}

// TestWaitAndCloseFailureVisibleDuringClose covers a teardown for an
// unrelated failure that is already running when ctx is canceled: Done is
// not closed yet, and the cause only becomes visible once Close returns.
func TestWaitAndCloseFailureVisibleDuringClose(t *testing.T) {
	errDevice := errors.New("queue failed")
	errClose := errors.New("delete failed")

	for _, closeErr := range []error{nil, errClose} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		dev := newFakeDevice(false, nil)
		dev.closeErr = closeErr
		dev.onClose = func(d *fakeDevice) { d.err = errDevice }

		err := waitAndClose(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), dev)
		if !errors.Is(err, errDevice) {
			t.Fatalf("closeErr %v: err = %v, want %v", closeErr, err, errDevice)
		}
	}
}

// TestWaitAndCloseCancelDuringClose covers cancellation teardown that
// finishes while Close runs: the ctx cause is still a clean stop.
func TestWaitAndCloseCancelDuringClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	dev := newFakeDevice(false, nil)
	dev.onClose = func(d *fakeDevice) { d.err = ctx.Err() }

	if err := waitAndClose(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), dev); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}
