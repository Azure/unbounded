// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package s3

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/Azure/unbounded/internal/orca/cachestore"
	"github.com/Azure/unbounded/internal/orca/chunk"
)

// makeResponseErr builds an *awshttp.ResponseError wrapping the
// given HTTP status code. Mirrors how the AWS SDK surfaces service
// errors to callers: an *awshttp.ResponseError nesting a
// *smithyhttp.ResponseError that carries the HTTP response.
func makeResponseErr(status int, inner error) *awshttp.ResponseError {
	return &awshttp.ResponseError{
		ResponseError: &smithyhttp.ResponseError{
			Response: &smithyhttp.Response{
				Response: &http.Response{StatusCode: status},
			},
			Err: inner,
		},
	}
}

// TestIsPreconditionFailed_FromHTTPStatus verifies that 412 alone
// signals precondition failure; other statuses (and errors lacking
// HTTP-response context) do not. The original implementation matched
// service error codes by string ("PreconditionFailed",
// "InvalidArgument", "ConditionalRequestConflict") plus substring
// "412" - fragile across SDK versions and backend implementations.
func TestIsPreconditionFailed_FromHTTPStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"412 ResponseError -> true", makeResponseErr(412, errors.New("precondition")), true},
		{"500 ResponseError -> false", makeResponseErr(500, errors.New("ise")), false},
		{"404 ResponseError -> false", makeResponseErr(404, errors.New("not found")), false},
		{"plain error -> false", errors.New("StatusCode: 412 something"), false},
		{"nil -> false", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPreconditionFailed(tt.err); got != tt.want {
				t.Errorf("isPreconditionFailed = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestIsNotFound covers the typed-error and HTTP-status branches.
func TestIsNotFound(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"NoSuchKey typed", &s3types.NoSuchKey{}, true},
		{"NoSuchBucket typed", &s3types.NoSuchBucket{}, true},
		{"NotFound typed", &s3types.NotFound{}, true},
		{"404 ResponseError", makeResponseErr(404, errors.New("not found")), true},
		{"500 ResponseError", makeResponseErr(500, errors.New("ise")), false},
		{"plain error", errors.New("random"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNotFound(tt.err); got != tt.want {
				t.Errorf("isNotFound = %v, want %v", got, tt.want)
			}
		})
	}
}

// fakeAPIError implements smithy.APIError for testing the
// AccessDenied / Forbidden mapping path.
type fakeAPIError struct{ code string }

func (e *fakeAPIError) Error() string                 { return e.code }
func (e *fakeAPIError) ErrorCode() string             { return e.code }
func (e *fakeAPIError) ErrorMessage() string          { return e.code }
func (e *fakeAPIError) ErrorFault() smithy.ErrorFault { return smithy.FaultUnknown }
func (e *fakeAPIError) HTTPStatusCode() int           { return 0 }

// TestMapErr covers the full mapping table: 404 / typed not-found
// -> ErrNotFound, AccessDenied APIError -> ErrAuth, 5xx ->
// ErrTransient, anything else passes through.
func TestMapErr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want error
	}{
		{"NoSuchKey -> ErrNotFound", &s3types.NoSuchKey{}, cachestore.ErrNotFound},
		{"404 ResponseError -> ErrNotFound", makeResponseErr(404, errors.New("nf")), cachestore.ErrNotFound},
		{"AccessDenied APIError -> ErrAuth", &fakeAPIError{code: "AccessDenied"}, cachestore.ErrAuth},
		{"InvalidAccessKeyId APIError -> ErrAuth", &fakeAPIError{code: "InvalidAccessKeyId"}, cachestore.ErrAuth},
		{"403 ResponseError -> ErrAuth", makeResponseErr(403, errors.New("denied")), cachestore.ErrAuth},
		{"401 ResponseError -> ErrAuth", makeResponseErr(401, errors.New("unauth")), cachestore.ErrAuth},
		{"500 ResponseError -> ErrTransient", makeResponseErr(500, errors.New("ise")), cachestore.ErrTransient},
		{"503 ResponseError -> ErrTransient", makeResponseErr(503, errors.New("unavail")), cachestore.ErrTransient},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapErr(tt.err)
			if !errors.Is(got, tt.want) {
				t.Errorf("mapErr = %v, want errors.Is(_, %v) true", got, tt.want)
			}
		})
	}
}

