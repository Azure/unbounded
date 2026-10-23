// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package backend

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk"
)

// Defaults and limits for [Config].
const (
	DefaultChunkSize    = 4 << 20
	MinChunkSize        = 64 << 10
	MaxChunkSize        = racersdk.PageSize
	DefaultCacheBytes   = 1 << 30
	DefaultReadaheadMax = 64 << 20
	DefaultMaxAttempts  = 5

	// MinCacheChunks is the number of chunks CacheBytes must hold in
	// addition to ReadaheadMax.
	MinCacheChunks = 4
)

// Config configures [Open].
type Config struct {
	// ETag optionally pins a version. Empty selects the current version.
	ETag string
	// ChunkSize is the cache granularity. Zero means 4 MiB. It must be a
	// power of two in [64 KiB, 16 MiB], so it divides racersdk.PageSize.
	ChunkSize int64
	// CacheBytes caps resident plus pending chunk buffers, apart from the
	// bounded overcommit described on [Backend]. Zero means 1 GiB. It must be
	// at least MinCacheChunks*ChunkSize + ReadaheadMax (after rounding).
	CacheBytes int64
	// ReadaheadMax is the largest read-ahead window. Zero means 64 MiB and a
	// negative value disables read-ahead. It is rounded down to a multiple
	// of ChunkSize, and a positive value smaller than one chunk becomes one
	// chunk.
	ReadaheadMax int64
	// WarmTTL, when positive, warms the object at Open and refreshes the
	// retention until Close. Renewals happen at half the time left on the
	// latest deadline Racer granted, but no later than WarmTTL/2 after the
	// last response, and never closer than 1 ms apart. Zero disables warming.
	WarmTTL time.Duration
	// MaxAttempts bounds attempts for retryable fetch errors. Zero means 5.
	MaxAttempts int
	// Logger receives diagnostics. Nil means slog.Default().
	Logger *slog.Logger
}

func (c Config) normalize() (Config, error) {
	if c.ChunkSize == 0 {
		c.ChunkSize = DefaultChunkSize
	}

	if c.ChunkSize < MinChunkSize || c.ChunkSize > MaxChunkSize || c.ChunkSize&(c.ChunkSize-1) != 0 {
		return c, fmt.Errorf("chunk size %d must be a power of two in [%d, %d]", c.ChunkSize, MinChunkSize, MaxChunkSize)
	}

	if c.CacheBytes == 0 {
		c.CacheBytes = DefaultCacheBytes
	}

	if c.CacheBytes < 0 {
		return c, fmt.Errorf("negative cache bytes %d", c.CacheBytes)
	}

	switch {
	case c.ReadaheadMax == 0:
		c.ReadaheadMax = DefaultReadaheadMax
	case c.ReadaheadMax < 0:
		c.ReadaheadMax = 0
	}

	if c.ReadaheadMax > 0 {
		c.ReadaheadMax = max(c.ReadaheadMax/c.ChunkSize, 1) * c.ChunkSize
	}

	// MinCacheChunks*ChunkSize is small, but adding ReadaheadMax can
	// overflow, so compare by subtraction first.
	base := MinCacheChunks * c.ChunkSize
	if c.ReadaheadMax > math.MaxInt64-base {
		return c, fmt.Errorf("cache bytes %d must be at least %d chunks of %d plus read-ahead %d, which overflows int64",
			c.CacheBytes, MinCacheChunks, c.ChunkSize, c.ReadaheadMax)
	}

	if need := base + c.ReadaheadMax; c.CacheBytes < need {
		return c, fmt.Errorf("cache bytes %d must be at least %d (%d chunks of %d plus read-ahead %d)",
			c.CacheBytes, need, MinCacheChunks, c.ChunkSize, c.ReadaheadMax)
	}

	if c.WarmTTL < 0 {
		return c, errors.New("negative warm TTL")
	}

	if c.MaxAttempts < 0 {
		return c, errors.New("negative max attempts")
	}

	if c.MaxAttempts == 0 {
		c.MaxAttempts = DefaultMaxAttempts
	}

	if c.Logger == nil {
		c.Logger = slog.Default()
	}

	return c, nil
}
