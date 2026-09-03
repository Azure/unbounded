// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/xml"
	"net/http"
)

// S3 standard error code names mirrored from the AWS S3 REST API.
// SDKs (aws-sdk-go-v2, boto3, MinIO client, etc.) branch on the
// <Code> element of the error envelope below to surface a typed
// error to callers. Using the canonical AWS names means existing S3
// client code "just works" against orca's edge surface.
//
// See:
// https://docs.aws.amazon.com/AmazonS3/latest/API/ErrorResponses.html
const (
	s3ErrNoSuchKey        = "NoSuchKey"
	s3ErrInvalidRange     = "InvalidRange"
	s3ErrMethodNotAllowed = "MethodNotAllowed"
	s3ErrNotImplemented   = "NotImplemented"
	s3ErrAccessDenied     = "AccessDenied"

	// Orca-specific extension codes. AWS S3 does not define these,
	// but the response shape is still a standard <Error> envelope so
	// SDKs that look only at the HTTP status (502) for retry
	// classification continue to behave correctly. Operators reading
	// orca logs can use the Code to distinguish failure modes.
	s3ErrOriginUnauthorized = "OriginUnauthorized"
	s3ErrOriginUnsupported  = "OriginUnsupported"
	s3ErrOriginETagChanged  = "OriginETagChanged"
	s3ErrOriginMissingETag  = "OriginMissingETag"
	s3ErrOriginUnreachable  = "OriginUnreachable"
)

// s3ErrorBody is the standard <Error> envelope returned by AWS S3 on
// non-2xx responses. Field order matches the AWS documented response.
// Optional fields are omitted when empty.
//
// Orca does not populate RequestId/HostId; SDKs tolerate their
// absence and there is presently no operational use for them in a
// single-cluster deployment.
type s3ErrorBody struct {
	XMLName    xml.Name `xml:"Error"`
	Code       string   `xml:"Code"`
	Message    string   `xml:"Message"`
	Resource   string   `xml:"Resource,omitempty"`
	BucketName string   `xml:"BucketName,omitempty"`
	Key        string   `xml:"Key,omitempty"`
}

// s3ErrorOpt customizes optional fields on the error envelope.
type s3ErrorOpt func(*s3ErrorBody)

// withBucketKey sets the <BucketName> and <Key> elements. Either may
// be empty.
func withBucketKey(bucket, key string) s3ErrorOpt {
	return func(b *s3ErrorBody) {
		b.BucketName = bucket
		b.Key = key
	}
}

// writeS3Error writes an S3-compatible error response.
//
// For HEAD requests the body is suppressed (mirroring real S3
// behavior: HEAD responses MUST NOT include a body per RFC 9110, and
// AWS S3 communicates the failure entirely via the status code and
// response headers).
//
// For all other methods an XML <Error> envelope is written with the
// supplied Code and Message. The envelope shape is the one S3 SDKs
// parse to extract a typed error code.
func writeS3Error(w http.ResponseWriter, r *http.Request, status int, code, message string, opts ...s3ErrorOpt) {
	w.Header().Set("Server", "orca")

	if r != nil && r.Method == http.MethodHead {
		// HEAD: status code + headers only.
		w.WriteHeader(status)
		return
	}

	body := s3ErrorBody{Code: code, Message: message}
	if r != nil {
		body.Resource = r.URL.Path
	}

	for _, opt := range opts {
		opt(&body)
	}

	xmlBytes, err := xml.Marshal(body)
	if err != nil {
		// Should be unreachable: s3ErrorBody is a fixed struct with
		// only string fields. Fall back to plain text on the off
		// chance the stdlib ever surprises us so the client at least
		// receives a meaningful status code + message.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(code + ": " + message)) //nolint:errcheck // best-effort fallback

		return
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header)) //nolint:errcheck // body write best-effort
	_, _ = w.Write(xmlBytes)           //nolint:errcheck // body write best-effort
}