// TestMapErr_PassthroughUnknown verifies that unrecognized errors
// pass through unchanged.
func TestMapErr_PassthroughUnknown(t *testing.T) {
	t.Parallel()

	src := errors.New("unrecognized")
	if got := mapErr(src); got != src {
		t.Errorf("mapErr(unknown) = %v, want passthrough %v", got, src)
	}
}

// TestGetChunk_RejectsZeroN verifies that GetChunk refuses n <= 0.
// Forwarding such a request would produce a malformed S3 Range
// header (bytes=0--1) which the backend rejects with InvalidArgument.
// The wire-format boundary (cluster.DecodeChunkKey) already rejects
// object_size <= 0, so an in-process caller reaching this with n <= 0
// is a logic bug we want surfaced as an explicit error.
//
// Regression for C-2.
func TestGetChunk_RejectsZeroN(t *testing.T) {
	t.Parallel()

	d := &Driver{}

	tests := []struct {
		name string
		off  int64
		n    int64
	}{
		{"n zero", 0, 0},
		{"n negative", 0, -1},
		{"off negative", -1, 1024},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := d.GetChunk(context.Background(), chunkPathOnlyKey(), tt.off, tt.n)
			if err == nil {
				t.Errorf("GetChunk(off=%d, n=%d) returned nil; want error", tt.off, tt.n)
			}
		})
	}
}

// TestPutChunk_RejectsZeroSize verifies that PutChunk refuses
// size <= 0. A zero-byte commit would poison the path with a
// 0-byte blob and subsequent GetChunk(n=expected) reads would
// either error or stream zero bytes.
//
// Regression for C-3.
func TestPutChunk_RejectsZeroSize(t *testing.T) {
	t.Parallel()

	d := &Driver{}

	for _, size := range []int64{0, -1} {
		if err := d.PutChunk(context.Background(), chunkPathOnlyKey(), size, nil); err == nil {
			t.Errorf("PutChunk(size=%d) returned nil; want error", size)
		}
	}
}

// chunkPathOnlyKey returns a minimal chunk.Key whose Path() can be
// computed; used by the GetChunk / PutChunk guard tests that error
// before any S3 round-trip.
func chunkPathOnlyKey() chunk.Key {
	return chunk.Key{
		OriginID:  "ox",
		Bucket:    "b",
		ObjectKey: "o",
		ETag:      "e1",
		ChunkSize: 1024,
		Index:     0,
	}
}

// TestPutChunk_SeekableSizeMismatch verifies that PutChunk rejects
// a seekable reader whose actual length does not match the declared
// size. Without the seekable-path probe, a buggy caller passing a
// Reader of length M with size=N would either be rejected by S3
// (ContentLength mismatch) or upload a wrong-sized blob.
//
// Regression for H-6.
func TestPutChunk_SeekableSizeMismatch(t *testing.T) {
	t.Parallel()

	d := &Driver{}

	// Reader has 10 bytes, but caller claims 1024. PutChunk must
	// fail at the seek-and-check probe before any RPC.
	r := bytes.NewReader(make([]byte, 10))
	if err := d.PutChunk(context.Background(), chunkPathOnlyKey(), 1024, r); err == nil {
		t.Errorf("PutChunk accepted seekable reader with size mismatch")
	}

	// Reader has 100 bytes, caller claims 50: also a mismatch
	// (caller would upload only 50, leaving 50 unread).
	r = bytes.NewReader(make([]byte, 100))
	if err := d.PutChunk(context.Background(), chunkPathOnlyKey(), 50, r); err == nil {
		t.Errorf("PutChunk accepted seekable reader longer than declared size")
	}
}
