// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racerobject

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"

	"github.com/Azure/unbounded/pkg/racersdk"
)

// NewAzureOrigin reads block blobs without caching. Schema-1 Bucket names the
// container, which must equal config.Bucket. Use a separate cache for each
// provider, account, and container.
func NewAzureOrigin(client *service.Client, config OriginConfig) (racersdk.Origin, error) {
	if client == nil || config.MetadataTTL < 0 {
		return nil, racersdk.ErrInvalidRequest
	}

	if !validBucket(config.Bucket) {
		return nil, racersdk.ErrInvalidRequest
	}

	origin := &azureOrigin{client: client, bucket: config.Bucket, ttl: config.MetadataTTL}

	return origin.read, nil
}

type azureOrigin struct {
	client *service.Client
	bucket string
	ttl    time.Duration
}

func (o *azureOrigin) read(ctx context.Context, request racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return racersdk.Metadata{}, nil, classifyAzureError(err, false)
	}

	object, err := decodeObject(request, o.bucket)
	if err != nil {
		return racersdk.Metadata{}, nil, err
	}

	// NewBlobClient escapes the whole key without cleaning its path segments.
	client := o.client.NewContainerClient(object.Bucket).NewBlobClient(object.Key)
	if object.VersionID != "" {
		// azblob 1.8 appends this value to the query without escaping it.
		client, err = client.WithVersionID(url.QueryEscape(object.VersionID))
		if err != nil {
			return racersdk.Metadata{}, nil, classifyAzureError(err, request.ETag != "")
		}
	}

	head, err := client.GetProperties(ctx, &blob.GetPropertiesOptions{AccessConditions: azureConditions(request.ETag)})
	if err != nil {
		return racersdk.Metadata{}, nil, classifyAzureError(err, request.ETag != "")
	}

	metadata, err := o.headMetadata(head, object)
	if err != nil {
		return racersdk.Metadata{}, nil, err
	}

	// Recheck the pin even if the service ignored If-Match.
	if request.ETag != "" && request.ETag != metadata.ETag {
		return metadata, nil, racersdk.ErrVersionMismatch
	}

	if request.Head {
		return metadata, nil, nil
	}

	body, err := o.readPage(ctx, request, client, head, metadata)

	return metadata, body, err
}

func azureConditions(tag string) *blob.AccessConditions {
	if tag == "" {
		return nil
	}

	return &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfMatch: to.Ptr(azcore.ETag(tag))}}
}

var errInvalidAzureResponse = errors.New("racer-object: invalid Azure Blob response")

func (o *azureOrigin) headMetadata(head blob.GetPropertiesResponse, object Object) (racersdk.Metadata, error) {
	if head.ContentLength == nil || *head.ContentLength < 0 || azureValue(head.BlobType) != blob.BlobTypeBlockBlob ||
		(object.VersionID != "" && azureValue(head.VersionID) != object.VersionID) ||
		azureValue(head.AccessTier) == "Archive" || azureValue(head.ArchiveStatus) != "" {
		return racersdk.Metadata{}, errInvalidAzureResponse
	}

	// SDK metadata cannot carry Content-Encoding to the reader.
	if encoding := azureValue(head.ContentEncoding); encoding != "" && encoding != "identity" {
		return racersdk.Metadata{}, errInvalidAzureResponse
	}

	tag := string(azureValue(head.ETag))
	if !validS3ETag(tag) {
		return racersdk.Metadata{}, errInvalidAzureResponse
	}

	return racersdk.Metadata{
		Size: *head.ContentLength, ETag: tag, ContentType: azureValue(head.ContentType),
		ExpiresAt: time.Now().Add(o.ttl).Truncate(time.Millisecond),
	}, nil
}

func (o *azureOrigin) readPage(ctx context.Context, request racersdk.OriginRequest, client *blob.Client, head blob.GetPropertiesResponse, metadata racersdk.Metadata) (io.ReadCloser, error) {
	if request.Offset < 0 || request.Offset%racersdk.PageSize != 0 || request.Length <= 0 || request.Length > racersdk.PageSize {
		return nil, racersdk.ErrInvalidRequest
	}

	// The SDK handles empty objects and pages outside the object.
	if request.Offset >= metadata.Size {
		return nil, nil
	}

	first := request.Offset
	count := min(request.Length, metadata.Size-first)

	var raw *http.Response

	output, err := client.DownloadStream(policy.WithCaptureResponse(ctx, &raw), &blob.DownloadStreamOptions{
		Range: blob.HTTPRange{Offset: first, Count: count}, AccessConditions: azureConditions(metadata.ETag),
	})
	if err != nil {
		// azblob 1.8 drops the unread body when success headers fail to decode.
		// Error-status bodies are already closed by the Azure pipeline.
		if output.Body == nil && raw != nil && (raw.StatusCode == http.StatusOK || raw.StatusCode == http.StatusPartialContent) && azureHeaderDecodeError(err) {
			return raw.Body, classifyAzureError(err, true)
		}

		return output.Body, classifyAzureError(err, true)
	}

	// Transfer rejected bodies too. The SDK closes them and checks byte counts.
	// Do not use a retry reader, which could hide changed response metadata.
	return output.Body, validateAzurePage(output, head, metadata, first, first+count-1)
}

