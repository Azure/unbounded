// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
	sdk "github.com/Azure/unbounded/pkg/racer"
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

func originContext(ctx context.Context) context.Context {
	return registryauth.WithAuthorization(ctx, sdk.AuthorizationFromContext(ctx))
}

// Stat prefers local metadata, using registry HEAD when local media type is
// unknown. It never guesses a manifest/index media type or reads payload.
func (o *Origin) Stat(ctx context.Context, target string) (sdk.Metadata, error) {
	if auth := sdk.AuthorizationFromContext(ctx); auth != "" && registryauth.Normalize(auth) == "" {
		return sdk.Metadata{}, &sdk.HTTPError{StatusCode: http.StatusUnauthorized}
	}

	ref, err := o.reference(target)
	if err != nil {
		return sdk.Metadata{}, err
	}

	size, contentType := int64(-1), ""

	if o.Local != nil {
		desc, localErr := o.Local.Descriptor(ctx, ref.Digest)
		if localErr == nil && desc.MediaType != "" {
			size, contentType = desc.Size, desc.MediaType
		} else if localErr != nil {
			var missing *ifaces.ErrNotFound
			if !errors.As(localErr, &missing) {
				return sdk.Metadata{}, localErr
			}
		}
	}

	if size < 0 {
		meta, headErr := o.Registry.HeadMetadata(originContext(ctx), ref)
		if headErr != nil {
			return sdk.Metadata{}, originError(headErr)
		}

		size, contentType = meta.Size, meta.ContentType
	}

	ttl := MetadataTTL

	return sdk.Metadata{Size: size, ETag: `"` + ref.Digest.Hex() + `"`, ContentType: contentType, TTL: &ttl}, nil
}

func (o *Origin) OpenRange(ctx context.Context, target, etag string, offset, length int64) (io.ReadCloser, error) {
	if auth := sdk.AuthorizationFromContext(ctx); auth != "" && registryauth.Normalize(auth) == "" {
		return nil, &sdk.HTTPError{StatusCode: http.StatusUnauthorized}
	}

	ref, err := o.reference(target)
	if err != nil {
		return nil, err
	}

	if etag != `"`+ref.Digest.Hex()+`"` {
		return nil, sdk.ErrVersionChanged
	}

	if o.Local != nil {
		body, size, localErr := o.Local.Open(ctx, ref.Digest)
		if localErr == nil {
			if offset < 0 || length < 0 || offset > size || length > size-offset {
				_ = body.Close() //nolint:errcheck // Reject invalid bounds and release snapshot.
				return nil, sdk.ErrVersionChanged
			}

			seeker, ok := body.(io.Seeker)
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

	ctx = originContext(ctx)

	meta, err := o.Registry.HeadMetadata(ctx, ref)
	if err != nil {
		return nil, originError(err)
	}

	if length == 0 && offset == 0 && meta.Size == 0 {
		return io.NopCloser(strings.NewReader("")), nil
	}

	body, err := o.Registry.OpenRange(ctx, meta.Ref, offset, length, meta.Size)

	return body, originError(err)
}

type limitedBody struct {
	io.Reader
	io.Closer
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
