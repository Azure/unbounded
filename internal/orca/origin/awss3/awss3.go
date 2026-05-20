// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package awss3 is the AWS S3 (and S3-compatible) origin driver. It
// targets either real AWS S3 or a local S3-compatible endpoint such as
// LocalStack. Useful as a credential-free origin for the dev harness:
// LocalStack acts as both origin and cachestore (different buckets).
//
// This driver is read-only from Orca's perspective (Head, GetRange).
// The seed step that uploads test objects to the origin bucket
// happens out-of-band via aws-cli or similar.
package awss3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/Azure/unbounded/internal/orca/origin"
)

// Adapter implements origin.Origin against an S3-compatible endpoint.
type Adapter struct {
	cfg    Config
	client *s3.Client
	log    *slog.Logger
}

// Config is the awss3-driver configuration. Mirrors config.AWSS3 but
// kept package-local so the driver can be unit-tested without
// importing the whole config package.
type Config struct {
	// Endpoint, when set, overrides the regional default and routes
	// requests at a custom URL (LocalStack uses
	// http://localstack:4566). Leave empty for real AWS S3.
	Endpoint string

	// Region is the AWS region. LocalStack ignores this; the SDK
	// requires a value.
	Region string

	// Bucket is the source bucket holding origin objects.
	Bucket string

	// AccessKey / SecretKey are static credentials. For LocalStack
	// these are "test"/"test"; for real AWS, supply real creds.
	AccessKey string
	SecretKey string

	// UsePathStyle: true for LocalStack (host-based addressing
	// requires DNS wildcards LocalStack does not provide).
	UsePathStyle bool
}

// New constructs an Adapter. The log receives debug-level
// emissions for every Head / GetRange / List call and the error
// mapping decision (not-found / auth / precondition) on failure
// paths. Passing nil falls back to slog.Default().
func New(ctx context.Context, cfg Config, log *slog.Logger) (*Adapter, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("origin/awss3: bucket required")
	}

	if cfg.Region == "" {
		cfg.Region = "us-east-1"
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
		return nil, fmt.Errorf("origin/awss3: aws config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}

		o.UsePathStyle = cfg.UsePathStyle
	})

	if log == nil {
		log = slog.Default()
	}

	return &Adapter{cfg: cfg, client: client, log: log}, nil
}

// Head returns ObjectInfo for the named object. The bucket arg lets
// callers override the configured bucket; if empty, the configured
// bucket is used.
func (a *Adapter) Head(ctx context.Context, bucket, key string) (origin.ObjectInfo, error) {
	b := bucket
	if b == "" {
		b = a.cfg.Bucket
	}

	a.log.LogAttrs(ctx, slog.LevelDebug, "awss3_head_request",
		slog.String("bucket", b),
		slog.String("key", key),
	)

	out, err := a.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(b),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			a.log.LogAttrs(ctx, slog.LevelDebug, "awss3_head_not_found",
				slog.String("bucket", b),
				slog.String("key", key),
			)

			return origin.ObjectInfo{LastStatus: http.StatusNotFound}, origin.ErrNotFound
		}

		if isAuth(err) {
			a.log.LogAttrs(ctx, slog.LevelDebug, "awss3_head_auth",
				slog.String("bucket", b),
				slog.String("key", key),
			)

			return origin.ObjectInfo{}, origin.ErrAuth
		}

		return origin.ObjectInfo{}, fmt.Errorf("awss3 head: %w", err)
	}

	info := origin.ObjectInfo{LastStatus: http.StatusOK}
	if out.ContentLength != nil {
		info.Size = *out.ContentLength
	}

	if out.ETag != nil {
		info.ETag = strings.Trim(*out.ETag, "\"")
	}

	if out.ContentType != nil {
		info.ContentType = *out.ContentType
	}

	if out.LastModified != nil {
		info.LastValidated = *out.LastModified
	}

	a.log.LogAttrs(ctx, slog.LevelDebug, "awss3_head_response",
		slog.String("bucket", b),
		slog.String("key", key),
		slog.Int64("size", info.Size),
		slog.String("etag", origin.ETagShort(info.ETag)),
	)

	return info, nil
}

// GetRange fetches [off, off+n) of the object, sending If-Match: <etag>.
func (a *Adapter) GetRange(ctx context.Context, bucket, key, etag string, off, n int64) (io.ReadCloser, error) {
	b := bucket
	if b == "" {
		b = a.cfg.Bucket
	}

	rng := fmt.Sprintf("bytes=%d-%d", off, off+n-1)

	in := &s3.GetObjectInput{
		Bucket: aws.String(b),
		Key:    aws.String(key),
		Range:  aws.String(rng),
	}
	if etag != "" {
		// S3 expects the etag wrapped in double quotes.
		in.IfMatch = aws.String("\"" + etag + "\"")
	}

	a.log.LogAttrs(ctx, slog.LevelDebug, "awss3_get_range_request",
		slog.String("bucket", b),
		slog.String("key", key),
		slog.String("etag", origin.ETagShort(etag)),
		slog.Int64("off", off),
		slog.Int64("n", n),
	)

	out, err := a.client.GetObject(ctx, in)
	if err != nil {
		if isPreconditionFailed(err) {
			a.log.LogAttrs(ctx, slog.LevelDebug, "awss3_get_range_etag_changed",
				slog.String("bucket", b),
				slog.String("key", key),
				slog.String("want_etag", origin.ETagShort(etag)),
			)

			return nil, &origin.OriginETagChangedError{
				Bucket: b, Key: key, Want: etag,
			}
		}

		if isNotFound(err) {
			a.log.LogAttrs(ctx, slog.LevelDebug, "awss3_get_range_not_found",
				slog.String("bucket", b),
				slog.String("key", key),
			)

			return nil, origin.ErrNotFound
		}

		if isAuth(err) {
			a.log.LogAttrs(ctx, slog.LevelDebug, "awss3_get_range_auth",
				slog.String("bucket", b),
				slog.String("key", key),
			)

			return nil, origin.ErrAuth
		}

		return nil, fmt.Errorf("awss3 get-range: %w", err)
	}

	a.log.LogAttrs(ctx, slog.LevelDebug, "awss3_get_range_response",
		slog.String("bucket", b),
		slog.String("key", key),
	)

	return out.Body, nil
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

func isAuth(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "AccessDenied", "Unauthorized", "Forbidden", "InvalidAccessKeyId", "SignatureDoesNotMatch":
			return true
		}
	}

	var respErr *awshttp.ResponseError
	if errors.As(err, &respErr) && respErr.Response != nil {
		status := respErr.Response.StatusCode
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			return true
		}
	}

	return false
}

// isPreconditionFailed reports whether err carries an HTTP 412
// Precondition Failed response. Used to translate
// If-Match-rejected GetRange calls into the orca-internal
// OriginETagChangedError. We rely on the HTTP status code on the
// underlying *awshttp.ResponseError rather than service error
// codes; the status code is part of the stable wire contract
// across SDK and backend versions.
func isPreconditionFailed(err error) bool {
	var respErr *awshttp.ResponseError
	if errors.As(err, &respErr) && respErr.Response != nil {
		return respErr.Response.StatusCode == http.StatusPreconditionFailed
	}

	return false
}
