// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racerobject

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/Azure/unbounded/pkg/racersdk"
)

// S3Client is the read-only subset of the AWS v2 S3 client used by the origin.
type S3Client interface {
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// OriginConfig binds requests to one upstream namespace and optional buckets.
type OriginConfig struct {
	Namespace string
	Buckets   []string
	// MetadataTTL is an admission hint. Zero expires metadata immediately.
	MetadataTTL time.Duration
}

// NewOrigin serves full metadata and conditional page reads without caching.
// The client must return bodies whose Close interrupts Read, as the AWS client does.
func NewOrigin(client S3Client, config OriginConfig) (racersdk.Origin, error) {
	if client == nil || (reflect.ValueOf(client).Kind() == reflect.Pointer && reflect.ValueOf(client).IsNil()) ||
		!validNamespace(config.Namespace) || config.MetadataTTL < 0 {
		return nil, racersdk.NewOriginError(racersdk.ErrorInvalidArgument, nil)
	}

	buckets := make(map[string]struct{}, len(config.Buckets))
	for _, bucket := range config.Buckets {
		if !validBucket(bucket) {
			return nil, racersdk.NewOriginError(racersdk.ErrorInvalidArgument, nil)
		}

		buckets[bucket] = struct{}{}
	}

	origin := &objectOrigin{client: client, namespace: config.Namespace, buckets: buckets, ttl: config.MetadataTTL}

	return origin.read, nil
}

type objectOrigin struct {
	client    S3Client
	namespace string
	buckets   map[string]struct{}
	ttl       time.Duration
}

func (o *objectOrigin) read(ctx context.Context, request racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return racersdk.Metadata{}, nil, classifyS3Error(err, false)
	}

	object, err := decodeObject(request, o.namespace, o.buckets)
	if err != nil {
		return racersdk.Metadata{}, nil, err
	}

	if request.Operation() != racersdk.OperationHead && request.Operation() != racersdk.OperationBootstrap && request.Operation() != racersdk.OperationPinned {
		return racersdk.Metadata{}, nil, racersdk.NewOriginError(racersdk.ErrorInvalidArgument, nil)
	}

	pin, pinned := request.Pin()

	input := &s3.HeadObjectInput{Bucket: aws.String(object.Bucket), Key: aws.String(object.Key)}
	if object.VersionID != "" {
		input.VersionId = aws.String(object.VersionID)
	}

	if pinned {
		input.IfMatch = aws.String(pin.String())
	}

	head, err := o.client.HeadObject(ctx, input)
	if err != nil {
		return racersdk.Metadata{}, nil, classifyS3Error(err, pinned)
	}

	metadata, err := o.headMetadata(head, object)
	if err != nil {
		return racersdk.Metadata{}, nil, err
	}

	if pinned && metadata.ETag != pin {
		return metadata, nil, racersdk.NewOriginError(racersdk.ErrorVersionUnavailable, nil)
	}

	if request.Operation() == racersdk.OperationHead {
		return metadata, nil, nil
	}

	page, present := request.Range()
	if !present {
		return metadata, nil, racersdk.NewOriginError(racersdk.ErrorInvalidArgument, nil)
	}

	// The SDK permits an empty bootstrap, but no pinned page of an empty object.
	if metadata.Size == 0 && request.Operation() == racersdk.OperationBootstrap {
		return metadata, nil, nil
	}

	first, last, err := page.Resolve(metadata.Size)
	if err != nil {
		return metadata, nil, err
	}

	get := &s3.GetObjectInput{
		Bucket: input.Bucket, Key: input.Key, VersionId: input.VersionId,
		IfMatch: aws.String(metadata.ETag.String()), Range: aws.String(fmt.Sprintf("bytes=%d-%d", first, last)),
	}

	output, err := o.client.GetObject(ctx, get)
	if err != nil {
		return metadata, responseBody(output), classifyS3Error(err, true)
	}

	if err := validateObjectPage(output, head, metadata, first, last); err != nil {
		// Transfer even a rejected body to the SDK, which closes it exactly once.
		return metadata, responseBody(output), err
	}

	// Do not limit the reader: the SDK must detect short and excess bodies.
	return metadata, output.Body, nil
}

