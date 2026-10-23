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
// Open uses [racersdk.ReadOptions.SmallObject] only when the most recent Stat
// or Warm through this Source reported an object no larger than
// [racersdk.PageSize] with the same ETag that Open requests. SmallObject is a
// promise about the whole version, not the range, so it is never set for an
// unpinned read or for a version whose size has not been observed.
func FromClient(c *racersdk.Client, req racersdk.Request) Source {
	return &clientSource{client: c, req: req}
}

type clientSource struct {
	client *racersdk.Client
	req    racersdk.Request
	// last is the version most recently reported by Stat or Warm, or nil.
	// The ETag and size are stored together so a concurrent refresh cannot
	// pair one version's size with another version's ETag.
	last atomic.Pointer[versionSize]
}

type versionSize struct {
	etag string
	size int64
}

func (s *clientSource) remember(md racersdk.Metadata) {
	s.last.Store(&versionSize{etag: md.ETag, size: md.Size})
}

// smallObject reports whether etag names a version known to fit in one page.
func (s *clientSource) smallObject(etag string) bool {
	v := s.last.Load()

	return etag != "" && v != nil && v.etag == etag && v.size > 0 && v.size <= racersdk.PageSize
}

func (s *clientSource) Stat(ctx context.Context) (racersdk.Metadata, error) {
	md, err := s.client.Stat(ctx, s.req)
	if err == nil {
		s.remember(md)
	}

	return md, err
}

func (s *clientSource) Warm(ctx context.Context, ttl time.Duration, etag string) (racersdk.WarmResult, error) {
	res, err := s.client.Warm(ctx, s.req, ttl, racersdk.WarmOptions{ETag: etag})
	if err == nil {
		s.remember(res.Metadata)
	}

	return res, err
}

func (s *clientSource) Open(ctx context.Context, offset, length int64, etag string) (io.ReadCloser, error) {
	opts := racersdk.ReadOptions{
		Offset:      offset,
		Length:      length,
		ETag:        etag,
		SmallObject: s.smallObject(etag),
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
