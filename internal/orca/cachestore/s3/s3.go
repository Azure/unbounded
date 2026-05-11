// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package s3 is the cachestore driver for in-DC S3-compatible stores.
// In production this targets VAST or another S3-compatible object
// store; in dev it targets LocalStack.
//
// Atomic commit is implemented via PutObject + If-None-Match: * (s3
// conditional writes). The boot SelfTestAtomicCommit verifies the
// backend honors the precondition; the boot versioning gate verifies
// the bucket is not versioned (since If-None-Match is not honored on
// versioned buckets).
package s3

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/Azure/unbounded/internal/orca/cachestore"
	"github.com/Azure/unbounded/internal/orca/chunk"
)

// Driver implements cachestore.CacheStore against an S3-compatible
// endpoint.
type Driver struct {
	client *s3.Client
	bucket string
}

// Config is the s3-driver configuration. Mirrors config.CachestoreS3
// but kept package-local so the driver can be unit-tested without
// importing the whole config package.
type Config struct {
	Endpoint     string
	Bucket       string
	Region       string
	AccessKey    string
	SecretKey    string
	UsePathStyle bool
}

// New constructs a Driver. The bucket-versioning gate is run here
// unconditionally: a versioned bucket silently breaks the no-clobber
// atomic-commit primitive (PutObject + If-None-Match: *) so the
// driver refuses to start against one.
//
// SelfTestAtomicCommit is a separate step (called by main after New)
// to keep the constructor side-effect-light.
func New(ctx context.Context, cfg Config) (*Driver, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("cachestore/s3: bucket required")
	}

	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("cachestore/s3: endpoint required")
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.AccessKey, cfg.SecretKey, "",
		)),
		// Opt out of CRC64NVME default introduced in aws-sdk-go-v2
		// 1.32. LocalStack 3.8 returns InvalidRequest for unknown
		// algorithms; real AWS S3 still works either way.
		awsconfig.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		awsconfig.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
	)
	if err != nil {
		return nil, fmt.Errorf("cachestore/s3: aws config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(cfg.Endpoint)
		o.UsePathStyle = cfg.UsePathStyle
	})

	d := &Driver{
		client: client,
		bucket: cfg.Bucket,
	}

	if err := d.versioningGate(ctx); err != nil {
		return nil, err
	}

	return d, nil
}

// versioningGate refuses to start if the bucket has versioning enabled
// or suspended. If-None-Match: * is not honored against versioned
// buckets, which would silently break atomic commit's no-clobber
// guarantee.
func (d *Driver) versioningGate(ctx context.Context) error {
	out, err := d.client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{
		Bucket: aws.String(d.bucket),
	})
	if err != nil {
		return fmt.Errorf("cachestore/s3: GetBucketVersioning failed: %w", err)
	}

	return validateBucketVersioning(d.bucket, out.Status)
}

// validateBucketVersioning returns an error if the bucket's versioning
// status is incompatible with cachestore/s3's atomic-commit primitive.
// Extracted as a pure function so unit tests can cover all branches
// (empty / Enabled / Suspended) without round-tripping to a real or
// emulated S3 backend.
func validateBucketVersioning(bucket string, status s3types.BucketVersioningStatus) error {
	switch status {
	case s3types.BucketVersioningStatusEnabled, s3types.BucketVersioningStatusSuspended:
		return fmt.Errorf(
			"cachestore/s3: bucket %s has versioning %s; If-None-Match: * is not "+
				"honored on versioned buckets and the atomic-commit primitive cannot "+
				"guarantee no-clobber; disable bucket versioning to use cachestore/s3",
			bucket, status)
	}

	return nil
}

// SelfTestAtomicCommit verifies the backend honors PutObject +
// If-None-Match: *.
func (d *Driver) SelfTestAtomicCommit(ctx context.Context) error {
	probeKey := fmt.Sprintf("_orca-selftest/%s", randHex(16))
	body := []byte("orca-selftest")

	// First put: must succeed.
	_, err := d.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(d.bucket),
		Key:         aws.String(probeKey),
		Body:        bytes.NewReader(body),
		IfNoneMatch: aws.String("*"),
	})
	if err != nil {
		return fmt.Errorf("cachestore/s3 self-test: first put failed: %w", err)
	}

	// Second put: must fail with 412.
	_, err = d.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(d.bucket),
		Key:         aws.String(probeKey),
		Body:        bytes.NewReader(body),
		IfNoneMatch: aws.String("*"),
	})
	if err == nil {
		// Clean up before returning the failure.
		_, _ = d.client.DeleteObject(ctx, &s3.DeleteObjectInput{ //nolint:errcheck // best-effort selftest cleanup
			Bucket: aws.String(d.bucket),
			Key:    aws.String(probeKey),
		})

		return fmt.Errorf(
			"cachestore/s3: backend does not honor If-None-Match: *; refusing to start " +
				"(second concurrent put returned 200 instead of 412)")
	}

	if !isPreconditionFailed(err) {
		_, _ = d.client.DeleteObject(ctx, &s3.DeleteObjectInput{ //nolint:errcheck // best-effort selftest cleanup
			Bucket: aws.String(d.bucket),
			Key:    aws.String(probeKey),
		})

		return fmt.Errorf("cachestore/s3 self-test: second put returned unexpected error "+
			"(want 412 PreconditionFailed): %w", err)
	}

	// Cleanup probe key.
	_, _ = d.client.DeleteObject(ctx, &s3.DeleteObjectInput{ //nolint:errcheck // best-effort selftest cleanup
		Bucket: aws.String(d.bucket),
		Key:    aws.String(probeKey),
	})

	return nil
}