func (o *objectOrigin) headMetadata(head *s3.HeadObjectOutput, object Object) (racersdk.Metadata, error) {
	if head == nil || head.ContentLength == nil || *head.ContentLength < 0 || aws.ToBool(head.DeleteMarker) ||
		aws.ToString(head.ContentRange) != "" || (object.VersionID != "" && aws.ToString(head.VersionId) != object.VersionID) {
		return racersdk.Metadata{}, racersdk.NewOriginError(racersdk.ErrorBadGateway, nil)
	}

	// SDK metadata cannot carry Content-Encoding to the reader.
	if encoding := aws.ToString(head.ContentEncoding); encoding != "" && encoding != "identity" {
		return racersdk.Metadata{}, racersdk.NewOriginError(racersdk.ErrorBadGateway, nil)
	}

	tag, err := racersdk.ParseETag(aws.ToString(head.ETag))
	if err != nil {
		return racersdk.Metadata{}, racersdk.NewOriginError(racersdk.ErrorBadGateway, err)
	}

	metadata := racersdk.Metadata{
		Size: racersdk.ByteLength(*head.ContentLength), ETag: tag,
		ExpiresAt: time.Now().Add(o.ttl).Truncate(time.Millisecond), ContentType: aws.ToString(head.ContentType),
	}
	if err := metadata.Validate(); err != nil {
		return racersdk.Metadata{}, racersdk.NewOriginError(racersdk.ErrorBadGateway, err)
	}

	return metadata, nil
}

func responseBody(output *s3.GetObjectOutput) io.ReadCloser {
	if output == nil {
		return nil
	}

	return output.Body
}

func validateObjectPage(output *s3.GetObjectOutput, head *s3.HeadObjectOutput, metadata racersdk.Metadata, first, last racersdk.ByteOffset) error {
	if output == nil {
		return racersdk.NewOriginError(racersdk.ErrorBadGateway, nil)
	}

	tag, err := racersdk.ParseETag(aws.ToString(output.ETag))
	if err != nil {
		return racersdk.NewOriginError(racersdk.ErrorBadGateway, err)
	}

	if tag != metadata.ETag {
		return racersdk.NewOriginError(racersdk.ErrorVersionUnavailable, nil)
	}

	if output.Body == nil || output.ContentLength == nil || *output.ContentLength != int64(last-first+1) ||
		aws.ToString(output.ContentRange) != fmt.Sprintf("bytes %d-%d/%d", first, last, metadata.Size) ||
		aws.ToString(output.ContentType) != metadata.ContentType || aws.ToString(output.VersionId) != aws.ToString(head.VersionId) ||
		aws.ToString(output.ContentEncoding) != aws.ToString(head.ContentEncoding) || aws.ToBool(output.DeleteMarker) {
		return racersdk.NewOriginError(racersdk.ErrorBadGateway, nil)
	}

	return nil
}

func classifyS3Error(err error, pinned bool) error {
	kind := racersdk.ErrorBadGateway

	var (
		api    smithy.APIError
		status interface{ HTTPStatusCode() int }
	)

	switch {
	case errors.Is(err, context.Canceled):
		kind = racersdk.ErrorCanceled
	case errors.Is(err, context.DeadlineExceeded):
		kind = racersdk.ErrorDeadline
	case errors.As(err, &api):
		switch api.ErrorCode() {
		case "NoSuchKey", "NoSuchBucket", "NoSuchVersion", "NotFound":
			kind = racersdk.ErrorNotFound
		case "AccessDenied", "InvalidAccessKeyId", "SignatureDoesNotMatch", "ExpiredToken", "InvalidToken", "TokenRefreshRequired":
			kind = racersdk.ErrorForbidden
		case "PreconditionFailed":
			kind = racersdk.ErrorVersionUnavailable
		case "SlowDown", "ServiceUnavailable", "InternalError", "RequestTimeout", "Throttling", "ThrottlingException":
			kind = racersdk.ErrorUnavailable
		}
	}

	if kind == racersdk.ErrorBadGateway && errors.As(err, &status) {
		switch code := status.HTTPStatusCode(); {
		case code == 401:
			kind = racersdk.ErrorUnauthorized
		case code == 403:
			kind = racersdk.ErrorForbidden
		case code == 404:
			kind = racersdk.ErrorNotFound
		case code == 412:
			kind = racersdk.ErrorVersionUnavailable
		case code == 408 || code == 429 || code >= 500:
			kind = racersdk.ErrorUnavailable
		}
	}

	// An upstream 416 contradicts the range resolved from HEAD, not caller input.
	if pinned && kind == racersdk.ErrorNotFound {
		kind = racersdk.ErrorVersionUnavailable
	}

	return racersdk.NewOriginError(kind, err)
}
