// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
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

// startBounded runs Start in the background and fails the test if it
// does not return within startupTimeout.
func startBounded(t *testing.T) (*Device, error) {
	t.Helper()

	type result struct {
		d   *Device
		err error
	}

	res := make(chan result, 1)

	go func() {
		d, err := Start(context.Background(), Config{ID: -1, Size: 1 << 20, Queues: 2, Depth: 4}, newPatternHandler(1<<20))
		res <- result{d, err}
	}()

	select {
	case r := <-res:
		return r.d, r.err
	case <-time.After(startupTimeout):
		t.Fatal("Start did not return; START_DEV is likely waiting on a dead queue")
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
		testHooks.fetchTag = nil
		testHooks.beforeStartDev = nil
	})
}

// TestIntegrationStartupFetchRejected makes the kernel reject one
// FETCH_REQ of the last queue by fetching tag 0 twice, so that queue
// never becomes ready. Start must fail instead of issuing START_DEV,
// which would otherwise wait for that queue forever.
func TestIntegrationStartupFetchRejected(t *testing.T) {
	requireUblk(t)

	before := ublkNodes(t)

	setHooks(t, func(qid, tag int) int {
		if qid == 1 && tag == 3 {
			return 0
		}

		return tag
	}, func(*Device) {
		t.Error("reached START_DEV with a rejected FETCH_REQ")
	})

	d, err := startBounded(t)
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

// TestIntegrationStartupQueueFailsBeforeStartDev kills a ready queue
// thread through its real error path and waits for it to exit before
// START_DEV would be issued.
func TestIntegrationStartupQueueFailsBeforeStartDev(t *testing.T) {
	requireUblk(t)

	before := ublkNodes(t)
	boom := errors.New("injected queue failure")

	setHooks(t, nil, func(d *Device) {
		q := d.queues[0]
		q.fault.Store(&boom)
		q.wake()
		<-q.exited
	})

	d, err := startBounded(t)
	if err == nil {
		_ = d.Close()

		t.Fatal("Start succeeded with a dead queue")
	}

	if !errors.Is(err, boom) {
		t.Fatalf("Start = %v, want %v", err, boom)
	}

	checkNoLeak(t, before)
}
