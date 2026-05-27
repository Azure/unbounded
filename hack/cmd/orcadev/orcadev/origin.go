// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcadev

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// originInfo is the per-object metadata an orcadev subcommand cares
// about. ETag is unquoted (the SDKs vary in whether they return
// quoted forms; we normalize here).
type originInfo struct {
	Size int64
	ETag string
}

// originObject names a single object in the origin enumerate output.
type originObject struct {
	Name string
	Size int64
	ETag string
}

// originClient is the orcadev-internal origin abstraction. Unlike
// internal/orca/origin.Origin (which is read-only by design - orca
// only reads), this surface supports the write operations a dev tool
// needs: PutObject, DeleteObject, ListObjects. Both awss3 and
// azureblob drivers implement it.
//
// Errors are surfaced verbatim from the underlying SDK; orcadev does
// not classify them into structured sentinel errors because the
// human-readable wrapper text is what a developer triaging a dev
// cluster wants to see.
type originClient interface {
	// Driver returns "awss3" or "azureblob".
	Driver() string

	// Bucket returns the bucket/container the client is bound to.
	Bucket() string

	// EnsureBucket creates the bucket/container if absent. Idempotent
	// (existing buckets are not an error).
	EnsureBucket(ctx context.Context) error

	// Head returns size + etag.
	Head(ctx context.Context, key string) (originInfo, error)

	// Get returns a streaming body for the named object. Caller
	// closes.
	Get(ctx context.Context, key string) (io.ReadCloser, int64, error)

	// Put streams r (sized n) into the origin under key.
	Put(ctx context.Context, key string, r io.Reader, n int64) error

	// List enumerates objects whose name starts with prefix. Returns
	// up to limit entries; pass 0 to read everything.
	List(ctx context.Context, prefix string, limit int) ([]originObject, error)

	// Delete removes a single object. Missing-on-delete is NOT an
	// error (idempotent).
	Delete(ctx context.Context, key string) error
}

// newOriginClient constructs the right driver from the resolved
// global flags. The chosen driver dictates which fields are
// required; missing-required fields surface here so subcommands
// fail fast with a useful message.
//
// Held in a package-level variable so tests can swap in a fake
// origin client without standing up real Azure/S3 backends.
var newOriginClient = newOriginClientImpl

func newOriginClientImpl(ctx context.Context, g *globalFlags) (originClient, error) {
	switch g.originDriver {
	case "awss3":
		return newAWSS3Origin(ctx, g)
	case "azureblob":
		return newAzureblobOrigin(g)
	default:
		return nil, fmt.Errorf("origin driver %q: must be awss3 or azureblob", g.originDriver)
	}
}

// --- awss3 driver ---

type awss3Origin struct {
	cfg    *globalFlags
	client *s3.Client
}

func newAWSS3Origin(ctx context.Context, g *globalFlags) (*awss3Origin, error) {
	if g.originBucket == "" {
		return nil, fmt.Errorf("origin/awss3: --origin-bucket required")
	}

	client, err := buildS3Client(ctx,
		g.originRegion,
		g.originAccessKey, g.originSecretKey,
		g.originEndpoint,
		g.originUsePathStyle,
	)
	if err != nil {
		return nil, fmt.Errorf("origin/awss3: %w", err)
	}

	return &awss3Origin{cfg: g, client: client}, nil
}

func (a *awss3Origin) Driver() string { return "awss3" }
func (a *awss3Origin) Bucket() string { return a.cfg.originBucket }

func (a *awss3Origin) EnsureBucket(ctx context.Context) error {
	_, err := a.client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(a.cfg.originBucket),
	})
	if err == nil {
		return nil
	}

	if _, cErr := a.client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(a.cfg.originBucket),
	}); cErr != nil {
		return fmt.Errorf("origin/awss3: create bucket %q: %w", a.cfg.originBucket, cErr)
	}

	return nil
}

func (a *awss3Origin) Head(ctx context.Context, key string) (originInfo, error) {
	out, err := a.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(a.cfg.originBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return originInfo{}, err
	}

	var info originInfo
	if out.ContentLength != nil {
		info.Size = *out.ContentLength
	}

	if out.ETag != nil {
		info.ETag = unquoteETag(*out.ETag)
	}

	return info, nil
}

func (a *awss3Origin) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	out, err := a.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(a.cfg.originBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, 0, err
	}

	var size int64
	if out.ContentLength != nil {
		size = *out.ContentLength
	}

	return out.Body, size, nil
}

