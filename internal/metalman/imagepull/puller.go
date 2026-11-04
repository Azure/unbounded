// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package imagepull

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

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
	// DefaultMaxBufferedBytes is the default Puller-wide limit on bytes
	// held in prefetched Range chunks.
	DefaultMaxBufferedBytes int64 = 256 << 20
	// DefaultMaxIndexedLayers is the default number of layer indexes kept
	// in memory by a Puller.
	DefaultMaxIndexedLayers = 256

	// DefaultMirrorManifestTimeout is the default bound on a mirror
	// manifest (or other non-blob) request until response headers arrive.
	// Manifests are small, and on a cold miss Gantry fetches one from the
	// upstream registry in a single round trip, so a healthy mirror answers
	// well within this; it is short enough to leave most of a typical
	// caller budget for the upstream fallback.
	DefaultMirrorManifestTimeout = 30 * time.Second
	// DefaultMirrorBlobHeaderTimeout is the default bound on a mirror blob
	// request until response headers arrive. It is generous because on a
	// cold miss Gantry may download the whole blob from a peer or the
	// upstream registry into its cache before it sends headers, which for a
	// multi-gigabyte layer can take minutes. Falling back too early would
	// duplicate that download against the upstream registry.
	DefaultMirrorBlobHeaderTimeout = 5 * time.Minute
	// DefaultMirrorIdleTimeout is the default bound on how long a mirror
	// response body may go without delivering any bytes. Once headers are
	// sent the mirror is streaming, so a minute of silence means it is
	// stuck; the read then resumes from the upstream registry at the
	// current offset.
	DefaultMirrorIdleTimeout = time.Minute
	// DefaultMirrorQuarantine is the default time a blob the mirror served
	// corrupted is read from the upstream registry only, across Images
	// resolved by the same Puller.
	DefaultMirrorQuarantine = 10 * time.Minute

	// userAgent identifies requests made by this package.
	userAgent = "unbounded-metalman-imagepull"

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
// and pulls directly from the upstream registry. A Puller must not be copied
// after first use; Images resolved by the same Puller share its layer index
// cache and its prefetch memory budget.
type Puller struct {
	// Mirror is an optional registry mirror base URL such as
	// http://127.0.0.1:5000 (Gantry). When set, manifests addressed by
	// digest and blobs are requested from the mirror first with
	// ?ns=<upstream registry host>, falling back to (or, mid-stream,
	// resuming from) the upstream registry on any mirror error. Tag
	// references are always resolved against the upstream registry.
	//
	// Source.Credential is sent to the mirror host as well as to the
	// upstream registry, because Gantry uses it for delegated
	// authentication against the upstream registry. Use an https mirror, or
	// a mirror on a trusted local link, when pulling with credentials. A
	// warning is logged with slog.Default when credentials are used with a
	// plain http mirror.
	Mirror string
	// Concurrency is the maximum number of parallel Range requests per blob.
	// Zero means DefaultConcurrency; one or less disables parallelism.
	Concurrency int
	// ChunkSize is the number of bytes per Range request. Zero or less means
	// DefaultChunkSize.
	ChunkSize int64
	// MaxBufferedBytes bounds the bytes held in prefetched Range chunks
	// across all readers of this Puller. Zero or less means
	// DefaultMaxBufferedBytes. A reader that cannot get budget streams its
	// next chunk directly instead of prefetching it.
	MaxBufferedBytes int64
	// MaxIndexedLayers is the number of layer indexes (tar metadata, not
	// content) kept in memory, keyed by layer digest. Zero or less means
	// DefaultMaxIndexedLayers.
	MaxIndexedLayers int
	// HTTPClient is the base HTTP client. Nil means a plain client for blob
	// requests (chunk requests are retried by this package), a retrying
	// client for upstream manifest requests, and a non-retrying client for
	// the mirror, so that mirror failures fall back to the upstream
	// registry quickly.
	HTTPClient *http.Client
	// MirrorManifestTimeout bounds each mirror request other than a blob
	// request (manifests and token requests) until response headers
	// arrive. When it expires the request fails and the upstream registry
	// is tried. Zero means DefaultMirrorManifestTimeout; negative disables
	// the bound. It applies even when HTTPClient is set.
	MirrorManifestTimeout time.Duration
	// MirrorBlobHeaderTimeout bounds each mirror blob request until
	// response headers arrive. Zero means DefaultMirrorBlobHeaderTimeout;
	// negative disables the bound.
	//
	// When the caller's context has a deadline, this and the other mirror
	// timeouts are further capped at half of the time remaining when each
	// mirror request starts, so that a stalled mirror always leaves the
	// upstream fallback the other half, even when the caller's budget is
	// shorter than the configured timeout. This cap applies even when a
	// timeout is disabled.
	MirrorBlobHeaderTimeout time.Duration
	// MirrorIdleTimeout bounds how long a mirror response body may stall
	// without delivering bytes. When it expires the read fails and blob
	// reads resume from the upstream registry at the current offset. Zero
	// means DefaultMirrorIdleTimeout; negative disables the bound.
	MirrorIdleTimeout time.Duration
	// MirrorQuarantine is how long a layer blob the mirror served
	// corrupted is read from the upstream registry only, for every Image
	// this Puller resolves. At most 1024 blobs are remembered; the oldest
	// is forgotten first. Zero means DefaultMirrorQuarantine; negative
	// disables the cross-Image memory, so only the Image that detected the
	// corruption avoids the mirror.
	MirrorQuarantine time.Duration

	once     sync.Once
	st       *pullerState
	warnOnce sync.Once
}

