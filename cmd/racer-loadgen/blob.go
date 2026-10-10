// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/aes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/errgroup"
)

const (
	maxCatalogBlobs    = 32768
	maxCatalogWorkers  = 64
	catalogBufferBytes = 128 * 1024
)

// blobRequest retains OCI roles only for the HTTP acquisition adapter and metrics.
// Scheduling, consumption, and verification do not depend on the backend.
type blobRequest struct {
	kind       string
	descriptor ocispec.Descriptor
}

// blobBatch is one admitted operation. Its prefix is ordered; the remaining blobs
// share the bounded per-operation worker pool. OCI compatibility uses an ordered
// manifest/config prefix followed by layers. Generic operations contain one blob.
type blobBatch struct {
	prefix []blobRequest
	blobs  []blobRequest
}

func imageBatch(img *syntheticImage) blobBatch {
	batch := blobBatch{prefix: []blobRequest{{"manifest", img.Manifest}, {"config", img.Config}}}
	for _, desc := range img.Layers {
		batch.blobs = append(batch.blobs, blobRequest{"layer", desc})
	}

	return batch
}

func (p *puller) batchCount() int {
	if len(p.batches) != 0 {
		return len(p.batches)
	}

	return len(p.images)
}

func (p *puller) nextBatch(t *catalogTraversal) blobBatch {
	index := t.nextIndex(p.batchCount())
	if len(p.batches) != 0 {
		return p.batches[index]
	}

	return imageBatch(p.images[index])
}

// newBlobCatalog creates exactly count objects of exactly size bytes, without tar
// framing, config, manifest, or jitter. The same virtual ReaderAt generator used
// by OCI layers supplies the raw payload; memory is independent of object size.
func newBlobCatalog(ctx context.Context, repository, seed string, count int, size int64) (*blobCatalog, error) {
	return newBlobCatalogWithWorkers(ctx, repository, seed, count, size, 1)
}

func newBlobCatalogWithWorkers(ctx context.Context, repository, seed string, count int, size int64, workers int) (*blobCatalog, error) {
	lastLog := time.Now()
	progress := func(completed int) {
		if completed == count || time.Since(lastLog) >= 5*time.Second {
			slog.Info("blob catalog initialization progress", "completed_objects", completed, "objects", count,
				"completed_bytes", float64(completed)*float64(size), "workers", min(workers, count))

			lastLog = time.Now()
		}
	}

	return buildBlobCatalog(ctx, repository, count, size, workers, func(ctx context.Context, index int, buffer []byte) (blobSource, error) {
		return hashCatalogBlob(ctx, seed, index, size, buffer)
	}, progress)
}

// buildBlobCatalog joins every worker before returning, including on failure.
// Only metadata survives hashing. Each worker reuses its own scratch buffer.
// The progress callback is serialized and counts fully hashed, distinct objects.
func buildBlobCatalog(ctx context.Context, repository string, count int, size int64, workers int,
	build func(context.Context, int, []byte) (blobSource, error), progress func(int),
) (*blobCatalog, error) {
	if count < 1 || count > maxCatalogBlobs || size < 1 {
		return nil, fmt.Errorf("catalog-blobs must be in [1, %d] and blob-bytes must be positive", maxCatalogBlobs)
	}

	if workers < 1 || workers > maxCatalogWorkers {
		return nil, fmt.Errorf("catalog-workers must be in [1, %d]", maxCatalogWorkers)
	}

	if len(repository) > 255 || !repositoryPattern.MatchString(repository) {
		return nil, errors.New("repository must be a valid lowercase registry repository name")
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	catalog := &blobCatalog{repository: repository, blobs: make(map[digest.Digest]blobSource, count), batches: make([]blobBatch, count)}
	group, workCtx := errgroup.WithContext(ctx)
	workers = min(workers, count)

	var (
		mu        sync.Mutex
		completed int
	)

	for worker := range workers {
		group.Go(func() error {
			buffer := make([]byte, catalogBufferBytes)

			for index := worker; index < count; index += workers {
				if err := workCtx.Err(); err != nil {
					return err
				}

				blob, err := build(workCtx, index, buffer)
				if err != nil {
					return fmt.Errorf("catalog blob %d: %w", index, err)
				}

				mu.Lock()
				if err := workCtx.Err(); err != nil {
					mu.Unlock()
					return err
				}

				if _, duplicate := catalog.blobs[blob.descriptor.Digest]; duplicate {
					mu.Unlock()
					return errors.New("blob catalog contains duplicate content; increase blob-bytes or change seed")
				}

				catalog.blobs[blob.descriptor.Digest] = blob
				catalog.batches[index] = blobBatch{blobs: []blobRequest{{"blob", blob.descriptor}}}

				completed++
				if progress != nil {
					progress(completed)
				}
				mu.Unlock()
			}

			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return catalog, nil
}

func hashCatalogBlob(ctx context.Context, seed string, index int, size int64, buffer []byte) (blobSource, error) {
	if err := ctx.Err(); err != nil {
		return blobSource{}, err
	}

	key := sha256.Sum256(fmt.Appendf(nil, "racer-loadgen/blob/v1/%d/%s", index, seed))

	block, err := aes.NewCipher(key[:])
	if err != nil {
		return blobSource{}, err
	}

	data := &virtualLayer{payloadBytes: size, size: size, block: block}
	digester := digest.Canonical.Digester()

	for offset := int64(0); offset < size; {
		if err := ctx.Err(); err != nil {
			return blobSource{}, err
		}

		n, err := data.ReadAt(buffer, offset)
		if err != nil && !errors.Is(err, io.EOF) {
			return blobSource{}, err
		}

		if _, err := digester.Hash().Write(buffer[:n]); err != nil {
			return blobSource{}, err
		}

		offset += int64(n)
	}

	desc := ocispec.Descriptor{MediaType: "application/octet-stream", Digest: digester.Digest(), Size: size}

	return blobSource{descriptor: desc, data: data}, nil
}