// GetChunk fetches [off, off+n) of the chunk path from the bucket.
func (d *Driver) GetChunk(ctx context.Context, k chunk.Key, off, n int64) (io.ReadCloser, error) {
	rng := fmt.Sprintf("bytes=%d-%d", off, off+n-1)

	out, err := d.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(k.Path()),
		Range:  aws.String(rng),
	})
	if err != nil {
		return nil, mapErr(err)
	}

	return out.Body, nil
}

// PutChunk uploads the chunk via PutObject + If-None-Match: *. On
// 412 returns ErrCommitLost (loser of an atomic-commit race).
func (d *Driver) PutChunk(ctx context.Context, k chunk.Key, size int64, r io.Reader) error {
	// AWS SDK v2 needs an io.ReadSeeker for unsigned-payload uploads
	// (so it can rewind on signed-retry). If the caller already passed
	// a seekable reader we hand it to the SDK directly; otherwise
	// buffer the bytes ourselves as a fallback.
	body, ok := r.(io.ReadSeeker)
	if !ok {
		buf, err := io.ReadAll(r)
		if err != nil {
			return fmt.Errorf("cachestore/s3 put: read body: %w", err)
		}

		if int64(len(buf)) != size && size > 0 {
			return fmt.Errorf("cachestore/s3 put: short body (got %d want %d)", len(buf), size)
		}

		body = bytes.NewReader(buf)
	}

	_, err := d.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(d.bucket),
		Key:           aws.String(k.Path()),
		Body:          body,
		ContentLength: aws.Int64(size),
		IfNoneMatch:   aws.String("*"),
	})
	if err != nil {
		if isPreconditionFailed(err) {
			return cachestore.ErrCommitLost
		}

		return mapErr(err)
	}

	return nil
}

// Stat checks for chunk presence.
func (d *Driver) Stat(ctx context.Context, k chunk.Key) (cachestore.Info, error) {
	out, err := d.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(k.Path()),
	})
	if err != nil {
		return cachestore.Info{}, mapErr(err)
	}

	info := cachestore.Info{}
	if out.ContentLength != nil {
		info.Size = *out.ContentLength
	}

	if out.LastModified != nil {
		info.Committed = *out.LastModified
	}

	return info, nil
}

// Delete removes the chunk; idempotent.
func (d *Driver) Delete(ctx context.Context, k chunk.Key) error {
	_, err := d.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(k.Path()),
	})
	if err != nil {
		if isNotFound(err) {
			return nil
		}

		return mapErr(err)
	}

	return nil
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// Fallback: time-based; only used for boot-test probe key.
		return fmt.Sprintf("ts%d", time.Now().UnixNano())
	}

	return hex.EncodeToString(b)
}

// isPreconditionFailed reports whether err represents a 412
// Precondition Failed response from S3. The atomic-commit primitive
// (PutObject + If-None-Match: *) returns 412 when the key already
// exists; the SelfTest path also expects 412 on the duplicate put.
// We use the HTTP status code carried on *awshttp.ResponseError
// rather than matching service error codes by string, since the
// code surface is version-dependent across SDK and backend
// implementations whereas the HTTP status code is part of the
// stable wire contract.
func isPreconditionFailed(err error) bool {
	var respErr *awshttp.ResponseError
	if errors.As(err, &respErr) && respErr.Response != nil {
		return respErr.Response.StatusCode == http.StatusPreconditionFailed
	}

	return false
}

func isNotFound(err error) bool {
	var nsk *s3types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}

	var nsb *s3types.NoSuchBucket
	if errors.As(err, &nsb) {
		return true
	}

	var notFound *s3types.NotFound
	if errors.As(err, &notFound) {
		return true
	}

	var respErr *awshttp.ResponseError
	if errors.As(err, &respErr) && respErr.Response != nil &&
		respErr.Response.StatusCode == http.StatusNotFound {
		return true
	}

	return false
}

// mapErr normalises driver errors to the cachestore sentinel
// taxonomy. AccessDenied / Forbidden / Unauthorized are surfaced by
// the SDK with stable smithy.APIError codes so we keep that match
// path; everything else routes through HTTP status code on the
// underlying *awshttp.ResponseError.
func mapErr(err error) error {
	if isNotFound(err) {
		return cachestore.ErrNotFound
	}

	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "AccessDenied", "Unauthorized", "Forbidden", "InvalidAccessKeyId", "SignatureDoesNotMatch":
			return cachestore.ErrAuth
		}
	}

	var respErr *awshttp.ResponseError
	if errors.As(err, &respErr) && respErr.Response != nil {
		status := respErr.Response.StatusCode
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			return cachestore.ErrAuth
		}

		if status >= 500 && status < 600 {
			return cachestore.ErrTransient
		}
	}

	return err
}
