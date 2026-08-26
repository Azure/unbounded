// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integrationtest

package inttest

import (
	"context"
	"io"
	"sync/atomic"

	"github.com/Azure/unbounded/internal/orca/origin"
)

// CountingOrigin is an origin.Origin decorator that counts Head and
// GetRange calls. It is used by tests that need to assert
// singleflight collapse and coordinator routing.
type CountingOrigin struct {
	inner origin.Origin

	heads     atomic.Int64
	getRanges atomic.Int64
}

// NewCountingOrigin wraps inner with call counters.
func NewCountingOrigin(inner origin.Origin) *CountingOrigin {
	return &CountingOrigin{inner: inner}
}

// GetRanges returns the number of GetRange() calls observed.
func (c *CountingOrigin) GetRanges() int64 { return c.getRanges.Load() }

// Reset zeroes all counters.
func (c *CountingOrigin) Reset() {
	c.heads.Store(0)
	c.getRanges.Store(0)
}

// Head implements origin.Origin.
func (c *CountingOrigin) Head(ctx context.Context, bucket, key string) (origin.ObjectInfo, error) {
	c.heads.Add(1)

	return c.inner.Head(ctx, bucket, key)
}

// GetRange implements origin.Origin.
func (c *CountingOrigin) GetRange(ctx context.Context, bucket, key, etag string, off, length int64) (io.ReadCloser, error) {
	c.getRanges.Add(1)

	return c.inner.GetRange(ctx, bucket, key, etag, off, length)
}
