// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package oci

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	ispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestCopyImageWithRetrySucceedsAfterTemporaryFailures(t *testing.T) {
	origSleep := sleepBeforeOCIPullRetry

	t.Cleanup(func() {
		sleepBeforeOCIPullRetry = origSleep
	})

	var delays []time.Duration

	sleepBeforeOCIPullRetry = func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		return nil
	}

	task := &downloadRootFS{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	want := ispec.Descriptor{MediaType: "application/vnd.oci.image.manifest.v1+json"}

	attempts := 0

	desc, err := task.copyImageWithRetry(context.Background(), "example.test/rootfs:latest", func(context.Context) (ispec.Descriptor, error) {
		attempts++
		if attempts < 3 {
			return ispec.Descriptor{}, errors.New("temporary DNS failure")
		}

		return want, nil
	})
	if err != nil {
		t.Fatalf("copyImageWithRetry returned error: %v", err)
	}

	if desc.MediaType != want.MediaType {
		t.Fatalf("copyImageWithRetry descriptor = %#v, want %#v", desc, want)
	}

	if attempts != 3 {
		t.Fatalf("copyImageWithRetry attempts = %d, want 3", attempts)
	}

	wantDelays := []time.Duration{2 * time.Second, 4 * time.Second}
	if !equalDurations(delays, wantDelays) {
		t.Fatalf("retry delays = %v, want %v", delays, wantDelays)
	}
}

func TestCopyImageWithRetryReturnsLastError(t *testing.T) {
	origSleep := sleepBeforeOCIPullRetry

	t.Cleanup(func() {
		sleepBeforeOCIPullRetry = origSleep
	})

	sleepBeforeOCIPullRetry = func(context.Context, time.Duration) error {
		return nil
	}

	task := &downloadRootFS{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	wantErr := errors.New("manifest not found")

	attempts := 0
	_, err := task.copyImageWithRetry(context.Background(), "example.test/rootfs:missing", func(context.Context) (ispec.Descriptor, error) {
		attempts++
		return ispec.Descriptor{}, wantErr
	})

	if !errors.Is(err, wantErr) {
		t.Fatalf("copyImageWithRetry error = %v, want %v", err, wantErr)
	}

	if attempts != ociPullMaxAttempts {
		t.Fatalf("copyImageWithRetry attempts = %d, want %d", attempts, ociPullMaxAttempts)
	}
}

func TestCopyImageWithRetryHonorsContextCancellation(t *testing.T) {
	origSleep := sleepBeforeOCIPullRetry

	t.Cleanup(func() {
		sleepBeforeOCIPullRetry = origSleep
	})

	sleepBeforeOCIPullRetry = func(ctx context.Context, _ time.Duration) error {
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	task := &downloadRootFS{log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	attempts := 0
	_, err := task.copyImageWithRetry(ctx, "example.test/rootfs:latest", func(context.Context) (ispec.Descriptor, error) {
		attempts++

		cancel()

		return ispec.Descriptor{}, errors.New("temporary DNS failure")
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("copyImageWithRetry error = %v, want %v", err, context.Canceled)
	}

	if attempts != 1 {
		t.Fatalf("copyImageWithRetry attempts = %d, want 1", attempts)
	}
}

func equalDurations(got, want []time.Duration) bool {
	if len(got) != len(want) {
		return false
	}

	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}

	return true
}
