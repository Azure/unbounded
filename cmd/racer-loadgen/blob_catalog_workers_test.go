// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/require"
)

func TestBlobCatalogWorkerOptions(t *testing.T) {
	for _, workers := range []int{1, 32, maxCatalogWorkers} {
		opts, err := parseOptions([]string{fmt.Sprintf("--catalog-blobs=%d", maxCatalogBlobs), fmt.Sprintf("--catalog-workers=%d", workers)}, io.Discard)
		require.NoError(t, err)
		require.Equal(t, maxCatalogBlobs, opts.catalogBlobs)
		require.Equal(t, workers, opts.catalogWorkers)
	}

	for _, args := range [][]string{
		{"--catalog-workers=1"},
		{"--catalog-images=1", "--catalog-workers=1"},
		{"--backend=s3", "--catalog-workers=1"},
		{"--catalog-blobs=1", "--catalog-workers=0"},
		{"--catalog-blobs=1", "--catalog-workers=-1"},
		{"--catalog-blobs=1", "--catalog-workers=65"},
	} {
		_, err := parseOptions(args, io.Discard)
		require.ErrorContains(t, err, "catalog-workers")
	}
}

func TestBlobCatalogWorkersStableContent(t *testing.T) {
	const size = 2*catalogBufferBytes + 17

	legacy, err := newBlobCatalog(t.Context(), "test/blobs", "parallel", 9, size)
	require.NoError(t, err)

	for _, workers := range []int{1, 4, maxCatalogWorkers} {
		catalog, err := newBlobCatalogWithWorkers(t.Context(), "other/repo", "parallel", 9, size, workers)
		require.NoError(t, err)
		require.Equal(t, legacy.batches, catalog.batches)

		for _, batch := range catalog.batches {
			desc := batch.blobs[0].descriptor
			blob := catalog.blobs[desc.Digest]
			require.IsType(t, &virtualLayer{}, blob.data)

			var readers sync.WaitGroup
			for range 3 {
				readers.Go(func() {
					data, err := io.ReadAll(io.NewSectionReader(blob.data, 0, desc.Size))
					if err != nil || digest.FromBytes(data) != desc.Digest {
						t.Errorf("shared read: error=%v, digest=%s", err, digest.FromBytes(data))
					}
				})
			}

			readers.Wait()
		}
	}
}

func TestBlobCatalogWorkersBounds(t *testing.T) {
	catalog, err := newBlobCatalogWithWorkers(t.Context(), "test/blobs", "large", maxCatalogBlobs, 32, 32)
	require.NoError(t, err)
	require.Len(t, catalog.blobs, maxCatalogBlobs)
	require.Len(t, catalog.batches, maxCatalogBlobs)

	for _, workers := range []int{0, -1, maxCatalogWorkers + 1} {
		catalog, err := newBlobCatalogWithWorkers(t.Context(), "test/blobs", "seed", 1, 32, workers)
		require.ErrorContains(t, err, "catalog-workers")
		require.Nil(t, catalog)
	}

	for _, workers := range []int{1, 8} {
		catalog, err := newBlobCatalogWithWorkers(t.Context(), "test/blobs", "seed", 512, 1, workers)
		require.ErrorContains(t, err, "duplicate content")
		require.Nil(t, catalog)
	}
}

func TestBlobCatalogWorkersJoinOnFailureAndCancellation(t *testing.T) {
	const workers = 4

	failure := errors.New("hash failed")

	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure=%v", fail), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			var active, started atomic.Int32

			entered := make(chan struct{}, workers)
			release := make(chan struct{})
			done := make(chan error, 1)

			go func() {
				catalog, err := buildBlobCatalog(ctx, "test/blobs", 100, 1, workers, func(ctx context.Context, index int, buffer []byte) (blobSource, error) {
					active.Add(1)

					started.Add(1)
					defer active.Add(-1)

					if len(buffer) != catalogBufferBytes || cap(buffer) != catalogBufferBytes {
						t.Errorf("scratch buffer size: len=%d cap=%d", len(buffer), cap(buffer))
					}

					entered <- struct{}{}

					if fail && index == 0 {
						select {
						case <-release:
							return blobSource{}, failure
						case <-ctx.Done():
							return blobSource{}, ctx.Err()
						}
					}

					<-ctx.Done()

					return blobSource{}, ctx.Err()
				}, nil)
				if catalog != nil {
					t.Error("published partial catalog")
				}

				done <- err
			}()

			for range workers {
				select {
				case <-entered:
				case <-time.After(3 * time.Second):
					t.Fatal("workers did not enter")
				}
			}

			require.Equal(t, int32(workers), active.Load())

			if fail {
				close(release)
			} else {
				cancel()
			}

			select {
			case err := <-done:
				if fail {
					require.ErrorIs(t, err, failure)
				} else {
					require.ErrorIs(t, err, context.Canceled)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("workers did not stop")
			}

			require.Zero(t, active.Load(), "all builders must be joined before return")
			require.Equal(t, int32(workers), started.Load(), "no queued work after failure")
		})
	}
}

func TestBlobCatalogWorkersProgressAndEarlyCancellation(t *testing.T) {
	var completed []int

	catalog, err := buildBlobCatalog(t.Context(), "test/blobs", 7, 32, 3,
		func(ctx context.Context, index int, buffer []byte) (blobSource, error) {
			return hashCatalogBlob(ctx, "progress", index, 32, buffer)
		}, func(count int) { completed = append(completed, count) })
	require.NoError(t, err)
	require.Len(t, catalog.batches, 7)
	require.Equal(t, []int{1, 2, 3, 4, 5, 6, 7}, completed)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	catalog, err = buildBlobCatalog(ctx, "test/blobs", 7, 32, 3,
		func(context.Context, int, []byte) (blobSource, error) {
			t.Error("started work after cancellation")
			return blobSource{}, nil
		}, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, catalog)
}

func TestBlobHashCancellationDuringLargeObject(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	catalog, err := newBlobCatalogWithWorkers(ctx, "test/blobs", "cancel", 8, 1<<40, 4)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, catalog)
}

func TestBlobCatalogWorkersReuseLocalBuffers(t *testing.T) {
	for _, count := range []int{1, 17} {
		const workers = 4

		var mu sync.Mutex

		buffers := make(map[int]*byte)
		catalog, err := buildBlobCatalog(t.Context(), "test/blobs", count, 32, workers,
			func(ctx context.Context, index int, buffer []byte) (blobSource, error) {
				worker := index % min(workers, count)

				mu.Lock()
				if prior, ok := buffers[worker]; ok && prior != &buffer[0] {
					t.Error("worker did not reuse its buffer")
				}

				for other, ptr := range buffers {
					if other != worker && ptr == &buffer[0] {
						t.Error("workers share a scratch buffer")
					}
				}

				buffers[worker] = &buffer[0]
				mu.Unlock()

				return hashCatalogBlob(ctx, "buffers", index, 32, buffer)
			}, nil)
		require.NoError(t, err)
		require.Len(t, catalog.batches, count)
		require.Len(t, buffers, min(workers, count))
	}
}

func TestBlobCatalogWorkersCancellationBeforePublish(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	catalog, err := buildBlobCatalog(ctx, "test/blobs", 7, 32, 3,
		func(ctx context.Context, index int, buffer []byte) (blobSource, error) {
			return hashCatalogBlob(ctx, "cancel-at-completion", index, 32, buffer)
		}, func(completed int) {
			if completed == 7 {
				cancel()
			}
		})
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, catalog)
}
