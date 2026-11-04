// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package imagepull

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	cplatforms "github.com/containerd/platforms"
	ispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/ociutil"
)

const (
	// DefaultConcurrency is the default maximum number of parallel Range
	// requests per blob.
	DefaultConcurrency = 4
	// DefaultChunkSize is the default number of bytes requested per Range
	// request.
	DefaultChunkSize int64 = 8 << 20

	// maxManifestBytes bounds manifest and index documents.
	maxManifestBytes int64 = 4 << 20
)

var (
	// ErrNotFound reports that a file does not exist in an image.
	ErrNotFound = errors.New("file not found in image")
	// ErrImageNotFound reports that the registry does not have the requested
	// image reference.
	ErrImageNotFound = errors.New("image not found")
	// ErrPlatformNotFound reports that an image index has no manifest for the
	// requested platform.
	ErrPlatformNotFound = errors.New("platform not found in image index")
)

// Docker V2 manifest media types. These mirror the constants in
// internal/ociutil.
const (
	mediaTypeDockerManifest     = ociutil.DockerMediaTypeManifest
	mediaTypeDockerManifestList = ociutil.DockerMediaTypeManifestList
)

// Source identifies an image to pull.
type Source struct {
	// Ref is a fully qualified image reference, such as
	// ghcr.io/org/img:tag or ghcr.io/org/img@sha256:....
	Ref string
	// Architecture selects the platform from a multi-platform index. It may
	// be an architecture ("arm64") or an OS/architecture pair
	// ("linux/arm64"). Empty means v1alpha3.DefaultPXEArchitecture.
	Architecture string
	// Credential is an optional registry credential. The zero value
	// (auth.EmptyCredential) pulls anonymously.
	Credential auth.Credential
}

// Puller resolves images and reads files from them. The zero value is usable
// and pulls directly from the upstream registry.
type Puller struct {
	// Mirror is an optional registry mirror base URL such as
	// http://127.0.0.1:5000 (Gantry). When set, manifests and blobs are
	// requested from the mirror first with ?ns=<upstream registry host>,
	// falling back to the upstream registry on any mirror error.
	Mirror string
	// Concurrency is the maximum number of parallel Range requests per blob.
	// Zero means DefaultConcurrency; one or less disables parallelism.
	Concurrency int
	// ChunkSize is the number of bytes per Range request. Zero or less means
	// DefaultChunkSize.
	ChunkSize int64
	// HTTPClient is the base HTTP client. Nil means retry.DefaultClient for
	// the upstream registry and a non-retrying client for the mirror, so
	// that mirror failures fall back to the upstream registry quickly.
	HTTPClient *http.Client
}

// endpoint is one registry (mirror or upstream) that can serve an image.
type endpoint struct {
	name    string
	repo    *remote.Repository
	client  *auth.Client
	baseURL string
	ref     registry.Reference
}

func (p *Puller) concurrency() int {
	if p.Concurrency == 0 {
		return DefaultConcurrency
	}

	if p.Concurrency < 1 {
		return 1
	}

	return p.Concurrency
}

func (p *Puller) chunkSize() int64 {
	if p.ChunkSize <= 0 {
		return DefaultChunkSize
	}

	return p.ChunkSize
}

