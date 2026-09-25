// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racer adapts Gantry's immutable OCI references to Racer requests and
// whole-page origin reads. Registry credentials travel only in authorization.
package racer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"net/url"
	"strings"
	"time"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/oci"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
	"github.com/Azure/unbounded/pkg/racersdk"
)

// CacheName is the fixed cache shared by Gantry's Racer client and origin.
const CacheName = "gantry"

const metadataTTL = 24 * time.Hour

// originMetadata deliberately excludes credentials and digest. The Racer key
// supplies the digest, and the configured registry supplies the endpoint.
type originMetadata struct {
	Version    int    `json:"version"`
	Registry   string `json:"registry"`
	Repository string `json:"repository"`
	Kind       string `json:"kind"`
}

// Request maps a whole sha256 object to Racer. Offsets belong to Racer's private
// continuation protocol and are rejected here rather than silently discarded.
func Request(ref ifaces.OriginRef, authorization string) (racersdk.Request, error) {
	if err := validateRef(ref); err != nil {
		return racersdk.Request{}, err
	}

	key, err := racersdk.ParseKey(ref.Digest.Hex())
	if err != nil {
		return racersdk.Request{}, err
	}

	data, err := json.Marshal(originMetadata{
		Version: 1, Registry: ref.Registry, Repository: ref.Repository, Kind: ref.Kind.String(),
	})
	if err != nil {
		return racersdk.Request{}, racersdk.NewOriginError(racersdk.ErrorInternal, err)
	}

	metadata, err := racersdk.ParseAdapterMetadata(string(data))
	if err != nil {
		return racersdk.Request{}, err
	}

	auth, err := parseAuthorization(authorization)
	if err != nil {
		return racersdk.Request{}, err
	}

	fetchContext, err := racersdk.NewFetchContext(metadata, auth)
	if err != nil {
		return racersdk.Request{}, err
	}

	return racersdk.Request{Key: key, Context: fetchContext}, nil
}

func validateRef(ref ifaces.OriginRef) error {
	if ref.Offset != 0 || ref.Digest.IsZero() || ref.Digest.Algorithm() != digest.SHA256 ||
		(ref.Kind != ifaces.KindBlob && ref.Kind != ifaces.KindManifest && ref.Kind != ifaces.KindConfig) ||
		oci.ValidateRepositoryName(ref.Repository) != nil || !validRegistry(ref.Registry) {
		return racersdk.NewOriginError(racersdk.ErrorInvalidArgument, nil)
	}

	return nil
}

func validRegistry(name string) bool {
	u, err := url.Parse("//" + name)

	return err == nil && u.Host == name && u.Hostname() != "" && u.User == nil &&
		u.Path == "" && u.RawQuery == "" && u.Fragment == "" && !u.ForceQuery
}

func parseAuthorization(value string) (racersdk.Authorization, error) {
	if value == "" {
		return racersdk.Authorization{}, nil
	}

	// Validate the original bytes before normalization can remove control bytes.
	if _, err := racersdk.ParseAuthorization(value); err != nil {
		return racersdk.Authorization{}, err
	}

	normalized := registryauth.Normalize(value)
	if normalized == "" {
		return racersdk.Authorization{}, racersdk.NewOriginError(racersdk.ErrorInvalidArgument, nil)
	}

	return racersdk.ParseAuthorization(normalized)
}

// Origin returns a concurrent callback using only configured registry names and
// aliases. The allowlist is copied at construction; upstream owns registry I/O,
// including authentication, redirects, cancellation, and offset validation.
func Origin(cfg *config.Config, upstream ifaces.OriginPuller) racersdk.Origin {
	registries := make(map[string]bool)

	if cfg != nil {
		for _, registry := range cfg.UpstreamRegistries {
			registries[registry.Name] = true
			if registry.NSAlias != "" {
				registries[registry.NSAlias] = true
			}
		}
	}

	return func(ctx context.Context, request racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		ref, err := decodeReference(request.Key(), request.Context().Metadata())
		if err != nil {
			return racersdk.Metadata{}, nil, err
		}

		if !registries[ref.Registry] {
			return racersdk.Metadata{}, nil, racersdk.NewOriginError(racersdk.ErrorInvalidArgument, nil)
		}

		auth, err := parseAuthorization(request.Context().Authorization().ForOrigin())
		if err != nil {
			return racersdk.Metadata{}, nil, classifyError(err)
		}

		ctx = registryauth.WithAuthorization(ctx, auth.ForOrigin())
		pin, _ := request.Pin()
		page, _ := request.Range()

		return open(ctx, upstream, ref, request.Operation(), pin, page)
	}
}

func decodeReference(key racersdk.Key, metadata racersdk.AdapterMetadata) (ifaces.OriginRef, error) {
	var data originMetadata

	decoder := json.NewDecoder(strings.NewReader(metadata.ForOrigin()))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&data); err != nil {
		return ifaces.OriginRef{}, racersdk.NewOriginError(racersdk.ErrorInvalidArgument, nil)
	}

	var extra any
	if decoder.Decode(&extra) != io.EOF || data.Version != 1 {
		return ifaces.OriginRef{}, racersdk.NewOriginError(racersdk.ErrorInvalidArgument, nil)
	}

	d, err := digest.Parse("sha256:" + key.String())
	if err != nil {
		return ifaces.OriginRef{}, racersdk.NewOriginError(racersdk.ErrorInvalidArgument, nil)
	}

	ref := ifaces.OriginRef{
		Registry: data.Registry, Repository: data.Repository,
		Digest: d,
	}
	switch data.Kind {
	case "blob":
		ref.Kind = ifaces.KindBlob
	case "manifest":
		ref.Kind = ifaces.KindManifest
	case "config":
		ref.Kind = ifaces.KindConfig
	default:
		return ifaces.OriginRef{}, racersdk.NewOriginError(racersdk.ErrorInvalidArgument, nil)
	}

	return ref, validateRef(ref)
}