func azureHeaderDecodeError(err error) bool {
	var (
		number      *strconv.NumError
		date        *time.ParseError
		base64Error base64.CorruptInputError
	)

	return errors.As(err, &number) || errors.As(err, &date) || errors.As(err, &base64Error)
}

func validateAzurePage(output blob.DownloadStreamResponse, head blob.GetPropertiesResponse, metadata racersdk.Metadata, first, last int64) error {
	tag := string(azureValue(output.ETag))
	if !validS3ETag(tag) {
		return errInvalidAzureResponse
	}

	if tag != metadata.ETag {
		return racersdk.ErrVersionMismatch
	}

	if output.Body == nil || output.ContentLength == nil || *output.ContentLength != last-first+1 ||
		azureValue(output.ContentRange) != fmt.Sprintf("bytes %d-%d/%d", first, last, metadata.Size) ||
		azureValue(output.ContentType) != metadata.ContentType || azureValue(output.VersionID) != azureValue(head.VersionID) ||
		azureValue(output.ContentEncoding) != azureValue(head.ContentEncoding) || azureValue(output.BlobType) != blob.BlobTypeBlockBlob {
		return errInvalidAzureResponse
	}

	return nil
}

func azureValue[T any](p *T) (value T) {
	if p != nil {
		return *p
	}

	return value
}

func classifyAzureError(err error, pinned bool) error {
	classification := azureError(err)
	if pinned && classification == racersdk.ErrNotFound {
		classification = racersdk.ErrVersionMismatch
	}

	return fmt.Errorf("%w: %w", classification, privateAzureError{err})
}

// Hide upstream URLs and credentials while preserving the cause for errors.Is.
type privateAzureError struct{ cause error }

func (e privateAzureError) Error() string { return "Azure Blob request failed" }
func (e privateAzureError) Unwrap() error { return e.cause }

func azureError(err error) error {
	var (
		response *azcore.ResponseError
		auth     *azidentity.AuthenticationFailedError
		required *azidentity.AuthenticationRequiredError
		urlError *url.Error
		network  net.Error
	)

	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.As(err, &auth):
		if auth.RawResponse != nil && azureHTTPError(auth.RawResponse.StatusCode) == racersdk.ErrUnavailable {
			return racersdk.ErrUnavailable
		}

		return racersdk.ErrUnauthorized
	case errors.As(err, &required):
		return racersdk.ErrUnauthorized
	case errors.As(err, &response):
		switch bloberror.Code(response.ErrorCode) {
		case bloberror.BlobNotFound, bloberror.ContainerNotFound, bloberror.ResourceNotFound:
			return racersdk.ErrNotFound
		case bloberror.AuthenticationFailed, bloberror.InvalidAuthenticationInfo, bloberror.NoAuthenticationInformation:
			return racersdk.ErrUnauthorized
		case bloberror.AuthorizationFailure, bloberror.AuthorizationPermissionMismatch, bloberror.AuthorizationProtocolMismatch,
			bloberror.AuthorizationResourceTypeMismatch, bloberror.AuthorizationServiceMismatch, bloberror.AuthorizationSourceIPMismatch,
			bloberror.InsufficientAccountPermissions, bloberror.AccountIsDisabled:
			return racersdk.ErrForbidden
		case bloberror.ConditionNotMet, bloberror.TargetConditionNotMet, bloberror.SourceConditionNotMet, bloberror.BlobOverwritten:
			return racersdk.ErrVersionMismatch
		case bloberror.ServerBusy, bloberror.InternalError, bloberror.OperationTimedOut:
			return racersdk.ErrUnavailable
		case bloberror.BlobArchived, bloberror.BlobBeingRehydrated:
			return errInvalidAzureResponse
		default:
			return azureHTTPError(response.StatusCode)
		}
	case errors.As(err, &urlError):
		// URL errors can wrap malformed HTTP responses, not just network errors.
		return azureError(urlError.Err)
	case errors.As(err, &network), errors.Is(err, net.ErrClosed), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return racersdk.ErrUnavailable
	default:
		return errInvalidAzureResponse
	}
}

func azureHTTPError(status int) error {
	switch {
	case status == 401:
		return racersdk.ErrUnauthorized
	case status == 403:
		return racersdk.ErrForbidden
	case status == 404:
		return racersdk.ErrNotFound
	case status == 412:
		return racersdk.ErrVersionMismatch
	case status == 408 || status == 429 || status >= 500 && status <= 599:
		return racersdk.ErrUnavailable
	default:
		return errInvalidAzureResponse
	}
}