// pullerState is shared by all Images resolved by one Puller.
type pullerState struct {
	indexes *indexCache
	budget  *budget
	// quarantine is nil when Puller.MirrorQuarantine is negative.
	quarantine *quarantine
}

func (p *Puller) state() *pullerState {
	p.once.Do(func() {
		maxBytes := p.MaxBufferedBytes
		if maxBytes <= 0 {
			maxBytes = DefaultMaxBufferedBytes
		}

		maxLayers := p.MaxIndexedLayers
		if maxLayers <= 0 {
			maxLayers = DefaultMaxIndexedLayers
		}

		p.st = &pullerState{indexes: newIndexCache(maxLayers), budget: newBudget(maxBytes)}

		if ttl := durationOr(p.MirrorQuarantine, DefaultMirrorQuarantine); ttl > 0 {
			p.st.quarantine = newQuarantine(ttl, maxQuarantined)
		}
	})

	return p.st
}

// endpoint is one registry (mirror or upstream) that can serve an image.
type endpoint struct {
	name    string
	repo    *remote.Repository
	client  *auth.Client // blob requests
	baseURL string
	ref     registry.Reference
	// attempts is the number of tries per blob request.
	attempts int
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

		if u.Scheme == "http" && cred != auth.EmptyCredential {
			p.warnOnce.Do(func() {
				slog.Default().Warn("imagepull: sending registry credentials to a plain http mirror",
					"mirror", mirrorHost, "registry", upstreamHost)
			})
		}
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
		hc.Transport = &nsTransport{
			base: &timeoutTransport{
				base:       base.Transport,
				host:       mirrorHost,
				manifest:   durationOr(p.MirrorManifestTimeout, DefaultMirrorManifestTimeout),
				blobHeader: durationOr(p.MirrorBlobHeaderTimeout, DefaultMirrorBlobHeaderTimeout),
				idle:       durationOr(p.MirrorIdleTimeout, DefaultMirrorIdleTimeout),
			},
			host: mirrorHost,
			ns:   ref.Registry,
		}
		client := &auth.Client{Client: &hc, Credential: credFn, Cache: cache}
		client.SetUserAgent(userAgent)

		mref := registry.Reference{Registry: mirrorHost, Repository: ref.Repository, Reference: ref.Reference}
		repo := &remote.Repository{Reference: mref, Client: client, PlainHTTP: mirrorURL.Scheme == "http"}
		eps = append(eps, &endpoint{
			name:     "mirror " + mirrorHost,
			repo:     repo,
			client:   client,
			baseURL:  mirrorURL.Scheme + "://" + mirrorHost,
			ref:      mref,
			attempts: 1,
		})
	}

	// Blob requests use a plain client: this package retries chunk
	// requests itself, so a retrying transport would stack retries.
	// Manifest requests have no package-level retries and use
	// retry.DefaultClient's policy.
	blobBase, manifestBase := p.HTTPClient, p.HTTPClient
	if blobBase == nil {
		blobBase = &http.Client{}
		manifestBase = retry.DefaultClient
	}

	client := &auth.Client{Client: blobBase, Credential: credFn, Cache: cache}
	client.SetUserAgent(userAgent)

	manifestClient := &auth.Client{Client: manifestBase, Credential: credFn, Cache: cache}
	manifestClient.SetUserAgent(userAgent)

	repo := &remote.Repository{Reference: ref, Client: manifestClient}
	ociutil.ConfigurePlainHTTP(repo)

	scheme := "https"
	if repo.PlainHTTP {
		scheme = "http"
	}

	eps = append(eps, &endpoint{
		name:     "registry " + upstreamHost,
		repo:     repo,
		client:   client,
		baseURL:  scheme + "://" + upstreamHost,
		ref:      ref,
		attempts: chunkAttempts,
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

// durationOr returns def for zero, zero (no bound) for negative values, and
// d otherwise.
func durationOr(d, def time.Duration) time.Duration {
	switch {
	case d == 0:
		return def
	case d < 0:
		return 0
	default:
		return d
	}
}

// errMirrorTimeout reports that a mirror request exceeded a
// timeoutTransport bound. It is distinct from the caller's context errors
// so that callers fall back to the upstream registry.
var errMirrorTimeout = errors.New("mirror request timed out")

// timeoutTransport bounds requests to the mirror host independently of the
// caller's context: each request must receive response headers within a
// per-kind timeout, and the response body must deliver bytes at least once
// per idle timeout. Bounds are applied with a derived context, so they work
// with any base transport.
type timeoutTransport struct {
	base       http.RoundTripper
	host       string
	manifest   time.Duration
	blobHeader time.Duration
	idle       time.Duration
}

func (t *timeoutTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}

	if req.URL.Host != t.host {
		return base.RoundTrip(req)
	}

	header := t.manifest
	if strings.Contains(req.URL.Path, "/blobs/") {
		header = t.blobHeader
	}

	idle := t.idle

	// Reserve part of the caller's remaining budget for the upstream
	// fallback: a mirror attempt may use at most half of it.
	if limit, ok := mirrorBudget(req.Context()); ok {
		header = capDuration(header, limit)
		idle = capDuration(idle, limit)
	}

	ctx, cancel := context.WithCancelCause(req.Context())

	var timer *time.Timer
	if header > 0 {
		timer = time.AfterFunc(header, func() {
			cancel(fmt.Errorf("%w: no response headers within %s", errMirrorTimeout, header))
		})
	}

	resp, err := base.RoundTrip(req.WithContext(ctx))

	if timer != nil && !timer.Stop() {
		// The header timer fired; report it even if a response raced in.
		if err == nil {
			_ = resp.Body.Close() //nolint:errcheck // discarding response
		}

		cancel(nil)

		return nil, context.Cause(ctx)
	}

	if err != nil {
		cancel(nil)

		if cause := context.Cause(ctx); errors.Is(cause, errMirrorTimeout) {
			return nil, cause
		}

		return nil, err
	}

	resp.Body = newIdleBody(resp.Body, ctx, cancel, idle)

	return resp, nil
}

// mirrorBudget returns the longest a single mirror attempt may wait for
// headers or stall mid-body under ctx: half the time left before its
// deadline, so the upstream registry always keeps the other half. ok is
// false when ctx has no deadline.
func mirrorBudget(ctx context.Context) (limit time.Duration, ok bool) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0, false
	}

	return max(time.Until(deadline)/2, time.Millisecond), true
}

