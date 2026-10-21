// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"log/slog"
	"os"
	"sync"
)

// drainFiles hands each concurrent unverified read its own /dev/null file.
//
// Every splice pump into an *os.File takes that file's write lock
// (internal/poll.splicePump), so a single /dev/null shared by all workers
// serializes their drains. With 32 streams of 16 MiB pages over a Unix
// socket, one shared file delivered about 140 Gb/s while one file per stream
// delivered about 370 Gb/s, so a shared sink capped the benchmark below the
// cache. Files are reused, so the number open tracks peak concurrency.
type drainFiles struct {
	mu     sync.Mutex
	free   []*os.File
	closed bool
}

// newDrainFiles opens the first drain file up front so a missing /dev/null
// fails at startup rather than on the first unverified read.
func newDrainFiles() (*drainFiles, error) {
	d := &drainFiles{}

	f, err := openDrainFile()
	if err != nil {
		return nil, err
	}

	d.free = append(d.free, f)

	return d, nil
}

func openDrainFile() (*os.File, error) {
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("open drain destination: %w", err)
	}

	return f, nil
}

// get returns an idle drain file, opening another when all are in use.
func (d *drainFiles) get() (*os.File, error) {
	d.mu.Lock()

	if d.closed {
		d.mu.Unlock()
		return nil, fmt.Errorf("drain destination: %w", os.ErrClosed)
	}

	if n := len(d.free); n > 0 {
		f := d.free[n-1]
		d.free = d.free[:n-1]
		d.mu.Unlock()

		return f, nil
	}

	d.mu.Unlock()

	return openDrainFile()
}

// put returns a file from get. Files returned after close are closed.
func (d *drainFiles) put(f *os.File) {
	d.mu.Lock()

	if !d.closed {
		d.free = append(d.free, f)
		d.mu.Unlock()

		return
	}

	d.mu.Unlock()
	closeDrainFile(f)
}

// close closes idle files now and files still in use when they are put back.
func (d *drainFiles) close() {
	d.mu.Lock()
	d.closed = true
	free := d.free
	d.free = nil
	d.mu.Unlock()

	for _, f := range free {
		closeDrainFile(f)
	}
}

func closeDrainFile(f *os.File) {
	if err := f.Close(); err != nil {
		slog.Warn("close drain destination", "error", err)
	}
}
