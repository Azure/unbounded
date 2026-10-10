// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package installstate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAcquireWithin covers the wait directly; the coordinator's tests cover it
// against a real lock.
func TestAcquireWithin(t *testing.T) {
	t.Parallel()

	t.Run("returns the lock once it is free", func(t *testing.T) {
		t.Parallel()

		attempts := 0
		lock, err := AcquireWithin(t.Context(), time.Minute, func() (*Lock, error) {
			attempts++
			if attempts < 3 {
				return nil, ErrLockHeld
			}

			return &Lock{}, nil
		})
		require.NoError(t, err)
		assert.NotNil(t, lock)
		assert.Equal(t, 3, attempts)
	})

	t.Run("any other error stops the wait at once", func(t *testing.T) {
		t.Parallel()

		injected := errors.New("lock directory is read-only")
		attempts := 0

		_, err := AcquireWithin(t.Context(), time.Minute, func() (*Lock, error) {
			attempts++

			return nil, injected
		})
		require.ErrorIs(t, err, injected)
		assert.Equal(t, 1, attempts, "only a held lock is worth waiting for")
	})

	t.Run("a lock still held when the wait ends is reported as held", func(t *testing.T) {
		t.Parallel()

		_, err := AcquireWithin(t.Context(), 0, func() (*Lock, error) { return nil, ErrLockHeld })
		require.ErrorIs(t, err, ErrLockHeld)
	})

	t.Run("an ended context ends the wait", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := AcquireWithin(ctx, time.Minute, func() (*Lock, error) { return nil, ErrLockHeld })
		require.ErrorIs(t, err, context.Canceled)
	})
}
