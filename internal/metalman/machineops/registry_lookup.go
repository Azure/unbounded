// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"context"
	"fmt"
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

// boundedLookup runs fn detached from ctx, deduplicated by key, and waits for
// it for at most timeout. The controller runs a single reconcile worker, so
// a stalled registry must not hold it: on timeout boundedLookup returns an
// error wrapping netboot.ErrImageUnavailable, which callers treat as "wait and
// requeue", while fn keeps running. A later call with the same key joins the
// running call, or, once it finished, calls fn again, which then finds the
// result in the lower layer (the metadata memo or the built ISO on disk).
// fn must bound its own run time.
func boundedLookup[T any](ctx context.Context, group *singleflight.Group, key string, timeout time.Duration, fn func(context.Context) (T, error)) (T, error) {
	var zero T

	detached := context.WithoutCancel(ctx)
	ch := group.DoChan(key, func() (any, error) {
		return fn(detached)
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

		v, _ := res.Val.(T) //nolint:errcheck // the flight only returns T

		return v, nil
	}
}
