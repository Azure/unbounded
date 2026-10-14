// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package object

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// invalidS3Response refines errInvalidS3Response for diagnostics only. It
// still matches errInvalidS3Response, so the Racer and sidecar mapping (502)
// is unchanged.
type invalidS3Response struct{ kind string }

// Error matches errInvalidS3Response so messages stay unchanged.
func (e *invalidS3Response) Error() string { return errInvalidS3Response.Error() }

func (e *invalidS3Response) Is(target error) bool { return target == errInvalidS3Response }

var (
	// errS3Redirect is a 3xx, typically a wrong region or addressing style.
	errS3Redirect error = &invalidS3Response{kind: "redirect"}
	// errS3Status is an HTTP status with no Racer meaning, such as 400 or 405.
	errS3Status error = &invalidS3Response{kind: "status"}
	// errS3Validation is a response that failed HEAD or GET validation.
	errS3Validation error = &invalidS3Response{kind: "validation"}
)

func invalidS3Status(code int) error {
	if code >= 300 && code < 400 {
		return errS3Redirect
	}

	return errS3Status
}

// InvalidResponseCategory returns a fixed diagnostic category for an invalid S3
// response: "redirect", "status", "validation", or "upstream" when the cause is
// unknown. It returns "" when err is not an invalid S3 response. The result
// never contains upstream text.
func InvalidResponseCategory(err error) string {
	var invalid *invalidS3Response
	if errors.As(err, &invalid) {
		return invalid.kind
	}

	if errors.Is(err, errInvalidS3Response) {
		return "upstream"
	}

	return ""
}

// ErrMetadataMismatch reports that a pinned read returned metadata that
// differs from the metadata selected by Stat.
var ErrMetadataMismatch = errors.New("racer-object: sidecar metadata mismatch")

// SidecarFailure describes a 5xx returned by the sidecar. It carries the
// bucket but not the key or query: the bucket is operator configuration and
// is already validated against the allowlist, while keys may hold tenant or
// user data and queries may hold presigned credentials. Err is the
// classified cause; callers must not log its text, which may hold upstream
// details. Use errors.Is or InvalidResponseCategory instead.
type SidecarFailure struct {
	Method string
	Status int
	Bucket string
	Err    error
}

func (s *sidecar) sdkFailure(r *http.Request, err error) sidecarFailure {
	return s.report(r, sidecarSDKFailure(err), err)
}

func (s *sidecar) report(r *http.Request, failure sidecarFailure, err error) sidecarFailure {
	if s.onFailure != nil && failure >= 500 {
		s.onFailure(SidecarFailure{Method: r.Method, Status: int(failure), Bucket: sidecarBucket(r), Err: err})
	}

	return failure
}

// sidecarBucket repeats the request parser's split; it is only called after
// the request has been accepted, so the bucket is valid and allowed.
func sidecarBucket(r *http.Request) string {
	path, _ := strings.CutPrefix(r.URL.EscapedPath(), "/")
	bucket, _, _ := strings.Cut(path, "/")
	bucket, _ = url.PathUnescape(bucket) //nolint:errcheck // EscapedPath guarantees valid escapes.

	return bucket
}
