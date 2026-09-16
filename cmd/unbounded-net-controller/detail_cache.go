// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"k8s.io/utils/clock"

	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

// nodeDetailSnapshot carries immutable details, separate from routine status.
// Status and all its nested data must remain read-only, including for callers
// retaining a returned snapshot after its cache entry expires.
type nodeDetailSnapshot struct {
	statusv1alpha1.NodeDetailSnapshot

	legacyRevision uint64
	peerIdentity   *peerIdentityDigest
}

// nodeDetailCache is a leader-local, TTL-only store. It owns no second result
// history or per-entry timers. TTL bounds retention time, not peak memory.
// Construct it with newNodeDetailCache and run one Run loop for proactive expiry.
type nodeDetailCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	clock   clock.Clock // May be replaced in tests before concurrent use.
	entries map[string]nodeDetailSnapshot
	changed chan struct{}
	running bool
}

func newNodeDetailCache(ttl time.Duration) (*nodeDetailCache, error) {
	if ttl <= 0 {
		return nil, errors.New("node detail cache TTL must be positive")
	}

	return &nodeDetailCache{
		ttl:     ttl,
		clock:   clock.RealClock{},
		entries: make(map[string]nodeDetailSnapshot),
		changed: make(chan struct{}, 1),
	}, nil
}

// Store accepts actual detailed data only; callers must not pass summaries or
// failed fetches. It shallow-copies status without modifying it. Nested slices,
// maps, and pointers remain shared and must not be mutated by the caller.
// Only Store renews the receipt-based TTL; request validation belongs upstream.
func (c *nodeDetailCache) Store(nodeName, requestID string, collectedAt time.Time, status *NodeStatusResponse) (nodeDetailSnapshot, error) {
	return c.store(nodeName, requestID, collectedAt, status, 0, nil, nil)
}

func (c *nodeDetailCache) store(nodeName, requestID string, collectedAt time.Time, status *NodeStatusResponse, revision uint64, identity *peerIdentityDigest, expected *NodeStatusResponse) (nodeDetailSnapshot, error) {
	if nodeName == "" {
		return nodeDetailSnapshot{}, errors.New("node detail cache requires a node name")
	}

	if status == nil {
		return nodeDetailSnapshot{}, errors.New("node detail cache requires detailed status")
	}

	statusCopy := *status

	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.clock.Now()
	if expected != nil {
		previous, ok := c.entries[nodeName]
		if !ok || previous.Status != expected || !now.Before(previous.ExpiresAt) {
			return nodeDetailSnapshot{}, errors.New("legacy detail base changed or expired")
		}
	}

	snapshot := nodeDetailSnapshot{
		NodeDetailSnapshot: statusv1alpha1.NodeDetailSnapshot{
			NodeName: nodeName, RequestID: requestID, CollectedAt: collectedAt,
			ReceivedAt: now, ExpiresAt: now.Add(c.ttl), Status: &statusCopy,
		},
		legacyRevision: revision,
		peerIdentity:   identity,
	}
	c.entries[nodeName] = snapshot
	c.notify()

	return snapshot, nil
}

// Get does not refresh TTL. At the deadline it drops the cache's ownership,
// even when the proactive expiry loop has not yet been scheduled.
func (c *nodeDetailCache) Get(nodeName string) (nodeDetailSnapshot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	snapshot, ok := c.entries[nodeName]
	if !ok {
		return nodeDetailSnapshot{}, false
	}

	if !c.clock.Now().Before(snapshot.ExpiresAt) {
		delete(c.entries, nodeName)
		c.notify()

		return nodeDetailSnapshot{}, false
	}

	return snapshot, true
}

func (c *nodeDetailCache) Delete(nodeName string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.entries, nodeName)
	c.notify()
}

// Clear releases all cache-owned details without mutating shared payloads.
// It does not stop Run; subsequent stores can be expired by the same loop.
func (c *nodeDetailCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	clear(c.entries)
	c.notify()
}

// Expire removes entries at or past their deadline and returns their count.
func (c *nodeDetailCache) Expire() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	removed, _ := c.expireLocked(c.clock.Now())
	c.notify()

	return removed
}

func (c *nodeDetailCache) expireLocked(now time.Time) (int, time.Time) {
	removed := 0

	var next time.Time

	for name, snapshot := range c.entries {
		if !now.Before(snapshot.ExpiresAt) {
			delete(c.entries, name)

			removed++
		} else if next.IsZero() || snapshot.ExpiresAt.Before(next) {
			next = snapshot.ExpiresAt
		}
	}

	return removed, next
}

func (c *nodeDetailCache) notify() {
	select {
	case c.changed <- struct{}{}:
	default:
	}
}

// Run blocks until cancellation, then stops its timer and clears all entries.
// Wait for Run to return before restarting it or storing for a new leadership
// term. Concurrent Run calls are rejected; no goroutine is started internally.
func (c *nodeDetailCache) Run(ctx context.Context) error {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()

		return errors.New("node detail cache expiry loop is already running")
	}

	c.running = true
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()

		clear(c.entries)
		c.running = false
	}()

	for ctx.Err() == nil {
		c.mu.Lock()
		_, next := c.expireLocked(c.clock.Now())
		c.mu.Unlock()

		var (
			timer  clock.Timer
			timerC <-chan time.Time
		)

		if !next.IsZero() {
			timer = c.clock.NewTimer(next.Sub(c.clock.Now()))
			timerC = timer.C()
		}

		select {
		case <-ctx.Done():
		case <-c.changed:
		case <-timerC:
		}

		if timer != nil {
			timer.Stop()
		}
	}

	return nil
}
