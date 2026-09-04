// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package artifacts

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOCIImageArchiveName(t *testing.T) {
	t.Parallel()

	got, err := OCIImageArchiveName("ghcr.io/azure/agent-ubuntu2404:v20260619")
	require.NoError(t, err)
	require.Equal(t, "rootfs-agent-ubuntu2404-v20260619.oci.tar.gz", got)
}

func TestRetryOCIImageCopyRetriesTransientReadFailure(t *testing.T) {
	t.Parallel()

	var delays []time.Duration

	transientErr := fmt.Errorf("read blob: %w", syscall.ECONNRESET)
	attempts := 0

	err := retryOCIImageCopy(
		context.Background(),
		func() error {
			attempts++
			if attempts < 3 {
				return transientErr
			}

			return nil
		},
		func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)

			return nil
		},
	)

	require.NoError(t, err)
	require.Equal(t, 3, attempts)
	require.Equal(t, []time.Duration{2 * time.Second, 4 * time.Second}, delays)
}

func TestRetryOCIImageCopyExhaustsTransientFailures(t *testing.T) {
	t.Parallel()

	var delays []time.Duration

	transientErr := fmt.Errorf("read blob: %w", syscall.ECONNRESET)
	attempts := 0

	err := retryOCIImageCopy(
		context.Background(),
		func() error {
			attempts++

			return transientErr
		},
		func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)

			return nil
		},
	)

	require.ErrorIs(t, err, syscall.ECONNRESET)
	require.Equal(t, ociImageCopyMaxAttempts, attempts)
	require.Equal(t, []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}, delays)
}

func TestRetryOCIImageCopyDoesNotRetryPermanentFailure(t *testing.T) {
	t.Parallel()

	permanentErr := errors.New("unauthorized")
	attempts := 0
	waits := 0

	err := retryOCIImageCopy(
		context.Background(),
		func() error {
			attempts++

			return permanentErr
		},
		func(context.Context, time.Duration) error {
			waits++

			return nil
		},
	)

	require.ErrorIs(t, err, permanentErr)
	require.Equal(t, 1, attempts)
	require.Zero(t, waits)
}

func TestRetryOCIImageCopyStopsWhenCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancelCause(context.Background())
	cancelErr := errors.New("stop retrying")
	attempts := 0

	err := retryOCIImageCopy(
		ctx,
		func() error {
			attempts++

			return fmt.Errorf("read blob: %w", syscall.ECONNRESET)
		},
		func(context.Context, time.Duration) error {
			cancel(cancelErr)

			return context.Cause(ctx)
		},
	)

	require.ErrorIs(t, err, cancelErr)
	require.Equal(t, 1, attempts)
}

func TestArchiveOCIImageRejectsDigestReference(t *testing.T) {
	t.Parallel()

	digest := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	err := ArchiveOCIImage(
		context.Background(),
		"ghcr.io/azure/agent-ubuntu2404@"+digest,
		filepath.Join(t.TempDir(), "rootfs.oci.tar.gz"),
	)
	require.ErrorContains(t, err, "must use a tag")
}
