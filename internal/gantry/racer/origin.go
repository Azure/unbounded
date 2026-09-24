// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"strings"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
	sdk "github.com/Azure/unbounded/pkg/racersdk"
)

// LocalStore supplies metadata without reading payload and seekable local reads.
type LocalStore interface {
	Descriptor(context.Context, digest.Digest) (ocispec.Descriptor, error)
	Open(context.Context, digest.Digest) (io.ReadCloser, int64, error)
}

// Origin uses only local containerd and the ordinary registry client. It never
// calls Racer, Gantry mirror, peers, or content discovery. Metadata is not cached
// across requests, so delegated identities cannot share auth results.
type Origin struct {
	Local      LocalStore
	Registry   ifaces.OriginRangePuller
	Registries map[string]bool
}

const MetadataTTL = time.Minute

func (o *Origin) reference(target string) (ifaces.OriginRef, error) {
	ref, err := ParseTarget(target)
	if err != nil || !o.Registries[ref.Registry] {
		return ref, fs.ErrNotExist
	}

	return ref, nil
}

func originContext(ctx context.Context, originData []byte) context.Context {
	return registryauth.WithAuthorization(ctx, string(originData))
}

// Racer carries arbitrary bytes; only this adapter interprets them as a
// delegated registry Authorization value.
func validOriginData(data []byte) bool {
	if len(data) == 0 {
		return true
	}

	for _, b := range data {
		if b < 32 || b > 126 {
			return false
		}
	}

	auth := string(data)

	return strings.TrimSpace(auth) == auth && registryauth.Normalize(auth) != ""
}

// Stat prefers local metadata, using registry HEAD when local media type is
// unknown. It never guesses a manifest/index media type or reads payload.
func (o *Origin) Stat(ctx context.Context, target string, originData []byte) (sdk.Metadata, error) {
	resolved, err := o.ResolveRange(ctx, target, originData)
	if err != nil {
		return sdk.Metadata{}, err
	}
	defer resolved.Close() //nolint:errcheck // Release metadata-only request state.

	return resolved.Metadata(), nil
}

var _ sdk.ResolvedRangeStore = (*Origin)(nil)

type resolvedRange struct {
	origin     *Origin
	ref        ifaces.OriginRef
	meta       sdk.Metadata
	originData []byte
	remote     bool
}

func (r *resolvedRange) Metadata() sdk.Metadata { return r.meta }

func (r *resolvedRange) Close() error {
	r.originData = nil
	r.origin = nil

	return nil
}

// ResolveRange keeps metadata resolution and delegated credentials within one
// request. No payload is opened until the handler accepts the GET.
func (o *Origin) ResolveRange(ctx context.Context, target string, originData []byte) (sdk.ResolvedRange, error) {
	if !validOriginData(originData) {
		return nil, &sdk.HTTPError{StatusCode: http.StatusUnauthorized}
	}

	ref, err := o.reference(target)
	if err != nil {
		return nil, err
	}

	size, contentType := int64(-1), ""

	if o.Local != nil {
		desc, localErr := o.Local.Descriptor(ctx, ref.Digest)
		if localErr == nil && desc.MediaType != "" {
			size, contentType = desc.Size, objectContentType(ref.Kind, desc.MediaType)
		} else if localErr != nil {
			var missing *ifaces.ErrNotFound
			if !errors.As(localErr, &missing) {
				return nil, localErr
			}
		}
	}

	remote := size < 0
	if remote {
		meta, headErr := o.Registry.HeadMetadata(originContext(ctx, originData), ref)
		if headErr != nil {
			return nil, originError(headErr)
		}

		if meta.Ref.Digest != ref.Digest || meta.Ref.Registry != ref.Registry || meta.Ref.Repository != ref.Repository {
			return nil, sdk.ErrVersionChanged
		}

		ref = meta.Ref
		size, contentType = meta.Size, objectContentType(meta.Ref.Kind, meta.ContentType)
	}

	ttl := MetadataTTL

	return &resolvedRange{
		origin: o, ref: ref, originData: originData, remote: remote,
		meta: sdk.Metadata{Size: size, ETag: `"` + ref.Digest.Hex() + `"`, ContentType: contentType, TTL: &ttl},
	}, nil
}

func (o *Origin) OpenRange(ctx context.Context, target, etag string, offset, length int64, originData []byte) (io.ReadCloser, error) {
	if !validOriginData(originData) {
		return nil, &sdk.HTTPError{StatusCode: http.StatusUnauthorized}
	}

	ref, err := o.reference(target)
	if err != nil {
		return nil, err
	}

	if etag != `"`+ref.Digest.Hex()+`"` {
		return nil, sdk.ErrVersionChanged
	}

	resolved, err := o.ResolveRange(ctx, target, originData)
	if err != nil {
		return nil, err
	}
	defer resolved.Close() //nolint:errcheck // The returned body owns its own resources.

	return resolved.OpenRange(ctx, offset, length)
}

