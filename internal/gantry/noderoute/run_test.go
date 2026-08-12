// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package noderoute

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunReconcileLoopRetriesAfterError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

	ticks := make(chan time.Time, 2)
	ticks <- time.Now()

	ticks <- time.Now()

	attempts := 0
	reported := 0

	err := runReconcileLoop(ctx, ticks, func() error {
		attempts++
		if attempts == 1 {
			return errors.New("transient host reset")
		}

		cancel()

		return nil
	}, func(error) {
		reported++
	})
	if err != nil {
		t.Fatalf("runReconcileLoop: %v", err)
	}

	if attempts != 2 || reported != 1 {
		t.Fatalf("attempts/reported = %d/%d, want 2/1", attempts, reported)
	}
}
