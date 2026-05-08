// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package origin defines the upstream-blob-store interface and shared
// types. Concrete adapters live under origin/<driver>/.
//
// See design/orca/design.md s7 for the full interface.
package origin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// Origin is a read-only view of an upstream blob store.
type Origin interface {
	// Head returns object metadata. If the blob does not exist, returns
	// ErrNotFound. If the blob is an unsupported type (e.g., azureblob
	// non-BlockBlob), returns UnsupportedBlobTypeError.
	Head(ctx context.Context, bucket, key string) (ObjectInfo, error)

	// GetRange fetches [off, off+n) bytes of the object. The etag is
	// passed as `If-Match: <etag>` so a mid-flight overwrite is detected
	// at the wire (returns OriginETagChangedError).
	GetRange(ctx context.Context, bucket, key, etag string, off, n int64) (io.ReadCloser, error)

	// List enumerates objects under prefix. Pagination via marker.
	List(ctx context.Context, bucket, prefix, marker string, max int) (ListResult, error)
}

// ObjectInfo is the result of a successful Head.
type ObjectInfo struct {
	Size          int64
	ETag          string
	ContentType   string
	LastValidated time.Time
	LastStatus    int
}

// ListResult is the paginated result of List.
type ListResult struct {
	Entries     []ObjectEntry
	NextMarker  string
	IsTruncated bool
}

// ObjectEntry is one item in a ListResult.
type ObjectEntry struct {
	Key      string
	Size     int64
	ETag     string
	BlobType string // "" for s3; "BlockBlob" / "PageBlob" / "AppendBlob" for azureblob
}

// Sentinel errors. Wrap with %w so callers use errors.Is.
var (
	ErrNotFound = errors.New("origin: not found")
	ErrAuth     = errors.New("origin: auth")
	ErrThrottle = errors.New("origin: throttle")
)

// OriginETagChangedError is returned by GetRange when the origin
// rejects the If-Match precondition.
type OriginETagChangedError struct {
	Bucket string
	Key    string
	Want   string
	Got    string
}

func (e *OriginETagChangedError) Error() string {
	return fmt.Sprintf("origin etag changed for %s/%s: want=%q got=%q",
		e.Bucket, e.Key, e.Want, e.Got)
}

// UnsupportedBlobTypeError is returned by azureblob.Head when the
// target is a Page or Append blob (design.md s9).
type UnsupportedBlobTypeError struct {
	Bucket   string
	Key      string
	BlobType string
}

func (e *UnsupportedBlobTypeError) Error() string {
	return fmt.Sprintf("origin unsupported blob type %s for %s/%s",
		e.BlobType, e.Bucket, e.Key)
}