func (a *awss3Origin) Put(ctx context.Context, key string, r io.Reader, n int64) error {
	// PutObject with SigV4 needs a seekable body so the SDK can
	// compute X-Amz-Content-SHA256 over the payload and then re-read
	// the body for the actual upload. Generators (e.g. crypto/rand,
	// math/rand) and os.File-with-seek-disabled are NOT seekable,
	// causing "failed to seek body to start". Buffer the body to a
	// bytes.Buffer up front so the resulting bytes.Reader is
	// seekable. The orcadev tool caps synthetic blob sizes at 1 GiB
	// by default (--force to override) so this in-memory buffer
	// stays bounded.
	//
	// If perf becomes an issue for multi-GiB payloads, swap in
	// feature/s3/transfermanager (multipart upload, streams
	// natively).
	buf := &bytes.Buffer{}
	if n > 0 {
		buf.Grow(int(n))
	}

	if _, err := io.Copy(buf, r); err != nil {
		return fmt.Errorf("origin/awss3: read body for %s: %w", key, err)
	}

	in := &s3.PutObjectInput{
		Bucket: aws.String(a.cfg.originBucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(buf.Bytes()),
	}
	if n > 0 {
		in.ContentLength = aws.Int64(n)
	}

	if _, err := a.client.PutObject(ctx, in); err != nil {
		return fmt.Errorf("origin/awss3: put %s: %w", key, err)
	}

	return nil
}

func (a *awss3Origin) List(ctx context.Context, prefix string, limit int) ([]originObject, error) {
	var out []originObject

	err := walkS3(ctx, a.client, a.cfg.originBucket, prefix, func(obj s3types.Object) bool {
		o := originObject{}
		if obj.Key != nil {
			o.Name = *obj.Key
		}

		if obj.Size != nil {
			o.Size = *obj.Size
		}

		if obj.ETag != nil {
			o.ETag = unquoteETag(*obj.ETag)
		}

		out = append(out, o)

		return limit <= 0 || len(out) < limit
	})
	if err != nil {
		return nil, fmt.Errorf("origin/awss3: list: %w", err)
	}

	return out, nil
}

func (a *awss3Origin) Delete(ctx context.Context, key string) error {
	_, err := a.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(a.cfg.originBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("origin/awss3: delete %s: %w", key, err)
	}

	return nil
}

// --- azureblob driver ---

type azureblobOrigin struct {
	cfg *globalFlags
	cc  *container.Client
}

func newAzureblobOrigin(g *globalFlags) (*azureblobOrigin, error) {
	if g.originAccount == "" {
		return nil, fmt.Errorf("origin/azureblob: --origin-account required")
	}

	if g.originAccountKey == "" {
		return nil, fmt.Errorf("origin/azureblob: --origin-account-key required")
	}

	if g.originBucket == "" {
		return nil, fmt.Errorf("origin/azureblob: --origin-bucket (azure container) required")
	}

	cred, err := azblob.NewSharedKeyCredential(g.originAccount, g.originAccountKey)
	if err != nil {
		return nil, fmt.Errorf("origin/azureblob: shared-key credential: %w", err)
	}

	endpoint := g.originEndpoint
	if endpoint == "" {
		endpoint = fmt.Sprintf("https://%s.blob.core.windows.net/", g.originAccount)
	}

	endpoint = strings.TrimRight(endpoint, "/")

	svc, err := azblob.NewClientWithSharedKeyCredential(endpoint, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("origin/azureblob: client: %w", err)
	}

	cc := svc.ServiceClient().NewContainerClient(g.originBucket)

	return &azureblobOrigin{cfg: g, cc: cc}, nil
}

func (a *azureblobOrigin) Driver() string { return "azureblob" }
func (a *azureblobOrigin) Bucket() string { return a.cfg.originBucket }

func (a *azureblobOrigin) EnsureBucket(ctx context.Context) error {
	_, err := a.cc.Create(ctx, nil)
	if err == nil {
		return nil
	}

	if bloberror.HasCode(err, bloberror.ContainerAlreadyExists) {
		return nil
	}

	return fmt.Errorf("origin/azureblob: create container %q: %w", a.cfg.originBucket, err)
}

func (a *azureblobOrigin) Head(ctx context.Context, key string) (originInfo, error) {
	props, err := a.cc.NewBlobClient(key).GetProperties(ctx, nil)
	if err != nil {
		return originInfo{}, err
	}

	var info originInfo
	if props.ContentLength != nil {
		info.Size = *props.ContentLength
	}

	if props.ETag != nil {
		info.ETag = unquoteETag(string(*props.ETag))
	}

	return info, nil
}

func (a *azureblobOrigin) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	out, err := a.cc.NewBlobClient(key).DownloadStream(ctx, nil)
	if err != nil {
		return nil, 0, err
	}

	var size int64
	if out.ContentLength != nil {
		size = *out.ContentLength
	}

	return out.Body, size, nil
}

func (a *azureblobOrigin) Put(ctx context.Context, key string, r io.Reader, _ int64) error {
	bc := a.cc.NewBlockBlobClient(key)
	if _, err := bc.UploadStream(ctx, r, &blockblob.UploadStreamOptions{}); err != nil {
		return fmt.Errorf("origin/azureblob: put %s: %w", key, err)
	}

	return nil
}

func (a *azureblobOrigin) List(ctx context.Context, prefix string, limit int) ([]originObject, error) {
	opts := &container.ListBlobsFlatOptions{}
	if prefix != "" {
		opts.Prefix = &prefix
	}

	var out []originObject

	pager := a.cc.NewListBlobsFlatPager(opts)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("origin/azureblob: list: %w", err)
		}

		for _, item := range page.Segment.BlobItems {
			obj := originObject{}
			if item.Name != nil {
				obj.Name = *item.Name
			}

			if item.Properties != nil {
				if item.Properties.ContentLength != nil {
					obj.Size = *item.Properties.ContentLength
				}

				if item.Properties.ETag != nil {
					obj.ETag = unquoteETag(string(*item.Properties.ETag))
				}
			}

			out = append(out, obj)
			if limit > 0 && len(out) >= limit {
				return out, nil
			}
		}
	}

	return out, nil
}

func (a *azureblobOrigin) Delete(ctx context.Context, key string) error {
	_, err := a.cc.NewBlobClient(key).Delete(ctx, nil)
	if err != nil && !bloberror.HasCode(err, bloberror.BlobNotFound) {
		return fmt.Errorf("origin/azureblob: delete %s: %w", key, err)
	}

	return nil
}

// Compile-time interface checks.
var (
	_ originClient = (*awss3Origin)(nil)
	_ originClient = (*azureblobOrigin)(nil)
)
