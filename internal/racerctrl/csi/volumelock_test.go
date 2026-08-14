// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package csi

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// Two volumes are independent. This is the property the node service is built
// on: a call that waits on one volume must not hold up a call on another.
func TestVolumeLocksDoNotBlockAcrossVolumes(t *testing.T) {
	locks := newVolumeLocks()

	releaseFirst := locks.lock("pv-1")
	defer releaseFirst()

	done := make(chan struct{})

	go func() {
		defer close(done)

		locks.lock("pv-2")()
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("locking one volume blocked locking another")
	}
}

// The same volume serialises, because the publish sequence is a read followed
// by a write and two of them interleaving would stack mounts.
func TestVolumeLocksSerialiseOneVolume(t *testing.T) {
	locks := newVolumeLocks()

	release := locks.lock("pv-1")

	acquired := make(chan struct{})

	go func() {
		defer close(acquired)

		locks.lock("pv-1")()
	}()

	select {
	case <-acquired:
		t.Fatal("a second holder acquired a lock that was still held")
	case <-time.After(50 * time.Millisecond):
	}

	release()

	select {
	case <-acquired:
	case <-time.After(10 * time.Second):
		t.Fatal("releasing the lock did not hand it to the waiter")
	}
}

// Releasing every holder must leave nothing behind. A node stages and unstages
// volumes for as long as it runs, so an entry that outlived its holders would
// be an unbounded leak keyed by volume name.
func TestVolumeLocksReleaseEveryEntry(t *testing.T) {
	locks := newVolumeLocks()

	for i := range 64 {
		locks.lock(fmt.Sprintf("pv-%d", i))()
	}

	if held := locks.held(); held != 0 {
		t.Fatalf("expected no entries after every release, got %d", held)
	}
}

// A queued waiter keeps the map entry alive, and the reference count is what
// makes that true. Releasing the last holder must not delete an entry somebody
// is still parked on, because the next caller to look the volume up would miss
// it, build a second lock for the same volume, and run beside the waiter.
//
// The late arriver below is the whole test. A version that deletes on every
// release rather than at zero references passes every other test in this file,
// since waiters that queued before the release already hold the entry pointer
// and hand off among themselves correctly. Only a caller that arrives after
// the release, while a waiter still holds the lock, can see the difference.
func TestVolumeLocksSurviveAHandoff(t *testing.T) {
	locks := newVolumeLocks()

	release := locks.lock("pv-1")

	waiterHolding := make(chan struct{})
	waiterDone := make(chan struct{})

	go func() {
		defer close(waiterDone)

		done := locks.lock("pv-1")
		defer done()

		close(waiterHolding)
		time.Sleep(500 * time.Millisecond)
	}()

	// Let the waiter queue on the entry before the holder lets go, so what
	// follows is a handoff rather than an uncontended acquisition.
	time.Sleep(20 * time.Millisecond)
	release()
	<-waiterHolding

	late := make(chan struct{})

	go func() {
		defer close(late)

		locks.lock("pv-1")()
	}()

	select {
	case <-late:
		t.Fatal("a caller arriving after the handoff acquired a lock the waiter still held")
	case <-time.After(100 * time.Millisecond):
	}

	<-waiterDone

	select {
	case <-late:
	case <-time.After(10 * time.Second):
		t.Fatal("the late caller never acquired the lock the waiter released")
	}

	if held := locks.held(); held != 0 {
		t.Fatalf("expected no entries after every release, got %d", held)
	}
}

// Many callers contending for one volume must still see one holder at a time.
// This is the property NodePublishVolume leans on: the kubelet retries freely,
// and a bind of the same source onto the same target succeeds and stacks
// another mount rather than failing, so two publishes of one volume
// interleaving would pile mounts up rather than announce themselves.
func TestVolumeLocksExcludeUnderContention(t *testing.T) {
	locks := newVolumeLocks()

	release := locks.lock("pv-1")

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		live int
		peak int
	)

	for range 8 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			done := locks.lock("pv-1")
			defer done()

			mu.Lock()
			live++

			if live > peak {
				peak = live
			}
			mu.Unlock()

			time.Sleep(time.Millisecond)

			mu.Lock()
			live--
			mu.Unlock()
		}()
	}

	// Let the contenders queue before the holder lets go, so they are
	// contending rather than acquiring an idle lock one after another.
	time.Sleep(20 * time.Millisecond)
	release()
	wg.Wait()

	if peak != 1 {
		t.Fatalf("expected one holder at a time, saw %d", peak)
	}

	if held := locks.held(); held != 0 {
		t.Fatalf("expected no entries after every release, got %d", held)
	}
}

// The lock is keyed by volume, and volume ids are PersistentVolume names, so

// an empty id is not a key worth special-casing - but it must not panic or
// alias every other volume either.
func TestVolumeLocksHandleAnEmptyVolumeID(t *testing.T) {
	locks := newVolumeLocks()

	release := locks.lock("")

	done := make(chan struct{})

	go func() {
		defer close(done)

		locks.lock("pv-1")()
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("an empty volume id blocked a named volume")
	}

	release()

	if held := locks.held(); held != 0 {
		t.Fatalf("expected no entries after every release, got %d", held)
	}
}
