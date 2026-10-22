// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"context"
	"errors"
	"testing"
	"time"
)

// startupBlockHandler blocks every read until ctx is canceled and reports
// the first read it receives.
type startupBlockHandler struct{ entered chan struct{} }

func (b *startupBlockHandler) ReadAt(ctx context.Context, _ []byte, _ int64) error {
	select {
	case b.entered <- struct{}{}:
	default:
	}

	<-ctx.Done()

	return ctx.Err()
}

// TestIntegrationStartCancelDuringStartup cancels the context passed to
// Start while a handler is blocked. Kernels that scan partitions inside
// START_DEV block Start on that read, so cancellation must unblock it and
// Start must fail with the context error and leave nothing behind. On
// kernels that scan asynchronously Start returns first, and canceling must
// then tear the running device down.
func TestIntegrationStartCancelDuringStartup(t *testing.T) {
	requireFaultVM(t)

	before := ublkNodes(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type result struct {
		d   *Device
		err error
	}

	bh := &startupBlockHandler{entered: make(chan struct{}, 1)}
	res := make(chan result, 1)

	go func() {
		d, err := Start(ctx, Config{ID: -1, Size: 1 << 20, Queues: 1, Depth: 4}, bh)
		res <- result{d, err}
	}()

	var r result

	select {
	case <-bh.entered:
		// Start may or may not still be inside START_DEV here.
	case r = <-res:
		if r.err != nil {
			t.Fatalf("Start: %v", r.err)
		}

		t.Cleanup(func() { _ = r.d.Close() })
	case <-time.After(startupTimeout):
		t.Fatal("neither a startup read nor Start returned")
	}

	cancel()

	if r.d == nil {
		select {
		case r = <-res:
		case <-time.After(startupTimeout):
			t.Fatal("Start did not return after its context was canceled")
		}

		if r.err == nil {
			// The read arrived after START_DEV returned, so the device
			// started and the canceled context must stop it.
			t.Cleanup(func() { _ = r.d.Close() })
		} else if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("Start = %v, want context.Canceled", r.err)
		}
	}

	if r.d != nil {
		select {
		case <-r.d.Done():
		case <-time.After(startupTimeout):
			t.Fatal("device did not stop after ctx cancel")
		}

		if !errors.Is(r.d.Err(), context.Canceled) {
			t.Fatalf("Err = %v, want context.Canceled", r.d.Err())
		}

		if err := r.d.Close(); err != nil {
			t.Fatalf("Close after cancel: %v", err)
		}
	}

	checkNoLeak(t, before)
}

// TestIntegrationStartCancelBeforeStartDev cancels the context just before
// START_DEV is sent. Start must not report a started device: it must
// return context.Canceled and remove the device.
func TestIntegrationStartCancelBeforeStartDev(t *testing.T) {
	requireFaultVM(t)

	before := ublkNodes(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	setHooks(t, nil, nil)

	testHooks.beforeStartCmd = func(*Device) { cancel() }

	res := make(chan error, 1)

	go func() {
		d, err := Start(ctx, Config{ID: -1, Size: 1 << 20, Queues: 1, Depth: 4}, newPatternHandler(1<<20))
		if d != nil {
			_ = d.Close()
		}

		res <- err
	}()

	select {
	case err := <-res:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Start = %v, want context.Canceled", err)
		}
	case <-time.After(startupTimeout):
		t.Fatal("Start did not return after its context was canceled")
	}

	checkNoLeak(t, before)
}
