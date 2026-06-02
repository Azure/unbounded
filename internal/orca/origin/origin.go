// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package origin defines the upstream-blob-store interface and shared
// types. Concrete adapters live under origin/<driver>/.
package origin

import (
	"context"
	"errors"
	"fmt"
	"io"
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
}

// ObjectInfo is the result of a successful Head.
type ObjectInfo struct {
	Size        int64
	ETag        string
	ContentType string
}

// Sentinel errors. Wrap with %w so callers use errors.Is.
//
// Driver contract:
//
//   - ErrNotFound: blob does not exist. AWS S3 driver returns this for
//     NoSuchKey responses; the azureblob driver for BlobNotFound /
//     ContainerNotFound.
//   - ErrAuth: 401 / 403. AWS S3 driver returns this for AccessDenied
//     and similar; the azureblob driver for HTTP 401/403 and the
//     AuthenticationFailed / AuthorizationFailure codes.
//
// New drivers should map their SDK-specific not-found and auth
// indicators onto these sentinels so handlers can route consistently
// via errors.Is.
var (
	ErrNotFound = errors.New("origin: not found")
	ErrAuth     = errors.New("origin: auth")
)

// OriginETagChangedError is returned by GetRange when the origin
// rejects the If-Match precondition.
type OriginETagChangedError struct {
	Bucket string
	Key    string
	Want   string
}

func (e *OriginETagChangedError) Error() string {
	return fmt.Sprintf("origin etag changed for %s/%s: want=%q",
		e.Bucket, e.Key, e.Want)
}

// UnsupportedBlobTypeError is returned by azureblob.Head when the
// target is a Page or Append blob. Orca only serves Block Blobs.
type UnsupportedBlobTypeError struct {
	Bucket   string
	Key      string
	BlobType string
}

func (e *UnsupportedBlobTypeError) Error() string {
	return fmt.Sprintf("origin unsupported blob type %s for %s/%s",
		e.BlobType, e.Bucket, e.Key)
}

// MissingETagError is returned by the fetch coordinator when an
// origin Head response carries an empty ETag. chunk.Path encodes the
// ETag in its hash input; a stable cache key requires the origin to
// supply one. Misconfigured backends (some S3-compatible
// implementations with specific bucket policies, custom origins not
// following the AWS/Azure contract) can omit ETags, in which case
// two different versions of the same (bucket, key) would alias to
// the same chunk.Path and orca would silently serve stale bytes.
// Rejecting at Head time surfaces the misconfiguration immediately
// instead of after observable corruption.
type MissingETagError struct {
	Bucket string
	Key    string
}

func (e *MissingETagError) Error() string {
	return fmt.Sprintf("origin returned empty ETag for %s/%s; orca requires versioned origins",
		e.Bucket, e.Key)
}

// ETagShort returns the first 8 characters of an unquoted ETag for
// log/debug emissions. ETags are not secrets but they're long enough
// to make log lines hard to read; the prefix is sufficient for
// matching one fill against another. Returns the input unchanged
// when shorter than 8 chars.
func ETagShort(etag string) string {
	const n = 8
	if len(etag) <= n {
		return etag
	}

	return etag[:n]
}
