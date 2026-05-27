// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcadev

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// cacheObject names a single object in the cachestore enumerate
// output. The Path field is the raw cachestore object path; for orca
// this is `<origin_id>/<sha256-hex>/<index>`. Splitting back into the
// components is done by parseCachePath.
type cacheObject struct {
	Path         string
	Size         int64
	LastModified time.Time
	ETag         string
}

// cachestoreClient talks directly to the orca cachestore's S3
// endpoint, bypassing the in-process CacheStore interface (which
// has no List method and isn't reusable from a host-side tool).
//
// The same aws-sdk-go-v2 client shape orca itself uses (see
// internal/orca/cachestore/s3) is replicated here. The dev tool
// does not need atomic-commit semantics (PutObject + If-None-Match),
// just inspection-grade reads + bulk delete.
type cachestoreClient struct {
	cfg    *globalFlags
	client *s3.Client
}

// newCachestoreClient constructs a client from the resolved global
// flags. Mirrors the orca cachestore driver's SDK configuration:
// path-style addressing, checksum opt-out for LocalStack 3.8
// compatibility, static credentials.
func newCachestoreClient(ctx context.Context, g *globalFlags) (*cachestoreClient, error) {
	if g.cachestoreBucket == "" {
		return nil, fmt.Errorf("cachestore: --cachestore-bucket required")
	}

	client, err := buildS3Client(ctx,
		g.cachestoreRegion,
		g.cachestoreAccessKey, g.cachestoreSecretKey,
		g.cachestoreEndpoint,
		g.cachestoreUsePathStyle,
	)
	if err != nil {
		return nil, fmt.Errorf("cachestore: %w", err)
	}

	return &cachestoreClient{cfg: g, client: client}, nil
}

// buildS3Client constructs an aws-sdk-go-v2 S3 client matching
// orca's own configuration: static credentials and the
// LocalStack-3.8 compatible request/response checksum opt-out
// (CRC64NVME breaks against LocalStack and orca itself opts out
// when talking to its S3 surfaces). When endpoint is non-empty it
// is set as the BaseEndpoint, and usePathStyle is forwarded so
// LocalStack-style backends work without DNS gymnastics.
func buildS3Client(ctx context.Context, region, accessKey, secretKey, endpoint string, usePathStyle bool) (*s3.Client, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			accessKey, secretKey, "",
		)),
		awsconfig.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		awsconfig.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
	)
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}

	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}

		o.UsePathStyle = usePathStyle
	}), nil
}

// List enumerates objects whose path starts with prefix. Returns up
// to limit entries; pass 0 to read everything.
func (c *cachestoreClient) List(ctx context.Context, prefix string, limit int) ([]cacheObject, error) {
	var out []cacheObject

	err := walkS3(ctx, c.client, c.cfg.cachestoreBucket, prefix, func(obj s3types.Object) bool {
		o := cacheObject{}
		if obj.Key != nil {
			o.Path = *obj.Key
		}

		if obj.Size != nil {
			o.Size = *obj.Size
		}

		if obj.LastModified != nil {
			o.LastModified = *obj.LastModified
		}

		if obj.ETag != nil {
			o.ETag = *obj.ETag
		}

		out = append(out, o)

		return limit <= 0 || len(out) < limit
	})
	if err != nil {
		return nil, fmt.Errorf("cachestore: list: %w", err)
	}

	return out, nil
}

// walkS3 iterates an S3 bucket's objects matching prefix using
// ListObjectsV2 pagination. The visit callback is invoked once per
// object; returning false short-circuits the walk (used to honor a
// caller-supplied result limit).
func walkS3(
	ctx context.Context,
	client *s3.Client,
	bucket, prefix string,
	visit func(s3types.Object) bool,
) error {
	var next *string

	for {
		page, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: next,
		})
		if err != nil {
			return err
		}

		for _, obj := range page.Contents {
			if !visit(obj) {
				return nil
			}
		}

		if page.IsTruncated == nil || !*page.IsTruncated {
			return nil
		}

		next = page.NextContinuationToken
	}
}

// Head returns size + last-modified for a single chunk path. Returns
// an error wrapping ErrCacheNotFound when the chunk is absent.
func (c *cachestoreClient) Head(ctx context.Context, path string) (cacheObject, error) {
	out, err := c.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.cfg.cachestoreBucket),
		Key:    aws.String(path),
	})
	if err != nil {
		var ae smithy.APIError
		if errors.As(err, &ae) {
			code := ae.ErrorCode()
			if code == "NotFound" || code == "NoSuchKey" {
				return cacheObject{}, ErrCacheNotFound
			}
		}

		return cacheObject{}, fmt.Errorf("cachestore: head %s: %w", path, err)
	}

	o := cacheObject{Path: path}
	if out.ContentLength != nil {
		o.Size = *out.ContentLength
	}

	if out.LastModified != nil {
		o.LastModified = *out.LastModified
	}

	if out.ETag != nil {
		o.ETag = *out.ETag
	}

	return o, nil
}

// Delete removes a single chunk path. Missing-on-delete is NOT an
// error (idempotent).
func (c *cachestoreClient) Delete(ctx context.Context, path string) error {
	_, err := c.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.cfg.cachestoreBucket),
		Key:    aws.String(path),
	})
	if err != nil {
		return fmt.Errorf("cachestore: delete %s: %w", path, err)
	}

	return nil
}

// ErrCacheNotFound is returned by Head when the chunk is absent.
// Used by `cache inspect` to render "no" vs an error.
var ErrCacheNotFound = errors.New("cache: chunk not found")

// cachestoreOps is the minimal subset of cachestoreClient that
// scenarios use to interact with the cachestore. Declared as an
// interface so tests can inject an in-memory fake without standing
// up a real S3 endpoint. The production *cachestoreClient satisfies
// this interface implicitly.
type cachestoreOps interface {
	Head(ctx context.Context, path string) (cacheObject, error)
	Delete(ctx context.Context, path string) error
}

// Compile-time check that the production client satisfies the ops
// interface used by scenarios.
var _ cachestoreOps = (*cachestoreClient)(nil)