// endpoints builds the ordered list of endpoints for ref: the mirror first
// (when configured), then the upstream registry.
func (p *Puller) endpoints(ref registry.Reference, cred auth.Credential) ([]*endpoint, error) {
	upstreamHost := ref.Host()

	var (
		mirrorURL  *url.URL
		mirrorHost string
	)

	if p.Mirror != "" {
		u, err := url.Parse(p.Mirror)
		if err != nil {
			return nil, fmt.Errorf("parse mirror URL %q: %w", p.Mirror, err)
		}

		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") {
			return nil, fmt.Errorf("invalid mirror URL %q: want http(s)://host[:port]", p.Mirror)
		}

		mirrorURL = u
		mirrorHost = u.Host
	}

	credFn := func(_ context.Context, host string) (auth.Credential, error) {
		if host == upstreamHost || (mirrorHost != "" && host == mirrorHost) {
			return cred, nil
		}

		return auth.EmptyCredential, nil
	}

	cache := auth.NewCache()

	var eps []*endpoint

	if mirrorURL != nil {
		base := p.HTTPClient
		if base == nil {
			base = &http.Client{}
		}

		hc := *base
		hc.Transport = &nsTransport{base: base.Transport, host: mirrorHost, ns: ref.Registry}
		client := &auth.Client{Client: &hc, Credential: credFn, Cache: cache}
		mref := registry.Reference{Registry: mirrorHost, Repository: ref.Repository, Reference: ref.Reference}
		repo := &remote.Repository{Reference: mref, Client: client, PlainHTTP: mirrorURL.Scheme == "http"}
		eps = append(eps, &endpoint{
			name:    "mirror " + mirrorHost,
			repo:    repo,
			client:  client,
			baseURL: mirrorURL.Scheme + "://" + mirrorHost,
			ref:     mref,
		})
	}

	base := p.HTTPClient
	if base == nil {
		base = retry.DefaultClient
	}

	client := &auth.Client{Client: base, Credential: credFn, Cache: cache}
	repo := &remote.Repository{Reference: ref, Client: client}
	ociutil.ConfigurePlainHTTP(repo)

	scheme := "https"
	if repo.PlainHTTP {
		scheme = "http"
	}

	eps = append(eps, &endpoint{
		name:    "registry " + upstreamHost,
		repo:    repo,
		client:  client,
		baseURL: scheme + "://" + upstreamHost,
		ref:     ref,
	})

	return eps, nil
}

// nsTransport adds the ns query parameter to requests sent to the mirror.
type nsTransport struct {
	base http.RoundTripper
	host string
	ns   string
}

func (t *nsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}

	if req.URL.Host != t.host {
		return base.RoundTrip(req)
	}

	r := req.Clone(req.Context())
	q := r.URL.Query()
	q.Set("ns", t.ns)
	r.URL.RawQuery = q.Encode()

	return base.RoundTrip(r)
}

// Resolve resolves src to a single-platform manifest and returns an Image
// pinned to that manifest digest. Tags are resolved to digests, and image
// indexes are narrowed to the manifest matching src.Architecture.
func (p *Puller) Resolve(ctx context.Context, src Source) (*Image, error) {
	ref, err := registry.ParseReference(src.Ref)
	if err != nil {
		return nil, fmt.Errorf("parse image reference %q: %w", src.Ref, err)
	}

	if ref.Reference == "" {
		ref.Reference = "latest"
	}

	platform, err := parsePlatform(src.Architecture)
	if err != nil {
		return nil, err
	}

	eps, err := p.endpoints(ref, src.Credential)
	if err != nil {
		return nil, err
	}

	desc, body, err := fetchManifest(ctx, eps, ref.Reference)
	if err != nil {
		return nil, fmt.Errorf("fetch manifest %s: %w", src.Ref, err)
	}

	if isIndex(desc.MediaType) {
		var idx ispec.Index
		if err := json.Unmarshal(body, &idx); err != nil {
			return nil, fmt.Errorf("decode image index %s: %w", src.Ref, err)
		}

		child, err := selectPlatform(platform, idx.Manifests)
		if err != nil {
			return nil, fmt.Errorf("image %s: %w", src.Ref, err)
		}

		desc, body, err = fetchManifest(ctx, eps, child.Digest.String())
		if err != nil {
			return nil, fmt.Errorf("fetch platform manifest %s@%s: %w", src.Ref, child.Digest, err)
		}

		if desc.Digest != child.Digest {
			return nil, fmt.Errorf("platform manifest digest mismatch: got %s, want %s", desc.Digest, child.Digest)
		}
	}

	if !isManifest(desc.MediaType) {
		return nil, fmt.Errorf("image %s: unsupported manifest media type %q", src.Ref, desc.MediaType)
	}

	var m ispec.Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("decode manifest %s: %w", src.Ref, err)
	}

	for _, l := range m.Layers {
		if _, err := layerCompression(l.MediaType); err != nil {
			return nil, fmt.Errorf("image %s layer %s: %w", src.Ref, l.Digest, err)
		}

		if err := l.Digest.Validate(); err != nil {
			return nil, fmt.Errorf("image %s: invalid layer digest: %w", src.Ref, err)
		}
	}

	return &Image{
		puller: p,
		src:    src,
		eps:    eps,
		digest: desc.Digest.String(),
		layers: m.Layers,
	}, nil
}

