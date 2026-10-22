// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package backend

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk"
)

// Source is the ranged-read surface of Racer, abstracted so tests can inject
// faults.
type Source interface {
	Stat(ctx context.Context) (racersdk.Metadata, error)
	Warm(ctx context.Context, ttl time.Duration, etag string) (racersdk.WarmResult, error)
	// Open returns a reader for exactly length bytes at offset of version
	// etag. Implementations should verify the returned metadata matches etag.
	// The reader's lifetime is bound to ctx.
	Open(ctx context.Context, offset, length int64, etag string) (io.ReadCloser, error)
}

// MetadataReader is optionally implemented by readers returned from
// [Source.Open]. Open uses it to learn the size of a pinned version that is
// not the current one.
type MetadataReader interface {
	Metadata() racersdk.Metadata
}

// FromClient adapts a racersdk.Client and a fixed request to a Source.
//
// Open uses [racersdk.ReadOptions.SmallObject] once a Stat or Warm through
// this Source has reported an object no larger than [racersdk.PageSize].
// SmallObject is a promise about the whole object, not the range, so it is
// never set before the size is known.
func FromClient(c *racersdk.Client, req racersdk.Request) Source {
	return &clientSource{client: c, req: req}
}

type clientSource struct {
	client *racersdk.Client
	req    racersdk.Request
	// size is the last object size seen by Stat or Warm, or 0 if unknown.
	size atomic.Int64
}

func (s *clientSource) Stat(ctx context.Context) (racersdk.Metadata, error) {
	md, err := s.client.Stat(ctx, s.req)
	if err == nil {
		s.size.Store(md.Size)
	}

	return md, err
}

func (s *clientSource) Warm(ctx context.Context, ttl time.Duration, etag string) (racersdk.WarmResult, error) {
	res, err := s.client.Warm(ctx, s.req, ttl, racersdk.WarmOptions{ETag: etag})
	if err == nil {
		s.size.Store(res.Metadata.Size)
	}

	return res, err
}

func (s *clientSource) Open(ctx context.Context, offset, length int64, etag string) (io.ReadCloser, error) {
	size := s.size.Load()
	opts := racersdk.ReadOptions{
		Offset:      offset,
		Length:      length,
		ETag:        etag,
		SmallObject: size > 0 && size <= racersdk.PageSize,
	}

	obj, err := s.client.Get(ctx, s.req, opts)
	if err != nil {
		return nil, err
	}

	if got := obj.Metadata().ETag; etag != "" && got != etag {
		_ = obj.Close() //nolint:errcheck // The mismatch is the error to report.

		return nil, fmt.Errorf("%w: got ETag %q, want %q", racersdk.ErrVersionMismatch, got, etag)
	}

	return obj, nil
}
