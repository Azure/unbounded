// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integrationtest

package inttest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// newOrcaS3Client returns an aws-sdk-go-v2 S3 client configured to
// talk to orca's edge listener. Static dummy credentials are supplied
// because the SDK refuses to run without any, but orca currently
// ignores the Authorization header (auth.enabled=false). Path-style
// addressing is required: orca does not implement virtual-hosted
// bucket parsing. Retries are disabled so test failures surface
// immediately rather than after exponential backoff.
//
// Checksum opt-out mirrors the same setting used by orca's own
// awss3 origin adapter: aws-sdk-go-v2 added default checksum
// computation that some servers (including orca's edge, which does
// not echo CRC headers) reject or ignore in confusing ways.
func newOrcaS3Client(t *testing.T, baseURL string) *s3.Client {
	t.Helper()

	cfg, err := awsconfig.LoadDefaultConfig(t.Context(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider("orca-test", "orca-test", ""),
		),
	)
	if err != nil {
		t.Fatalf("aws config: %v", err)
	}

	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(baseURL)
		o.UsePathStyle = true
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		o.RetryMaxAttempts = 1
	})
}

// TestS3SDK verifies that orca's edge surface is consumable by a
// real S3 SDK (aws-sdk-go-v2). This is the headline contract test
// for the XML <Error> envelope: TestS3Errors confirms the bytes on
// the wire are well-formed, while this suite confirms the SDK
// successfully unmarshals them into typed errors (*s3types.NoSuchKey,
// *s3types.NotFound) or surfaces the Code via smithy.APIError.
//
// Regression here means external S3 clients (CLI, boto3, MinIO,
// Java SDK, etc.) cannot do typed error handling against orca even
// though the response bytes look correct.
//
// A single cluster is shared across all subtests; subtests are
// independent and run in parallel.
func TestS3SDK(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	// Parallel subtests are paused until the parent returns, then
	// resumed; a `defer cancel()` would cancel the context before
	// they ever run. Use t.Cleanup so the cancel fires after all
	// subtests finish instead.
	t.Cleanup(cancel)

	bucket := pkgLocalStack.NewBucket(ctx, t, "orca-origin")
	blob := SmallBlob()
	SeedS3(ctx, t, pkgLocalStack.NewS3Client(ctx, t), bucket, []SeedBlob{blob})

	cl := StartCluster(ctx, t, ClusterOptions{
		LocalStack:   pkgLocalStack,
		OriginBucket: bucket,
	})

	client := newOrcaS3Client(t, cl.Get(1).HTTP.BaseURL)

	// GetObject_Success: positive control. If this fails the rest
	// of the suite's error assertions cannot be trusted.
	t.Run("GetObject_Success", func(t *testing.T) {
		t.Parallel()

		out, err := client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(blob.Key),
		})
		if err != nil {
			t.Fatalf("GetObject: %v", err)
		}
		defer out.Body.Close() //nolint:errcheck // body close best-effort in tests

		body, err := io.ReadAll(out.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}

		if !bytes.Equal(body, blob.Data) {
			t.Fatalf("body mismatch: got %d bytes, want %d", len(body), len(blob.Data))
		}

		if out.ETag == nil || *out.ETag == "" {
			t.Error("ETag missing on successful GetObject")
		}
	})

	// HeadObject_Size_TypedFields: verifies HEAD on a present
	// object returns the correct object size via the SDK's typed
	// *out.ContentLength field. Prefetchers and range planners
	// depend on this; a regression in setObjectHeaders or the
	// Content-Length formatting in handleHead would silently break
	// clients that key off the typed field rather than re-parsing
	// the header.
	t.Run("HeadObject_Size_TypedFields", func(t *testing.T) {
		t.Parallel()

		out, err := client.HeadObject(ctx, &s3.HeadObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(blob.Key),
		})
		if err != nil {
			t.Fatalf("HeadObject: %v", err)
		}

		if out.ContentLength == nil {
			t.Fatal("ContentLength is nil")
		}

		if got, want := *out.ContentLength, int64(len(blob.Data)); got != want {
			t.Errorf("ContentLength=%d want %d", got, want)
		}

		if out.ETag == nil || *out.ETag == "" {
			t.Error("ETag missing on HeadObject success")
		}
	})

	// GetObject_NoSuchKey_TypedError: the headline assertion. SDK
	// must surface *s3types.NoSuchKey, which requires orca to emit
	// a well-formed <Error><Code>NoSuchKey</Code></Error> body with
	// the right Content-Type.
	t.Run("GetObject_NoSuchKey_TypedError", func(t *testing.T) {
		t.Parallel()

		_, err := client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String("does-not-exist"),
		})
		if err == nil {
			t.Fatal("GetObject on missing key returned no error")
		}

		var nsk *s3types.NoSuchKey
		if !errors.As(err, &nsk) {
			t.Fatalf("err is not *s3types.NoSuchKey: %T: %v", err, err)
		}
	})

	// HeadObject_NotFound_TypedError: HEAD has an empty body, so
	// the SDK relies on the 404 status to surface *s3types.NotFound.
	// This is the contract test for the HEAD-no-body branch in
	// writeS3Error: if we ever accidentally write a body on HEAD,
	// some SDKs would fail to parse the (empty) Content-Type
	// response and surface a generic error instead.
	t.Run("HeadObject_NotFound_TypedError", func(t *testing.T) {
		t.Parallel()

		_, err := client.HeadObject(ctx, &s3.HeadObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String("does-not-exist"),
		})
		if err == nil {
			t.Fatal("HeadObject on missing key returned no error")
		}

		var nf *s3types.NotFound
		if !errors.As(err, &nf) {
			t.Fatalf("err is not *s3types.NotFound: %T: %v", err, err)
		}
	})

	// GetObject_InvalidRange_APICode: there is no typed
	// *s3types.InvalidRange error in aws-sdk-go-v2, so SDK callers
	// extract the code via smithy.APIError. Verify the Code reaches
	// them intact.
	t.Run("GetObject_InvalidRange_APICode", func(t *testing.T) {
		t.Parallel()

		_, err := client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(blob.Key),
			Range:  aws.String("bytes=99999999-"),
		})
		if err == nil {
			t.Fatal("GetObject with out-of-range Range returned no error")
		}

		var apiErr smithy.APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("err is not smithy.APIError: %T: %v", err, err)
		}

		if apiErr.ErrorCode() != "InvalidRange" {
			t.Errorf("ErrorCode=%q want InvalidRange", apiErr.ErrorCode())
		}
	})

	// GetObject_RangeRequest: positive range; verifies the SDK's
	// success-path 206 handling round-trips correctly through
	// orca's range slicing.
	t.Run("GetObject_RangeRequest", func(t *testing.T) {
		t.Parallel()

		const n = 100
		out, err := client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(blob.Key),
			Range:  aws.String("bytes=0-99"),
		})
		if err != nil {
			t.Fatalf("GetObject range: %v", err)
		}
		defer out.Body.Close() //nolint:errcheck // body close best-effort in tests

		body, err := io.ReadAll(out.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}

		if !bytes.Equal(body, blob.Data[:n]) {
			t.Fatalf("range body mismatch: got %d bytes, want %d", len(body), n)
		}

		if out.ContentRange == nil || !strings.HasPrefix(*out.ContentRange, "bytes 0-99/") {
			t.Errorf("ContentRange=%v want bytes 0-99/...", out.ContentRange)
		}
	})

	// ListObjectsV2_NotImplemented: verifies SDK surfaces orca's
	// 501 NotImplemented response for the bucket-level GET via
	// smithy.APIError with Code=NotImplemented.
	//
	// We intentionally do not assert ErrorFault: aws-sdk-go-v2
	// classifies the fault from the (untyped) deserialized error,
	// which for our XML envelope produces Fault=unknown rather than
	// server. The status code (501) is what S3 client retry
	// classifiers actually consult, and that path is exercised via
	// TestS3Errors.
	t.Run("ListObjectsV2_NotImplemented", func(t *testing.T) {
		t.Parallel()

		_, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket: aws.String(bucket),
		})
		if err == nil {
			t.Fatal("ListObjectsV2 returned no error")
		}

		var apiErr smithy.APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("err is not smithy.APIError: %T: %v", err, err)
		}

		if apiErr.ErrorCode() != "NotImplemented" {
			t.Errorf("ErrorCode=%q want NotImplemented", apiErr.ErrorCode())
		}
	})

	// PutObject_MethodNotAllowed: orca is read-only. Verify the
	// SDK surfaces 405 MethodNotAllowed cleanly so write attempts
	// fail with a recognizable code rather than a parse error or
	// silent retry storm.
	t.Run("PutObject_MethodNotAllowed", func(t *testing.T) {
		t.Parallel()

		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String("new-object"),
			Body:   bytes.NewReader([]byte("hello")),
		})
		if err == nil {
			t.Fatal("PutObject returned no error against read-only orca")
		}

		var apiErr smithy.APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("err is not smithy.APIError: %T: %v", err, err)
		}

		if apiErr.ErrorCode() != "MethodNotAllowed" {
			t.Errorf("ErrorCode=%q want MethodNotAllowed", apiErr.ErrorCode())
		}
	})
}
