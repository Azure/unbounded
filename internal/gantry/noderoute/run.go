// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package noderoute

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Run continuously reconciles the projected desired-registry file until the
// context is cancelled. Configuration errors are fatal so Kubernetes surfaces
// them through the standalone node-config DaemonSet rollout.
func Run(ctx context.Context, options Options, interval time.Duration) error {
	if interval <= 0 {
		return fmt.Errorf("reconcile interval must be positive, got %s", interval)
	}

	reconcile := func() error {
		desired, err := LoadConfig(options.DesiredPath)
		if err != nil {
			return err
		}

		return Reconcile(ctx, options, desired)
	}

	if err := reconcile(); err != nil {
		return err
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	return runReconcileLoop(ctx, ticker.C, reconcile, func(err error) {
		slog.Error("standalone Gantry node-route reconcile failed; retrying",
			"retry_after", interval,
			"err", err,
		)
	})
}

func runReconcileLoop(ctx context.Context, ticks <-chan time.Time, reconcile func() error, onError func(error)) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticks:
			if err := reconcile(); err != nil {
				onError(err)
			}
		}
	}
}

// CheckDesired loads the projected desired state and verifies host convergence.
func CheckDesired(options Options) error {
	desired, err := LoadConfig(options.DesiredPath)
	if err != nil {
		return err
	}

	return Check(options, desired)
}
