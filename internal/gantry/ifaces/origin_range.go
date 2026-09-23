// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package ifaces

import (
	"context"
	"io"
)

// OriginMetadata describes the identity representation returned by upstream
// HEAD. Ref contains the resolved URL kind, including blob-to-manifest fallback;
// callers must use this reference for subsequent OpenRange calls.
type OriginMetadata struct {
	Ref         OriginRef
	Size        int64
	ContentType string // Exact upstream Content-Type, possibly empty.
}

// OriginRangePuller is the bounded upstream API used by range cache origins.
// Metadata uses only HEAD. OpenRange uses one data GET, apart from bounded
// authentication negotiation and redirects, and never retries at another URL
// kind. Authorization is request-scoped through registryauth's context.
type OriginRangePuller interface {
	HeadMetadata(ctx context.Context, ref OriginRef) (OriginMetadata, error)
	OpenRange(ctx context.Context, ref OriginRef, offset, length, fullSize int64) (io.ReadCloser, error)
}

// OriginMetadataPuller exposes the authoritative GET Content-Type, including
// absence and blob-to-manifest fallback. It performs no separate HEAD request.
type OriginMetadataPuller interface {
	PullWithMetadata(ctx context.Context, ref OriginRef) (body io.ReadCloser, size int64, contentType string, err error)
}

// OriginMetadataUnavailableError means upstream HEAD omitted a usable size.
// Requesters can fall back to their direct registry path. This is not NotFound.
type OriginMetadataUnavailableError struct {
	Reason string
}

func (e *OriginMetadataUnavailableError) Error() string {
	return "origin metadata unavailable: " + e.Reason
}

// OriginRangeUnsupportedError means upstream cannot serve the bounded range.
// Requesters can fall back to their direct registry path without downloading
// and discarding a prefix on the range-serving node.
type OriginRangeUnsupportedError struct {
	Reason string
}

func (e *OriginRangeUnsupportedError) Error() string {
	return "origin range unsupported: " + e.Reason
}