func (r *resolvedRange) OpenRange(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if r.origin == nil {
		return nil, fs.ErrClosed
	}

	if offset < 0 || length < 0 || offset > r.meta.Size || length > r.meta.Size-offset {
		return nil, sdk.ErrVersionChanged
	}

	o, ref := r.origin, r.ref

	if o.Local != nil {
		body, size, localErr := o.Local.Open(ctx, ref.Digest)
		if localErr == nil {
			if size != r.meta.Size {
				_ = body.Close() //nolint:errcheck // Release snapshot with a changed size.
				return nil, sdk.ErrVersionChanged
			}

			seeker, ok := body.(io.Seeker)
			// A local object may have appeared after remote resolution. Require
			// known, matching representation metadata before using it.
			if ok && r.remote {
				desc, err := o.Local.Descriptor(ctx, ref.Digest)

				var missing *ifaces.ErrNotFound
				if err != nil && !errors.As(err, &missing) {
					_ = body.Close() //nolint:errcheck // Release unusable local snapshot.
					return nil, err
				}

				ok = err == nil && desc.MediaType != ""
				if ok && (desc.Size != r.meta.Size || objectContentType(ref.Kind, desc.MediaType) != r.meta.ContentType) {
					_ = body.Close() //nolint:errcheck // Release changed representation.
					return nil, sdk.ErrVersionChanged
				}
			}

			var err error
			if ok {
				_, err = seeker.Seek(offset, io.SeekStart)
				if err == nil {
					return &limitedBody{Reader: io.LimitReader(body, length), Closer: body}, nil
				}
			}

			_ = body.Close() //nolint:errcheck // Release snapshot before upstream fallback.

			if err != nil {
				return nil, err
			}
		} else {
			var missing *ifaces.ErrNotFound
			if !errors.As(localErr, &missing) {
				return nil, localErr
			}
		}
	}

	ctx = originContext(ctx, r.originData)

	if !r.remote {
		meta, err := o.Registry.HeadMetadata(ctx, ref)
		if err != nil {
			return nil, originError(err)
		}

		if meta.Ref.Digest != ref.Digest || meta.Ref.Registry != ref.Registry || meta.Ref.Repository != ref.Repository ||
			meta.Size != r.meta.Size || objectContentType(meta.Ref.Kind, meta.ContentType) != r.meta.ContentType {
			return nil, sdk.ErrVersionChanged
		}

		ref = meta.Ref
	}

	if length == 0 && offset == 0 && r.meta.Size == 0 {
		return io.NopCloser(strings.NewReader("")), nil
	}

	body, err := o.Registry.OpenRange(ctx, ref, offset, length, r.meta.Size)

	return body, originError(err)
}

type limitedBody struct {
	io.Reader
	io.Closer
}

// OCI descriptor layer/config media types describe stored content, not its HTTP
// representation. Both sources use octet-stream for those objects. Supported
// JSON manifest/index types use their lowercase base MIME type so optional
// parameters cannot change Racer's representation across local/registry sources.
// This also applies to manifests discovered through the blob URL.
func objectContentType(kind ifaces.OriginRefKind, contentType string) string {
	mediaType, _, _ := mime.ParseMediaType(contentType) //nolint:errcheck // Unrecognized types use the URL kind below.
	switch mediaType {
	case "application/vnd.oci.image.manifest.v1+json", "application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json", "application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.docker.distribution.manifest.v1+json", "application/vnd.docker.distribution.manifest.v1+prettyjws":
		return mediaType
	}

	if kind == ifaces.KindManifest {
		return contentType
	}

	return "application/octet-stream"
}

func originError(err error) error {
	if err == nil {
		return nil
	}

	var oe *ifaces.OriginError
	if errors.As(err, &oe) && oe.StatusCode >= 400 && oe.StatusCode <= 599 {
		return &sdk.HTTPError{StatusCode: oe.StatusCode, WWWAuthenticate: oe.Challenge, RetryAfter: oe.RetryAfterHeader}
	}

	var (
		unsupported *ifaces.OriginRangeUnsupportedError
		unknown     *ifaces.OriginMetadataUnavailableError
	)

	if errors.As(err, &unsupported) || errors.As(err, &unknown) {
		return &sdk.HTTPError{StatusCode: http.StatusNotImplemented}
	}

	return err
}
