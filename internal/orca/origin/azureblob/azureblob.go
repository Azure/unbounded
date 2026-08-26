// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package azureblob is the Azure Blob Storage adapter for the Origin
// interface. Block Blobs only; PageBlob and AppendBlob are rejected
// at Head() with UnsupportedBlobTypeError.
package azureblob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"

	"github.com/Azure/unbounded/internal/orca/config"
	"github.com/Azure/unbounded/internal/orca/origin"
)

// Adapter implements origin.Origin against Azure Blob Storage.
type Adapter struct {
	cfg    config.Azureblob
	client *azblob.Client
	log    *slog.Logger
}

// New builds an Adapter from config. The log receives debug-level
// emissions for every Head / GetRange call and the error
// mapping decision (not-found / auth / precondition / unsupported
// blob type) on failure paths. Passing nil falls back to
// slog.Default().
func New(cfg config.Azureblob, log *slog.Logger) (*Adapter, error) {
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

	if log == nil {
		log = slog.Default()
	}

	return &Adapter{cfg: cfg, client: client, log: log}, nil
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

	a.log.LogAttrs(ctx, slog.LevelDebug, "azureblob_head_request",
		slog.String("container", cName),
		slog.String("key", key),
	)

	props, err := a.client.ServiceClient().NewContainerClient(cName).
		NewBlobClient(key).GetProperties(ctx, nil)
	if err != nil {
		if isNotFound(err) {
			a.log.LogAttrs(ctx, slog.LevelDebug, "azureblob_head_not_found",
				slog.String("container", cName),
				slog.String("key", key),
			)

			return origin.ObjectInfo{}, origin.ErrNotFound
		}

		if isAuth(err) {
			a.log.LogAttrs(ctx, slog.LevelDebug, "azureblob_head_auth",
				slog.String("container", cName),
				slog.String("key", key),
			)

			return origin.ObjectInfo{}, origin.ErrAuth
		}

		return origin.ObjectInfo{}, fmt.Errorf("azureblob head: %w", err)
	}

	if err := validateBlobType(cName, key, props.BlobType); err != nil {
		a.log.LogAttrs(ctx, slog.LevelDebug, "azureblob_head_unsupported_blob_type",
			slog.String("container", cName),
			slog.String("key", key),
		)

		return origin.ObjectInfo{}, err
	}

	info := origin.ObjectInfo{}
	if props.ContentLength != nil {
		info.Size = *props.ContentLength
	}

	if props.ETag != nil {
		info.ETag = unwrapAzcoreETag(props.ETag)
	}

	if props.ContentType != nil {
		info.ContentType = *props.ContentType
	}

	a.log.LogAttrs(ctx, slog.LevelDebug, "azureblob_head_response",
		slog.String("container", cName),
		slog.String("key", key),
		slog.Int64("size", info.Size),
		slog.String("etag", origin.ETagShort(info.ETag)),
	)

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
		// Azure (like S3) expects the entity-tag value in If-Match
		// to be a quoted-string per RFC 7232. We strip the quotes
		// on Head (a.cfg internal representation is unquoted) so
		// re-wrap here at the point of egress, mirroring the
		// awss3 driver.
		etagVal := azcore.ETag("\"" + etag + "\"")
		opts.AccessConditions = &blob.AccessConditions{
			ModifiedAccessConditions: &blob.ModifiedAccessConditions{
				IfMatch: to.Ptr(etagVal),
			},
		}
	}

	a.log.LogAttrs(ctx, slog.LevelDebug, "azureblob_get_range_request",
		slog.String("container", cName),
		slog.String("key", key),
		slog.String("etag", origin.ETagShort(etag)),
		slog.Int64("off", off),
		slog.Int64("n", n),
	)

	resp, err := bc.DownloadStream(ctx, opts)
	if err != nil {
		if isPreconditionFailed(err) {
			a.log.LogAttrs(ctx, slog.LevelDebug, "azureblob_get_range_etag_changed",
				slog.String("container", cName),
				slog.String("key", key),
				slog.String("want_etag", origin.ETagShort(etag)),
			)

			return nil, &origin.OriginETagChangedError{
				Bucket: cName, Key: key, Want: etag,
			}
		}

		if isNotFound(err) {
			a.log.LogAttrs(ctx, slog.LevelDebug, "azureblob_get_range_not_found",
				slog.String("container", cName),
				slog.String("key", key),
			)

			return nil, origin.ErrNotFound
		}

		if isAuth(err) {
			a.log.LogAttrs(ctx, slog.LevelDebug, "azureblob_get_range_auth",
				slog.String("container", cName),
				slog.String("key", key),
			)

			return nil, origin.ErrAuth
		}

		return nil, fmt.Errorf("azureblob get-range: %w", err)
	}

	a.log.LogAttrs(ctx, slog.LevelDebug, "azureblob_get_range_response",
		slog.String("container", cName),
		slog.String("key", key),
	)

	return resp.Body, nil
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

// validateBlobType returns an UnsupportedBlobTypeError for any
// non-Block-Blob type (Page or Append). PageBlob and AppendBlob's
// random-access-mutation model is incompatible with orca's chunked
// immutable cache contract, so they are unconditionally rejected
// here. Extracted as a pure function so unit tests can cover the
// branches without an Azurite round-trip.
func validateBlobType(container, key string, blobType *blob.BlobType) error {
	if blobType == nil {
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

// unwrapAzcoreETag normalizes an *azcore.ETag from the Azure SDK
// to the unquoted form orca uses internally. The Azure REST API
// returns entity tags as quoted-strings per RFC 7232; the SDK
// preserves the quotes, and orca strips them at the boundary so
// later If-Match egress (which re-wraps via the awss3 / azureblob
// drivers) doesn't double-quote.
func unwrapAzcoreETag(e *azcore.ETag) string {
	if e == nil {
		return ""
	}

	return strings.Trim(string(*e), "\"")
}
