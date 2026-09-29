// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	maxLiveConcurrency      = 256
	livePollInterval        = time.Second
	maxConcurrencyFileBytes = 64
)

func readConcurrency(path string) (value int, retErr error) {
	// Nonblocking open also lets us reject an accidentally configured FIFO.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return 0, err
	}

	defer func() {
		if err := f.Close(); retErr == nil && err != nil {
			retErr = err
		}
	}()

	info, err := f.Stat()
	if err != nil {
		return 0, err
	}

	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("concurrency file must be a regular file")
	}

	data, err := io.ReadAll(io.LimitReader(f, maxConcurrencyFileBytes+1))
	if err != nil {
		return 0, err
	}

	if len(data) > maxConcurrencyFileBytes {
		return 0, fmt.Errorf("concurrency file exceeds %d bytes", maxConcurrencyFileBytes)
	}

	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || n < 0 || n > maxLiveConcurrency {
		return 0, fmt.Errorf("concurrency file must contain one integer in [0, %d]", maxLiveConcurrency)
	}

	return n, nil
}

// liveWorkers creates slots lazily up to the largest applied limit (at most 256).
// Excess slots park instead of being replaced: even rapid oscillation during slow
// pulls cannot create overlapping generations or unbounded draining workers.
type liveWorkers struct {
	mu      sync.Mutex
	desired int
	changed chan struct{}
	workers sync.WaitGroup
	started int
}

func (w *liveWorkers) apply(ctx context.Context, p *puller, desired int) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.desired = desired
	close(w.changed)

	w.changed = make(chan struct{})
	for w.started < desired {
		id := w.started
		w.started++
		w.workers.Go(func() {
			var (
				traversal catalogTraversal
				next      time.Time
			)

			for w.admit(ctx, id, next) {
				delay := p.opts.Interval
				if err := p.pullImage(ctx, traversal.nextImage(p.images)); err != nil {
					delay = p.opts.RetryDelay
				}

				next = time.Now().Add(delay)
			}
		})
	}

	p.metrics.appliedConcurrency.Set(float64(desired))
}

// Admission and updates share a lock. Already admitted pulls finish under their
// existing pull timeout; shutdown cancels them through ctx and joins every slot.
func (w *liveWorkers) admit(ctx context.Context, id int, next time.Time) bool {
	for {
		w.mu.Lock()
		enabled := id < w.desired
		changed := w.changed
		ready := enabled && !time.Now().Before(next) && ctx.Err() == nil
		w.mu.Unlock()

		if ready {
			return true
		}

		var (
			timer *time.Timer
			tick  <-chan time.Time
		)

		if enabled {
			timer = time.NewTimer(time.Until(next))
			tick = timer.C
		}

		select {
		case <-ctx.Done():
		case <-changed:
		case <-tick:
		}

		if timer != nil {
			timer.Stop()
		}

		if ctx.Err() != nil {
			return false
		}
	}
}

func (p *puller) runLive(ctx context.Context, interval time.Duration) {
	pool := &liveWorkers{changed: make(chan struct{})}
	desired := p.opts.Concurrency
	lastError := ""
	initialized := false
	poll := func() {
		next, err := readConcurrency(p.opts.ConcurrencyFile)
		if err != nil {
			if err.Error() != lastError {
				slog.Warn("concurrency file rejected; retaining safe concurrency", "file", p.opts.ConcurrencyFile, "concurrency", desired, "error", err)
			}

			lastError = err.Error()
		} else {
			if lastError != "" {
				slog.Info("concurrency file recovered", "file", p.opts.ConcurrencyFile, "concurrency", next)
			}

			lastError = ""
		}

		if !initialized || (err == nil && next != desired) {
			if err == nil {
				desired = next
			}

			pool.apply(ctx, p, desired)

			initialized = true

			slog.Info("pull concurrency applied", "file", p.opts.ConcurrencyFile, "concurrency", desired, "max", maxLiveConcurrency)
		}
	}

	poll()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			pool.workers.Wait()
			return
		case <-ticker.C:
			poll()
		}
	}
}