// capDuration returns d bounded by limit. A non-positive d (no bound) is
// replaced by limit.
func capDuration(d, limit time.Duration) time.Duration {
	if d <= 0 || d > limit {
		return limit
	}

	return d
}

// idleBody cancels its request when no Read completes within the idle
// timeout, and releases the request context on Close.
type idleBody struct {
	rc     io.ReadCloser
	ctx    context.Context //nolint:containedctx // reports the cancel cause
	cancel context.CancelCauseFunc
	idle   time.Duration
	timer  *time.Timer
}

func newIdleBody(rc io.ReadCloser, ctx context.Context, cancel context.CancelCauseFunc, idle time.Duration) *idleBody { //nolint:revive // ctx follows rc for readability
	b := &idleBody{rc: rc, ctx: ctx, cancel: cancel, idle: idle}
	if idle > 0 {
		b.timer = time.AfterFunc(idle, func() {
			cancel(fmt.Errorf("%w: response body idle for %s", errMirrorTimeout, idle))
		})
		// The timer runs only while a Read is blocked, so a slow consumer
		// does not trip it.
		b.timer.Stop()
	}

	return b
}

func (b *idleBody) Read(p []byte) (int, error) {
	if b.timer != nil {
		b.timer.Reset(b.idle)
	}

	n, err := b.rc.Read(p)

	if b.timer != nil {
		b.timer.Stop()
	}

	if err != nil {
		if cause := context.Cause(b.ctx); errors.Is(cause, errMirrorTimeout) {
			return n, cause
		}
	}

	return n, err
}

func (b *idleBody) Close() error {
	if b.timer != nil {
		b.timer.Stop()
	}

	err := b.rc.Close()
	b.cancel(nil)

	return err
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

	// Gantry (and mirrors in general) only serve content addressed by
	// digest, so tags are resolved against the upstream registry directly.
	tagEps := eps
	if _, derr := ref.Digest(); derr != nil {
		tagEps = eps[len(eps)-1:]
	}

	desc, body, err := fetchManifest(ctx, tagEps, ref.Reference)
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

	// The credential is already captured by the endpoint clients in eps.
	// Drop it from the stored Source so secret material does not live on
	// in Image or in the Files opened from it.
	kept := src
	kept.Credential = auth.EmptyCredential

	return &Image{
		puller: p,
		src:    kept,
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