func open(ctx context.Context, upstream ifaces.OriginPuller, ref ifaces.OriginRef, operation racersdk.Operation, pin racersdk.ETag, page racersdk.Range) (racersdk.Metadata, io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return racersdk.Metadata{}, nil, classifyError(err)
	}

	tag, err := racersdk.ParseETag(`"` + ref.Digest.String() + `"`)
	if err != nil {
		return racersdk.Metadata{}, nil, classifyError(err)
	}

	// Digest identity is known before any network I/O, including HEAD.
	if pin != (racersdk.ETag{}) && pin != tag {
		return racersdk.Metadata{}, nil, racersdk.NewOriginError(racersdk.ErrorVersionUnavailable, nil)
	}

	if operation < racersdk.OperationHead || operation > racersdk.OperationPinned ||
		(operation == racersdk.OperationPinned && pin == (racersdk.ETag{})) {
		return racersdk.Metadata{}, nil, racersdk.NewOriginError(racersdk.ErrorInvalidArgument, nil)
	}

	if upstream == nil {
		return racersdk.Metadata{}, nil, racersdk.NewOriginError(racersdk.ErrorInternal, nil)
	}

	size, contentType, err := upstream.Head(ctx, ref)
	if err != nil {
		return racersdk.Metadata{}, nil, classifyError(err)
	}

	if size < 0 {
		return racersdk.Metadata{}, nil, racersdk.NewOriginError(racersdk.ErrorBadGateway, nil)
	}

	metadata := racersdk.Metadata{
		Size: racersdk.ByteLength(size), ETag: tag,
		ExpiresAt: time.Now().Add(metadataTTL).Truncate(time.Millisecond),
	}
	if operation == racersdk.OperationHead || operation == racersdk.OperationBootstrap && size == 0 {
		return metadata, nil, nil
	}

	first, last, err := resolvePage(page, metadata.Size)
	if err != nil {
		return metadata, nil, classifyError(err)
	}

	ref.Offset = int64(first)
	if isManifest(contentType) {
		// HEAD may have fallen back from blobs to manifests. Pull only performs
		// that fallback at offset zero, so select the discovered route explicitly.
		ref.Kind = ifaces.KindManifest
	}

	body, pulledSize, err := upstream.Pull(ctx, ref)
	if err != nil {
		return metadata, body, classifyError(err)
	}

	if pulledSize != size || body == nil {
		return metadata, body, racersdk.NewOriginError(racersdk.ErrorBadGateway, nil)
	}

	return metadata, &pageBody{Reader: io.LimitReader(body, int64(last-first)+1), upstream: body}, nil
}

func resolvePage(page racersdk.Range, size racersdk.ByteLength) (racersdk.ByteOffset, racersdk.ByteOffset, error) {
	first, _ := page.First()
	last, _ := page.Last()

	nominalLast := first + racersdk.ByteOffset(min(uint64(racersdk.PageSize)-1, uint64(math.MaxInt64)-uint64(first)))
	if page.Kind() != racersdk.RangeClosed || uint64(first)%uint64(racersdk.PageSize) != 0 || last > nominalLast {
		return 0, 0, racersdk.NewOriginError(racersdk.ErrorInvalidArgument, nil)
	}

	start, end, err := page.Resolve(size)
	if err != nil {
		return 0, 0, err
	}

	if last != nominalLast && last != racersdk.ByteOffset(size-1) {
		return 0, 0, racersdk.NewOriginError(racersdk.ErrorInvalidArgument, nil)
	}

	return start, end, nil
}

// pageBody limits the open-ended registry response to one Racer page. Close
// delegates directly to the upstream body, without a lock held by a blocked Read.
type pageBody struct {
	io.Reader
	upstream io.ReadCloser
}

func (b *pageBody) Close() error { return b.upstream.Close() }

func isManifest(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}

	switch mediaType {
	case "application/vnd.oci.image.manifest.v1+json", "application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.v1+json", "application/vnd.docker.distribution.manifest.v1+prettyjws",
		"application/vnd.docker.distribution.manifest.v2+json", "application/vnd.docker.distribution.manifest.list.v2+json":
		return true
	default:
		return false
	}
}

func classifyError(err error) error {
	kind := racersdk.ErrorBadGateway

	var (
		originError *ifaces.OriginError
		sdkError    *racersdk.Error
	)

	switch {
	case errors.Is(err, context.Canceled):
		kind = racersdk.ErrorCanceled
	case errors.Is(err, context.DeadlineExceeded):
		kind = racersdk.ErrorDeadline
	case errors.As(err, &sdkError):
		kind = sdkError.Kind()
	case errors.As(err, &originError):
		switch originError.Class {
		case ifaces.FailureAuth:
			kind = racersdk.ErrorUnauthorized
		case ifaces.FailureNotFound:
			kind = racersdk.ErrorNotFound
		case ifaces.FailureRateLimited, ifaces.FailureTransient:
			kind = racersdk.ErrorUnavailable
		}
	}

	return racersdk.NewOriginError(kind, err)
}
