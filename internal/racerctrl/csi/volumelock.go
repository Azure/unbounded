// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package csi

import "sync"

// volumeLocks serializes node service calls per volume.
//
// The CSI spec lets the kubelet issue node RPCs for different volumes
// concurrently and retry calls for the same volume freely, so the driver needs
// mutual exclusion within a volume and none at all between volumes. A single
// driver-wide mutex supplies both, but it also parks every call on the node
// behind whichever stage happens to be running, and a stage waits for a ublk
// device to appear: that wait is bounded by RACER_STAGE_TIMEOUT, not by
// anything the caller controls. On a node where many pods share one volume,
// and on a node whose first volume in a universe is paying for a fabric
// attach, that turns one slow volume into a slow node.
//
// Entries are reference counted and dropped when the last holder releases, so
// the map holds the locks in flight rather than every volume the node has ever
// staged.
type volumeLocks struct {
	mu    sync.Mutex
	locks map[string]*volumeLock
}

type volumeLock struct {
	mu   sync.Mutex
	refs int
}

func newVolumeLocks() *volumeLocks {
	return &volumeLocks{locks: make(map[string]*volumeLock)}
}

// lock acquires the lock for one volume and returns the function that releases
// it. The returned function must be called exactly once, which is what a defer
// at the head of an RPC gives.
func (v *volumeLocks) lock(volume string) func() {
	v.mu.Lock()

	entry, ok := v.locks[volume]
	if !ok {
		entry = &volumeLock{}
		v.locks[volume] = entry
	}

	// Count the waiter before dropping the map lock. The entry has to outlive
	// every goroutine queued on it, or a release could delete it from under a
	// waiter and the next caller would queue on a different mutex.
	entry.refs++

	v.mu.Unlock()

	entry.mu.Lock()

	return func() { v.unlock(volume, entry) }
}

func (v *volumeLocks) unlock(volume string, entry *volumeLock) {
	entry.mu.Unlock()

	v.mu.Lock()
	defer v.mu.Unlock()

	entry.refs--
	if entry.refs == 0 {
		delete(v.locks, volume)
	}
}

// held reports how many volumes have a lock outstanding. Only tests use it,
// to check that entries are not accumulating.
func (v *volumeLocks) held() int {
	v.mu.Lock()
	defer v.mu.Unlock()

	return len(v.locks)
}
