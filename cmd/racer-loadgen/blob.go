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

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
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

func (p *puller) nextBatch(t *catalogTraversal) blobBatch {
	return p.batches[t.nextIndex(len(p.batches))]
}

// newBlobCatalog creates exactly count objects of exactly size bytes, without tar
// framing, config, manifest, or jitter. The same virtual ReaderAt generator used
// by OCI layers supplies the raw payload; memory is independent of object size.
func newBlobCatalog(ctx context.Context, repository, seed string, count int, size int64) (*blobCatalog, error) {
	if count < 1 || count > maxCatalogImages || size < 1 {
		return nil, errors.New("catalog-blobs must be in [1, 512] and blob-bytes must be positive")
	}

	if len(repository) > 255 || !repositoryPattern.MatchString(repository) {
		return nil, errors.New("repository must be a valid lowercase registry repository name")
	}

	catalog := &blobCatalog{repository: repository, blobs: make(map[digest.Digest]blobSource)}
	buffer := make([]byte, 128*1024)

	for index := range count {
		key := sha256.Sum256(fmt.Appendf(nil, "racer-loadgen/blob/v1/%d/%s", index, seed))

		block, err := aes.NewCipher(key[:])
		if err != nil {
			return nil, err
		}

		data := &virtualLayer{payloadBytes: size, size: size, block: block}
		digester := digest.Canonical.Digester()

		for offset := int64(0); offset < size; {
			if err := ctx.Err(); err != nil {
				return nil, err
			}

			n, err := data.ReadAt(buffer, offset)
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}

			if _, err := digester.Hash().Write(buffer[:n]); err != nil {
				return nil, err
			}

			offset += int64(n)
		}

		desc := ocispec.Descriptor{MediaType: "application/octet-stream", Digest: digester.Digest(), Size: size}
		if _, duplicate := catalog.blobs[desc.Digest]; duplicate {
			return nil, errors.New("blob catalog contains duplicate content; increase blob-bytes or change seed")
		}

		catalog.blobs[desc.Digest] = blobSource{descriptor: desc, data: data}
		catalog.batches = append(catalog.batches, blobBatch{blobs: []blobRequest{{"blob", desc}}})
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return catalog, nil
}