// fetchManifest fetches a manifest by tag or digest from the first endpoint
// that serves it, verifying its content against the returned descriptor.
func fetchManifest(ctx context.Context, eps []*endpoint, reference string) (ispec.Descriptor, []byte, error) {
	var errs []error

	for _, ep := range eps {
		desc, body, err := fetchManifestFrom(ctx, ep, reference)
		if err == nil {
			return desc, body, nil
		}

		if ctx.Err() != nil {
			return ispec.Descriptor{}, nil, ctx.Err()
		}

		errs = append(errs, fmt.Errorf("%s: %w", ep.name, err))
	}

	err := errors.Join(errs...)

	// Report not-found only when the upstream registry (the last
	// endpoint) says so; a mirror miss is not authoritative.
	if last := errs[len(errs)-1]; errors.Is(last, errdef.ErrNotFound) {
		return ispec.Descriptor{}, nil, fmt.Errorf("%w: %w", ErrImageNotFound, err)
	}

	return ispec.Descriptor{}, nil, err
}

func fetchManifestFrom(ctx context.Context, ep *endpoint, reference string) (ispec.Descriptor, []byte, error) {
	desc, rc, err := ep.repo.Manifests().FetchReference(ctx, reference)
	if err != nil {
		return ispec.Descriptor{}, nil, err
	}
	defer rc.Close() //nolint:errcheck // read-only body

	if desc.Size > maxManifestBytes {
		return ispec.Descriptor{}, nil, fmt.Errorf("manifest too large: %d bytes", desc.Size)
	}

	body, err := content.ReadAll(rc, desc)
	if err != nil {
		return ispec.Descriptor{}, nil, err
	}

	return desc, body, nil
}

func isIndex(mediaType string) bool {
	return mediaType == ispec.MediaTypeImageIndex || mediaType == mediaTypeDockerManifestList
}

func isManifest(mediaType string) bool {
	return mediaType == ispec.MediaTypeImageManifest || mediaType == mediaTypeDockerManifest
}

// parsePlatform turns Source.Architecture into a normalized platform.
func parsePlatform(arch string) (ispec.Platform, error) {
	if arch == "" {
		arch = v1alpha3.DefaultPXEArchitecture
	}

	spec := arch
	if !strings.Contains(spec, "/") {
		spec = "linux/" + spec
	}

	p, err := cplatforms.Parse(spec)
	if err != nil {
		return ispec.Platform{}, fmt.Errorf("parse architecture %q: %w", arch, err)
	}

	return cplatforms.Normalize(p), nil
}

// selectPlatform returns the first index entry matching want.
func selectPlatform(want ispec.Platform, manifests []ispec.Descriptor) (ispec.Descriptor, error) {
	matcher := cplatforms.NewMatcher(want)

	var checked []string

	for _, d := range manifests {
		if d.Platform == nil {
			continue
		}

		if matcher.Match(*d.Platform) {
			return d, nil
		}

		checked = append(checked, cplatforms.Format(*d.Platform))
	}

	return ispec.Descriptor{}, fmt.Errorf("%w: want %s, have [%s]", ErrPlatformNotFound,
		cplatforms.Format(want), strings.Join(checked, ", "))
}
