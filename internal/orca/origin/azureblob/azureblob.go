// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package azureblob is the Azure Blob Storage adapter for the Origin
// interface. Block Blobs only (design.md s9).
package azureblob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	"github.com/Azure/unbounded/internal/orca/config"
	"github.com/Azure/unbounded/internal/orca/origin"
)

// Adapter implements origin.Origin against Azure Blob Storage.
type Adapter struct {
	cfg    config.Azureblob
	client *azblob.Client
}

// New builds an Adapter from config.
func New(cfg config.Azureblob) (*Adapter, error) {
	if cfg.Account == "" {
		return nil, fmt.Errorf("azureblob: account required")
	}

	if cfg.AccountKey == "" {
		return nil, fmt.Errorf("azureblob: account_key required")
	}

	cred, err := azblob.NewSharedKeyCredential(cfg.Account, cfg.AccountKey)
	if err != nil {
		return nil, fmt.Errorf("azureblob: shared-key credential: %w", err)
	}

	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = fmt.Sprintf("https://%s.blob.core.windows.net/", cfg.Account)
	}

	client, err := azblob.NewClientWithSharedKeyCredential(endpoint, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("azureblob: client: %w", err)
	}

	return &Adapter{cfg: cfg, client: client}, nil
}

// Head returns ObjectInfo for the named blob.
//
// "bucket" maps to the configured container; the bucket arg is honored
// only if non-empty (allowing single-container deployments to use the
// configured container as the default).
func (a *Adapter) Head(ctx context.Context, bucket, key string) (origin.ObjectInfo, error) {
	cName := bucket
	if cName == "" {
		cName = a.cfg.Container
	}

	props, err := a.client.ServiceClient().NewContainerClient(cName).
		NewBlobClient(key).GetProperties(ctx, nil)
	if err != nil {
		if isNotFound(err) {
			return origin.ObjectInfo{LastStatus: http.StatusNotFound}, origin.ErrNotFound
		}

		if isAuth(err) {
			return origin.ObjectInfo{}, origin.ErrAuth
		}

		return origin.ObjectInfo{}, fmt.Errorf("azureblob head: %w", err)
	}

	if err := validateBlobType(a.cfg.EnforceBlockBlobOnly, cName, key, props.BlobType); err != nil {
		return origin.ObjectInfo{}, err
	}

	info := origin.ObjectInfo{LastStatus: http.StatusOK}
	if props.ContentLength != nil {
		info.Size = *props.ContentLength
	}

	if props.ETag != nil {
		info.ETag = strings.Trim(string(*props.ETag), "\"")
	}

	if props.ContentType != nil {
		info.ContentType = *props.ContentType
	}

	if props.LastModified != nil {
		info.LastValidated = *props.LastModified
	}

	return info, nil
}

// GetRange fetches [off, off+n) of the blob, sending If-Match: <etag>.
func (a *Adapter) GetRange(ctx context.Context, bucket, key, etag string, off, n int64) (io.ReadCloser, error) {
	cName := bucket
	if cName == "" {
		cName = a.cfg.Container
	}

	bc := a.client.ServiceClient().NewContainerClient(cName).NewBlobClient(key)
	opts := &azblob.DownloadStreamOptions{
		Range: blob.HTTPRange{Offset: off, Count: n},
	}

	if etag != "" {
		etagVal := azcore.ETag(etag)
		opts.AccessConditions = &blob.AccessConditions{
			ModifiedAccessConditions: &blob.ModifiedAccessConditions{
				IfMatch: to.Ptr(etagVal),
			},
		}
	}

	resp, err := bc.DownloadStream(ctx, opts)
	if err != nil {
		if isPreconditionFailed(err) {
			return nil, &origin.OriginETagChangedError{
				Bucket: cName, Key: key, Want: etag,
			}
		}

		if isNotFound(err) {
			return nil, origin.ErrNotFound
		}

		if isAuth(err) {
			return nil, origin.ErrAuth
		}

		return nil, fmt.Errorf("azureblob get-range: %w", err)
	}

	return resp.Body, nil
}

// List enumerates blobs in the container matching prefix.
func (a *Adapter) List(ctx context.Context, bucket, prefix, marker string, maxResults int) (origin.ListResult, error) {
	cName := bucket
	if cName == "" {
		cName = a.cfg.Container
	}

	cc := a.client.ServiceClient().NewContainerClient(cName)
	max := int32(maxResults)
	pager := cc.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{
		Prefix:     &prefix,
		MaxResults: &max,
		Marker:     stringOrNil(marker),
	})
	out := origin.ListResult{}

	if pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			if isAuth(err) {
				return origin.ListResult{}, origin.ErrAuth
			}

			return origin.ListResult{}, fmt.Errorf("azureblob list: %w", err)
		}

		for _, item := range page.Segment.BlobItems {
			entry := origin.ObjectEntry{}
			if item.Name != nil {
				entry.Key = *item.Name
			}

			if item.Properties != nil {
				if item.Properties.ContentLength != nil {
					entry.Size = *item.Properties.ContentLength
				}

				if item.Properties.ETag != nil {
					entry.ETag = strings.Trim(string(*item.Properties.ETag), "\"")
				}

				if item.Properties.BlobType != nil {
					entry.BlobType = string(*item.Properties.BlobType)
				}
			}

			out.Entries = append(out.Entries, entry)
		}

		if page.NextMarker != nil {
			out.NextMarker = *page.NextMarker
			out.IsTruncated = *page.NextMarker != ""
		}
	}

	return out, nil
}

func stringOrNil(s string) *string {
	if s == "" {
		return nil
	}

	return &s
}

func isNotFound(err error) bool {
	return bloberror.HasCode(err, bloberror.BlobNotFound) ||
		bloberror.HasCode(err, bloberror.ContainerNotFound) ||
		errors.Is(err, origin.ErrNotFound)
}

func isAuth(err error) bool {
	var rerr *azcore.ResponseError
	if errors.As(err, &rerr) {
		if rerr.StatusCode == http.StatusUnauthorized || rerr.StatusCode == http.StatusForbidden {
			return true
		}
	}

	return bloberror.HasCode(err, bloberror.AuthenticationFailed) ||
		bloberror.HasCode(err, bloberror.AuthorizationFailure)
}

func isPreconditionFailed(err error) bool {
	var rerr *azcore.ResponseError
	if errors.As(err, &rerr) && rerr.StatusCode == http.StatusPreconditionFailed {
		return true
	}

	return bloberror.HasCode(err, bloberror.ConditionNotMet)
}

// validateBlobType returns an UnsupportedBlobTypeError when
// enforceBlockBlobOnly is set and the blob is a non-Block-Blob type
// (Page or Append). Returns nil for Block Blobs and when the gate is
// disabled. Extracted as a pure function so unit tests can cover all
// branches without an Azurite round-trip.
func validateBlobType(enforceBlockBlobOnly bool, container, key string, blobType *blob.BlobType) error {
	if !enforceBlockBlobOnly || blobType == nil {
		return nil
	}

	if *blobType == blob.BlobTypeBlockBlob {
		return nil
	}

	return &origin.UnsupportedBlobTypeError{
		Bucket:   container,
		Key:      key,
		BlobType: string(*blobType),
	}
}
