// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"

	"github.com/opencontainers/go-digest"
)

const maxCatalogImages = 512

type imageCatalog struct {
	images     []*syntheticImage
	repository string
	manifests  map[string]*syntheticImage
	blobs      map[digest.Digest]imageBlob
}

// newCatalog hashes one image at a time with one bounded scratch buffer. Only
// descriptors, tar headers, and generator state survive initialization, not payloads.
func newCatalog(ctx context.Context, opts imageOptions, count int) (*imageCatalog, error) {
	if count < 1 || count > maxCatalogImages {
		return nil, fmt.Errorf("catalog-images must be in [1, %d]", maxCatalogImages)
	}

	images := make([]*syntheticImage, 0, count)
	for index := range count {
		imageOpts := opts
		// Image zero preserves the original content and latest tag. Derivation
		// depends only on the shared seed and index, never on the origin node.
		if index > 0 {
			imageOpts.Seed = fmt.Sprintf("racer-loadgen/catalog/v1/%d/%s", index, opts.Seed)
		}

		img, err := newImage(ctx, imageOpts)
		if err != nil {
			return nil, fmt.Errorf("catalog image %d: %w", index, err)
		}

		images = append(images, img)
		slog.Info("catalog image initialized", "image", index, "images", count, "digest", img.Manifest.Digest)
	}

	catalog := catalogFromImages(images)

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return catalog, nil
}

func catalogFromImages(images []*syntheticImage) *imageCatalog {
	c := &imageCatalog{
		images: images, repository: images[0].repository,
		manifests: make(map[string]*syntheticImage), blobs: make(map[digest.Digest]imageBlob),
	}

	c.manifests["latest"] = images[0]
	for index, img := range images {
		c.manifests[fmt.Sprintf("image-%06d", index)] = img

		c.manifests[img.Manifest.Digest.String()] = img
		for key, blob := range img.blobs {
			c.blobs[key] = blob
		}
	}

	return c
}

// Each worker independently shuffles a complete pass. Randomness affects only
// traversal, not catalog content. Failed attempts advance too, avoiding a hot key.
type catalogTraversal struct {
	order []int
	next  int
}

func (t *catalogTraversal) nextImage(images []*syntheticImage) *syntheticImage {
	if len(t.order) == 0 {
		t.order = rand.Perm(len(images))
	} else if t.next == len(t.order) {
		rand.Shuffle(len(t.order), func(i, j int) { t.order[i], t.order[j] = t.order[j], t.order[i] })
		t.next = 0
	}

	img := images[t.order[t.next]]
	t.next++

	return img
}
