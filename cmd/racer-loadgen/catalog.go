// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"sort"

	"github.com/opencontainers/go-digest"
)

const maxCatalogImages = 512

const (
	profileShuffle      = "shuffle"
	profileZipf         = "zipf"
	defaultZipfExponent = 1.2
)

func validateProfile(profile string, exponent float64) error {
	if profile != profileShuffle && profile != profileZipf {
		return fmt.Errorf("profile must be shuffle or zipf, got %q", profile)
	}

	if math.IsNaN(exponent) || math.IsInf(exponent, 0) || exponent <= 0 {
		return errors.New("zipf-exponent must be finite and positive")
	}

	return nil
}

// newZipfCDF builds a finite distribution with weight (index+1)^(-exponent).
// Unlike rand.NewZipf, this supports every positive exponent, including <= 1.
// Callers supply a nonempty catalog and a validated exponent. The table is
// immutable and shared by workers; rank never depends on worker or node identity.
func newZipfCDF(count int, exponent float64) []float64 {
	cdf := make([]float64, count)

	var total float64
	for index := range cdf {
		total += math.Pow(float64(index+1), -exponent)
		cdf[index] = total
	}

	for index := range cdf {
		cdf[index] /= total
	}
	// Close the distribution exactly, including when tiny tail weights round away.
	cdf[count-1] = 1

	return cdf
}

func zipfIndex(cdf []float64, draw float64) int {
	// draw is in [0,1). Strict comparison skips zero-width, rounded-away bins.
	return sort.Search(len(cdf), func(index int) bool { return cdf[index] > draw })
}

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
		imageOpts := catalogImageOptions(opts, index)

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

func catalogImageOptions(opts imageOptions, index int) imageOptions {
	// Image zero preserves the original content and latest tag.
	if index > 0 {
		opts.Seed = fmt.Sprintf("racer-loadgen/catalog/v1/%d/%s", index, opts.Seed)
	}

	return opts
}

// The zero value independently shuffles a complete pass, preserving the baseline.
// Zipf draws with replacement on every attempt, including after failures.
// Randomness affects only traversal, not catalog content or popularity rank.
type catalogTraversal struct {
	order         []int
	next          int
	zipfCDF       []float64
	randomFloat64 func() float64
}

// Each worker copies this initial state: the immutable CDF and concurrency-safe
// RNG are shared, while shuffle order and progress remain local to the worker.
func (p *puller) newTraversal() catalogTraversal {
	traversal := catalogTraversal{randomFloat64: p.randomFloat64}
	if p.opts.Profile == profileZipf {
		traversal.zipfCDF = newZipfCDF(len(p.images), p.opts.ZipfExponent)
	}

	return traversal
}

func (t *catalogTraversal) nextImage(images []*syntheticImage) *syntheticImage {
	if len(t.zipfCDF) != 0 {
		return images[zipfIndex(t.zipfCDF, t.randomFloat64())]
	}

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
