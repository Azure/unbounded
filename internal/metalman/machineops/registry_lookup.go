// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"context"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/Azure/unbounded/internal/metalman/netboot"
)

// DefaultReconcileLookupTimeout bounds how long one reconcile waits for a
// registry-backed lookup (HTTP boot URL resolution or boot media
// preparation).
const DefaultReconcileLookupTimeout = 10 * time.Second

func (r *Reconciler) reconcileLookupTimeout() time.Duration {
	if r.ReconcileLookupTimeout > 0 {
		return r.ReconcileLookupTimeout
	}

	return DefaultReconcileLookupTimeout
}

// retainedLookupTTL bounds how long a finished lookup result waits for a
// reconcile to consume it. Operations that never come back (deleted, or
// moved past the phase) must not pin results forever.
const retainedLookupTTL = 15 * time.Minute

// lookupGroup deduplicates detached lookups and retains a finished result
// until a caller consumes it. Without retention, a call that finishes between
// reconciles is forgotten, and a slow lower layer can keep exceeding the
// reconcile timeout even though the work already completed once.
type lookupGroup struct {
	flights singleflight.Group

	mu   sync.Mutex
	done map[string]retainedLookup
	// now is replaceable in tests. Nil uses time.Now.
	now func() time.Time
}

type retainedLookup struct {
	val     any
	expires time.Time
}

func (g *lookupGroup) clock() time.Time {
	if g.now != nil {
		return g.now()
	}

	return time.Now()
}

// store keeps a finished result for key and prunes expired entries.
func (g *lookupGroup) store(key string, val any) {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.clock()
	if g.done == nil {
		g.done = map[string]retainedLookup{}
	}

	for k, v := range g.done {
		if !now.Before(v.expires) {
			delete(g.done, k)
		}
	}

	g.done[key] = retainedLookup{val: val, expires: now.Add(retainedLookupTTL)}
}

// take removes and returns the unexpired retained result for key.
func (g *lookupGroup) take(key string) (any, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	v, ok := g.done[key]
	if !ok {
		return nil, false
	}

	delete(g.done, key)

	if !g.clock().Before(v.expires) {
		return nil, false
	}

	return v.val, true
}

// forget drops any retained result for key, for example when the media it
// describes is released.
func (g *lookupGroup) forget(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	delete(g.done, key)
}

// boundedLookup runs fn detached from ctx, deduplicated by key, and waits for
// it for at most timeout. The controller runs a single reconcile worker, so
// a stalled registry must not hold it: on timeout boundedLookup returns an
// error wrapping netboot.ErrImageUnavailable, which callers treat as "wait and
// requeue", while fn keeps running. A later call with the same key joins the
// running call, or, once it finished successfully, consumes its retained
// result without calling fn again. Failed calls are not retained.
// fn must bound its own run time.
func boundedLookup[T any](ctx context.Context, group *lookupGroup, key string, timeout time.Duration, fn func(context.Context) (T, error)) (T, error) {
	var zero T

	if v, ok := group.take(key); ok {
		if t, ok := v.(T); ok {
			return t, nil
		}
	}

	detached := context.WithoutCancel(ctx)
	ch := group.flights.DoChan(key, func() (any, error) {
		v, err := fn(detached)
		if err == nil {
			group.store(key, v)
		}

		return v, err
	})

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case <-timer.C:
		return zero, fmt.Errorf("%w: registry lookup still in progress after %s", netboot.ErrImageUnavailable, timeout)
	case res := <-ch:
		if res.Err != nil {
			return zero, res.Err
		}

		// This caller consumes the result, so later calls start fresh.
		group.forget(key)

		v, _ := res.Val.(T) //nolint:errcheck // the flight only returns T

		return v, nil
	}
}
