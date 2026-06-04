// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

const (
	defaultStreamCommitProbeInterval   = 5 * time.Second
	defaultStreamCommitVerifyWindow    = 45 * time.Second
	defaultStreamCommitInventoryBudget = 5 * time.Second
)

type inventorySource interface {
	Inventory(ctx context.Context) ([]digest.Digest, error)
}

// streamCommitTracker correlates completed live stream-through
// responses with later containerd inventory observations. The mirror
// records "stream completed" immediately, while this tracker answers
// the distinct question "did the local containerd later show the
// digest as present within a bounded window?".
type streamCommitTracker struct {
	inv    inventorySource
	logger *slog.Logger

	probeInterval   time.Duration
	verifyWindow    time.Duration
	inventoryBudget time.Duration

	onObserved func(n int)
	onMissing  func(n int)

	mu      sync.Mutex
	pending map[string][]time.Time
}

func newStreamCommitTracker(inv inventorySource, logger *slog.Logger, onObserved, onMissing func(n int)) *streamCommitTracker {
	if logger == nil {
		logger = slog.Default()
	}

	return &streamCommitTracker{
		inv:             inv,
		logger:          logger.With(slog.String("subsystem", "stream_commit_tracker")),
		probeInterval:   defaultStreamCommitProbeInterval,
		verifyWindow:    defaultStreamCommitVerifyWindow,
		inventoryBudget: defaultStreamCommitInventoryBudget,
		onObserved:      onObserved,
		onMissing:       onMissing,
		pending:         map[string][]time.Time{},
	}
}

// RecordCompleted marks one fully completed live stream-through
// response for later inventory correlation.
func (t *streamCommitTracker) RecordCompleted(d digest.Digest) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.pending[d.String()] = append(t.pending[d.String()], time.Now().Add(t.verifyWindow))
}

func (t *streamCommitTracker) Run(ctx context.Context) error {
	ticker := time.NewTicker(t.probeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			t.probe(ctx)
		}
	}
}

func (t *streamCommitTracker) probe(parent context.Context) {
	t.mu.Lock()
	hasPending := len(t.pending) > 0
	t.mu.Unlock()

	if !hasPending || t.inv == nil {
		return
	}

	invCtx, cancel := context.WithTimeout(parent, t.inventoryBudget)
	digests, err := t.inv.Inventory(invCtx)

	cancel()

	if err != nil {
		var unavailable *ifaces.ErrUnavailable
		if errors.As(err, &unavailable) {
			t.logger.Debug("inventory unavailable during stream commit probe; keeping pending responses",
				slog.Any("err", err),
			)

			return
		}

		t.logger.Warn("inventory probe failed during stream commit correlation",
			slog.Any("err", err),
		)

		return
	}

	present := make(map[string]struct{}, len(digests))
	for _, d := range digests {
		present[d.String()] = struct{}{}
	}

	now := time.Now()
	observed := 0
	missing := 0

	t.mu.Lock()
	for ds, deadlines := range t.pending {
		if _, ok := present[ds]; ok {
			observed += len(deadlines)

			delete(t.pending, ds)

			continue
		}

		kept := make([]time.Time, 0, len(deadlines))
		for _, deadline := range deadlines {
			if now.After(deadline) || now.Equal(deadline) {
				missing++
				continue
			}

			kept = append(kept, deadline)
		}

		if len(kept) == 0 {
			delete(t.pending, ds)
			continue
		}

		t.pending[ds] = kept
	}
	t.mu.Unlock()

	if observed > 0 && t.onObserved != nil {
		t.onObserved(observed)
	}

	if missing > 0 && t.onMissing != nil {
		t.onMissing(missing)
	}
}
