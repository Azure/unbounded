// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package origin pulls bytes from upstream OCI registries.
//
// scope:
//
// - Multi-registry: one *Client serves every UpstreamRegistry configured
// by the operator.
// - Endpoints: GET /v2/, GET /v2/<repo>/blobs/<digest>,
// GET /v2/<repo>/manifests/<digest>.
// - Auth: optional Basic auth (credentials from a hostPath file in
// "username:password" format) plus the OCI Distribution Spec bearer-
// token flow (401 -> realm/service/scope -> token -> retry).
// - Failure classification: maps HTTP status and network errors to
// ifaces.FailureClass for the design doc propagation. Tag-resolution requests are
// not handled here (the mirror returns 503 on tag manifests so
// containerd falls through to origin directly - the design doc / the design doc).
//
// Out of scope for (lands later):
//
// - the design doc negative-cache cooldown integration .
// - Per-pull retries with backoff (caller's responsibility for now).
// - Resumable / ranged pulls (the design doc layer-pull semantics).
package origin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/oci"
)

// Client is the concrete OriginPuller. It fans out to one *registry per
// configured upstream and routes calls by OriginRef.Registry.
type Client struct {
	logger     *slog.Logger
	registries map[string]*registry // keyed by both Name and NSAlias
	metrics    metricsHooks
}

// metricsHooks lets the origin client emit counters without importing the
// metrics package directly. Hooks may be nil.
//
// Origin reports STARTED and FAILURE at the boundary of its own
// responsibility (open the HTTP connection / parse the response /
// classify the error). It deliberately does NOT report SUCCESS:
// "origin pull succeeded" is a higher-level outcome that depends on
// the caller's downstream success (stream fully read, digest
// verified, bytes committed/reopened, and, for background ingest,
// advertised through the advertiser). The mirror's serveDigest and the
// puller pump's runOriginPull each own that definition and emit
// p2p_origin_pull_success_total themselves once their respective
// terminal checks pass. Counting success here,
// on Close, would inflate the counter on HEAD requests (which
// never read the body) and on io.Copy / cache-commit failure paths
// (where the caller deferred Close before returning a failure).
type metricsHooks struct {
	onPullStart   func(kind string)        // before request
	onPullFailure func(kind, class string) // any non-success terminal status
}

// Option configures a Client.
type Option func(*Client)

// WithLogger plumbs a structured logger.
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) {
		if l != nil {
			c.logger = l.With(slog.String("subsystem", "origin"))
		}
	}
}

// WithMetrics registers metric callbacks.
//
// start fires once at the top of every Pull invocation, before the
// registry lookup. failure fires once on any terminal error path
// (unknown registry, transport error, non-2xx response, auth
// failure). Success is NOT emitted here - see the metricsHooks doc
// for why and where it belongs.
func WithMetrics(start func(kind string), failure func(kind, class string)) Option {
	return func(c *Client) {
		c.metrics.onPullStart = start
		c.metrics.onPullFailure = failure
	}
}

// New builds a Client from the operator config. Returns an error if any
// upstream credentials file cannot be read.
func New(cfg *config.Config, opts ...Option) (*Client, error) {
	if cfg == nil || len(cfg.UpstreamRegistries) == 0 {
		return nil, errors.New("origin: at least one upstream registry required")
	}

	c := &Client{
		logger:     slog.Default().With(slog.String("subsystem", "origin")),
		registries: map[string]*registry{},
	}
	for _, opt := range opts {
		opt(c)
	}

	for _, ur := range cfg.UpstreamRegistries {
		r, err := newRegistry(ur, c.logger)
		if err != nil {
			return nil, fmt.Errorf("origin: registry %q: %w", ur.Name, err)
		}

		c.registries[ur.Name] = r
		if ur.NSAlias != "" {
			c.registries[ur.NSAlias] = r
		}
	}

	return c, nil
}

// Pull implements ifaces.OriginPuller.
//
// Returns the raw response-body ReadCloser; the caller is responsible
// for streaming the body and reporting per-operation success via
// their own metric hook (the mirror's serveDigest verifies stream
// completion; the puller pump's runOriginPull does the equivalent after
// commit, reopen, and advertiser mark-present).
// origin reports STARTED unconditionally on entry and FAILURE on its
// own error paths only; it does NOT wrap the response body in a
// success-on-Close counter because Close has no way to know whether
// the higher-level operation actually succeeded (HEAD never reads
// the body; io.Copy + cache.Commit failures both fire Close on a
// deferred path).
func (c *Client) Pull(ctx context.Context, ref ifaces.OriginRef) (io.ReadCloser, int64, error) {
	kind := ref.Kind.MetricLabel()
	if c.metrics.onPullStart != nil {
		c.metrics.onPullStart(kind)
	}

	if verr := oci.ValidateRepositoryName(ref.Repository); verr != nil {
		err := &ifaces.OriginError{Ref: ref, Class: ifaces.FailureNotFound, Err: verr}
		c.recordFailure(kind, err)

		return nil, 0, err
	}

	r, ok := c.registries[ref.Registry]
	if !ok {
		err := &ifaces.OriginError{
			Ref:   ref,
			Class: ifaces.FailureNotFound,
			Err:   fmt.Errorf("unknown registry %q", ref.Registry),
		}
		c.recordFailure(kind, err)

		return nil, 0, err
	}

	rc, size, err := r.pull(ctx, ref)
	if err != nil {
		c.recordFailure(kind, err)
		return nil, 0, err
	}

	return rc, size, nil
}

func (c *Client) recordFailure(kind string, err error) {
	if c.metrics.onPullFailure == nil {
		return
	}

	var oe *ifaces.OriginError

	class := string(ifaces.FailureUnspecified)
	if errors.As(err, &oe) {
		class = string(oe.Class)
	}

	c.metrics.onPullFailure(kind, class)
}

// Head implements ifaces.OriginPuller.
//
// HEAD is a deliberately separate code path from Pull. Two design
// points matter:
//
// 1. HEAD does NOT fire onPullStart. p2p_origin_pull_total
// counts byte-pull attempts; mixing HEAD in inflated the
// counter against an operation that produces no bytes, never
// commits to cache, and therefore can fire neither
// p2p_origin_pull_success_total (because mirror+puller-pump
// bump success after Commit, and HEAD never writes a cache
// entry) nor a downstream-failure counter (HEAD doesn't
// io.Copy a body, so it can't fail at the body-copy boundary).
// Leaving HEAD out keeps the
// started == success + failure + in_flight identity intact
// for the pull arithmetic. This constraint ensures
// exactly this drift.
//
// 2. HEAD also does NOT fire onPullFailure. The pull-failure
// hook double-bumps p2p_origin_pull_failure_total{kind,class}
// + p2p_origin_failure_total{class} (see cmd/gantry/main.go's
// origin.WithMetrics closure); both belong to the pull
// family. A future batch can add a dedicated
// p2p_origin_head_total / _failure_total pair if operators
// need HEAD-specific signal, but for now HEAD failures
// surface to operators via the mirror's HTTP status and
// access log alone.
func (c *Client) Head(ctx context.Context, ref ifaces.OriginRef) (int64, string, error) {
	if err := oci.ValidateRepositoryName(ref.Repository); err != nil {
		return 0, "", &ifaces.OriginError{Ref: ref, Class: ifaces.FailureNotFound, Err: err}
	}

	r, ok := c.registries[ref.Registry]
	if !ok {
		return 0, "", &ifaces.OriginError{
			Ref:   ref,
			Class: ifaces.FailureNotFound,
			Err:   fmt.Errorf("unknown registry %q", ref.Registry),
		}
	}

	return r.head(ctx, ref)
}

// Compile-time check.
var _ ifaces.OriginPuller = (*Client)(nil)

// ---------------------------------------------------------------------------
// per-registry client
// ---------------------------------------------------------------------------

type registry struct {
	name     string
	base     *url.URL // root of the OCI Distribution v2 API
	username string
	password string
	hc       *http.Client
	logger   *slog.Logger

	// keeps a single most-recent token per registry. OCI registries
	// usually issue tokens whose scope covers an entire repo's pulls, so one
	// token serves manifest + config + many layer requests. A per-scope map
	// lands when the design doc retries make that worthwhile.
	tokMu sync.Mutex
	token *cachedToken
}

type cachedToken struct {
	value     string
	expiresAt time.Time
}

func newRegistry(ur config.UpstreamRegistry, logger *slog.Logger) (*registry, error) {
	u, err := url.Parse(ur.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse endpoint: %w", err)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("endpoint %q: scheme must be http or https", ur.Endpoint)
	}

	r := &registry{
		name:   ur.Name,
		base:   u,
		hc:     &http.Client{Timeout: 5 * time.Minute, CheckRedirect: checkRedirect},
		logger: logger.With(slog.String("registry", ur.Name)),
	}
	if ur.CredentialsPath != "" {
		b, err := os.ReadFile(ur.CredentialsPath) //#nosec G304 -- operator-supplied path
		if err != nil {
			return nil, fmt.Errorf("read credentials %q: %w", ur.CredentialsPath, err)
		}

		line := strings.TrimSpace(string(b))

		idx := strings.IndexByte(line, ':')
		if idx <= 0 || idx == len(line)-1 {
			return nil, fmt.Errorf("credentials %q: want \"username:password\"", ur.CredentialsPath)
		}

		r.username = line[:idx]
		r.password = line[idx+1:]
	}

	return r, nil
}

func (r *registry) pull(ctx context.Context, ref ifaces.OriginRef) (io.ReadCloser, int64, error) {
	path := r.urlFor(ref)

	resp, err := r.do(ctx, http.MethodGet, path)
	if err != nil {
		return nil, 0, &ifaces.OriginError{Ref: ref, Class: classOf(err), Err: err}
	}

	if resp.StatusCode == http.StatusNotFound && ref.Kind != ifaces.KindManifest {
		// Containerd treats every digest in a pod spec as a generic
		// "content descriptor" and fetches it via /v2/<repo>/blobs/
		// <digest>. When that digest happens to be an image manifest,
		// registries (ACR, Docker Hub, GHCR all behave this way) only
		// serve the bytes at /v2/<repo>/manifests/<digest>. Without
		// this fallback the 404 from /blobs/ would be classified as
		// FailureNotFound, recorded in the cluster-wide negative cache,
		// and the next cold-start cascade through any peer would hit
		// rule-1 short-circuit (ErrFailureShortCircuit -> 5xx) for
		// every node trying to pull the same image. Symptom on
		// kubelet: ImagePullBackOff for the entire Job. Retry as a
		// manifest GET; on 200 return those bytes, on continued 404
		// surface the original blob 404 so the cluster still gets a
		// truthful negative-cache record.
		_ = resp.Body.Close() //nolint:errcheck // best-effort body close
		mRef := ref
		mRef.Kind = ifaces.KindManifest
		mPath := r.urlFor(mRef)

		mResp, mErr := r.do(ctx, http.MethodGet, mPath)
		if mErr == nil && mResp.StatusCode == http.StatusOK {
			mSize := int64(-1)

			if cl := mResp.Header.Get("Content-Length"); cl != "" {
				if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
					mSize = n
				}
			}

			return mResp.Body, mSize, nil
		}

		if mResp != nil {
			// Only preserve the original blob 404 if the manifest
			// attempt also 404'd. Non-404 manifest failures (auth,
			// rate-limit, outage) should be surfaced directly so the
			// negative cache classifies correctly.
			if mResp.StatusCode != http.StatusNotFound {
				defer func() { _ = mResp.Body.Close() }() //nolint:errcheck // best-effort body close

				return nil, 0, classify(mRef, mResp)
			}

			_ = mResp.Body.Close() //nolint:errcheck // best-effort body close
		} else if mErr != nil {
			// Transport error on the manifest fallback (DNS, RST, ctx
			// deadline). Must not poison the cluster-wide negative cache
			// with a definitive NotFound based on a transient blip.
			return nil, 0, &ifaces.OriginError{Ref: ref, Class: ifaces.FailureTransient, Err: mErr}
		}

		// Both blob and manifest returned 404 - classify as not-found
		// without re-issuing the original request.
		return nil, 0, &ifaces.OriginError{Ref: ref, Class: ifaces.FailureNotFound, Err: fmt.Errorf("origin: %s not found (blob 404, manifest 404)", ref.Digest)}
	}

	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close
		return nil, 0, classify(ref, resp)
	}

	size := int64(-1)

	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			size = n
		}
	}

	return resp.Body, size, nil
}

// head issues an HTTP HEAD against the digest URL and returns the
// content length (or -1 if the registry omitted Content-Length).
// The response body is always drained-and-closed because HEAD
// responses may carry a body in some non-conforming registries and
// leaving it open would leak the underlying connection.
//
// HEAD applies the same /blobs/ -> /manifests/ fallback that pull does:
// containerd does HEAD /blobs/<digest> first when verifying a content
// descriptor in a pod spec; for manifest digests that 404s. Without
// the fallback we'd return FailureNotFound and the kubelet pull would
// fail before the GET phase ever runs.
//
// Returns the upstream Content-Type verbatim alongside size so the
// mirror can serve HEAD responses with the same media type the
// eventual GET will carry.
func (r *registry) head(ctx context.Context, ref ifaces.OriginRef) (int64, string, error) {
	path := r.urlFor(ref)

	resp, err := r.do(ctx, http.MethodHead, path)
	if err != nil {
		return 0, "", &ifaces.OriginError{Ref: ref, Class: classOf(err), Err: err}
	}

	if resp.StatusCode == http.StatusNotFound && ref.Kind != ifaces.KindManifest {
		_ = resp.Body.Close() //nolint:errcheck // best-effort body close
		mRef := ref
		mRef.Kind = ifaces.KindManifest

		mResp, mErr := r.do(ctx, http.MethodHead, r.urlFor(mRef))
		if mErr == nil && mResp.StatusCode == http.StatusOK {
			defer func() { _ = mResp.Body.Close() }() //nolint:errcheck // best-effort body close

			size := int64(-1)

			if cl := mResp.Header.Get("Content-Length"); cl != "" {
				if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
					size = n
				}
			}

			return size, mResp.Header.Get("Content-Type"), nil
		}

		if mResp != nil {
			if mResp.StatusCode != http.StatusNotFound {
				defer func() { _ = mResp.Body.Close() }() //nolint:errcheck // best-effort body close

				return 0, "", classify(mRef, mResp)
			}

			_ = mResp.Body.Close() //nolint:errcheck // best-effort body close
		} else if mErr != nil {
			// Transport error on the manifest fallback HEAD. Must not
			// classify as NotFound from a transient failure.
			return 0, "", &ifaces.OriginError{Ref: ref, Class: ifaces.FailureTransient, Err: mErr}
		}

		return 0, "", &ifaces.OriginError{Ref: ref, Class: ifaces.FailureNotFound, Err: fmt.Errorf("origin HEAD: %s not found (blob 404, manifest 404)", ref.Digest)}
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != http.StatusOK {
		return 0, "", classify(ref, resp)
	}

	size := int64(-1)

	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			size = n
		}
	}

	return size, resp.Header.Get("Content-Type"), nil
}

// urlFor returns the full URL for an OriginRef.
func (r *registry) urlFor(ref ifaces.OriginRef) string {
	switch ref.Kind {
	case ifaces.KindManifest:
		return r.base.String() + "/v2/" + ref.Repository + "/manifests/" + ref.Digest.String()
	default:
		return r.base.String() + "/v2/" + ref.Repository + "/blobs/" + ref.Digest.String()
	}
}

// do issues a request, transparently performing the bearer-token flow on
// 401. The cached token (if any) is sent on the first attempt; on 401 the
// challenge is honored, the token is replaced, and one retry is issued.
func (r *registry) do(ctx context.Context, method, urlStr string) (*http.Response, error) {
	build := func(tok string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, method, urlStr, nil)
		if err != nil {
			return nil, err
		}

		if strings.Contains(urlStr, "/manifests/") {
			req.Header.Set("Accept",
				"application/vnd.oci.image.manifest.v1+json, "+
					"application/vnd.oci.image.index.v1+json, "+
					"application/vnd.docker.distribution.manifest.v2+json, "+
					"application/vnd.docker.distribution.manifest.list.v2+json")
		}

		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}

		return req, nil
	}

	cachedTok := r.cachedToken()

	req, err := build(cachedTok)
	if err != nil {
		return nil, err
	}

	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}

	// Cached token (if any) is stale; clear and negotiate fresh.
	challenge := resp.Header.Get("WWW-Authenticate")
	_ = resp.Body.Close() //nolint:errcheck // best-effort body close

	if cachedTok != "" {
		r.clearToken()
	}

	if !strings.HasPrefix(strings.ToLower(challenge), "bearer ") {
		// No bearer challenge - return 401 verbatim so classify reports auth.
		return r.repeatWithoutToken(ctx, method, urlStr)
	}

	tok, ttl, err := r.fetchBearerToken(ctx, challenge)
	if err != nil {
		return nil, err
	}

	r.setToken(tok, ttl)

	req2, err := build(tok)
	if err != nil {
		return nil, err
	}

	return r.hc.Do(req2)
}

// repeatWithoutToken re-issues a request that received a 401 but no usable
// bearer challenge. Returns the 401 response so the caller can classify it
// as FailureAuth.
func (r *registry) repeatWithoutToken(ctx context.Context, method, urlStr string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, urlStr, nil)
	if err != nil {
		return nil, err
	}

	if r.canSendBasicAuth() && r.username != "" {
		req.SetBasicAuth(r.username, r.password)
	}

	return r.hc.Do(req)
}

// fetchBearerToken parses a Bearer challenge and exchanges it for a token.
// Returns the token and the server-advertised TTL (or 0 if the response
// omitted expires_in, in which case the caller picks a default).
func (r *registry) fetchBearerToken(ctx context.Context, challenge string) (string, time.Duration, error) {
	params := parseChallenge(challenge)

	realm := params["realm"]
	if realm == "" {
		return "", 0, fmt.Errorf("bearer challenge missing realm: %q", challenge)
	}

	realmURL, err := url.Parse(realm)
	if err != nil {
		return "", 0, fmt.Errorf("bearer challenge invalid realm %q: %w", realm, err)
	}

	if !realmURL.IsAbs() || realmURL.Host == "" || !strings.EqualFold(realmURL.Scheme, "https") {
		return "", 0, fmt.Errorf("bearer challenge realm %q: token endpoint must be an absolute https URL", realm)
	}

	scope := params["scope"]

	// Build the token URL from the parsed/validated realm so the query is
	// placed correctly even when the realm carries an existing query or a
	// fragment. Concatenating onto the raw realm string could append the query
	// after a "#fragment", which url.Parse then folds into the fragment and
	// drops service/scope from the request that reaches the wire.
	q := realmURL.Query()
	if svc := params["service"]; svc != "" {
		q.Set("service", svc)
	}

	if scope != "" {
		q.Set("scope", scope)
	}

	realmURL.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realmURL.String(), nil)
	if err != nil {
		return "", 0, err
	}

	if r.canSendBasicAuth() && r.username != "" {
		req.SetBasicAuth(r.username, r.password)
	}

	resp, err := r.hc.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("token request: %w", err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != http.StatusOK {
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return "", 0, &tokenError{class: ifaces.FailureAuth, err: fmt.Errorf("token endpoint auth failure: %s", resp.Status)}
		case http.StatusTooManyRequests:
			return "", 0, &tokenError{class: ifaces.FailureRateLimited, err: fmt.Errorf("token endpoint rate limited: %s", resp.Status)}
		default:
			return "", 0, &tokenError{class: ifaces.FailureTransient, err: fmt.Errorf("token endpoint returned %s", resp.Status)}
		}
	}

	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	// Cap token body to 64 KiB - real tokens are tiny; a misconfigured or
	// hostile token endpoint must not be able to OOM the agent.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&body); err != nil {
		return "", 0, &tokenError{class: ifaces.FailureTransient, err: err}
	}

	tok := body.Token
	if tok == "" {
		tok = body.AccessToken
	}

	if tok == "" {
		return "", 0, errors.New("token endpoint returned no token")
	}

	var ttl time.Duration
	if body.ExpiresIn > 0 {
		ttl = time.Duration(body.ExpiresIn) * time.Second
	}

	return tok, ttl, nil
}

func (r *registry) canSendBasicAuth() bool {
	return r.base != nil && strings.EqualFold(r.base.Scheme, "https")
}

// checkRedirect mirrors net/http's default policy (stop after 10 hops) while
// additionally stripping the Authorization header on any redirect whose target
// is not HTTPS. net/http already drops sensitive headers on cross-host
// redirects, but it preserves them across a same-host HTTPS->HTTP downgrade,
// which would leak Basic or bearer credentials onto a plaintext hop. This is
// defense in depth on top of canSendBasicAuth and the bearer realm HTTPS
// validation.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}

	if !strings.EqualFold(req.URL.Scheme, "https") {
		req.Header.Del("Authorization")
	}

	return nil
}

func (r *registry) cachedToken() string {
	r.tokMu.Lock()
	defer r.tokMu.Unlock()

	if r.token == nil {
		return ""
	}

	if !r.token.expiresAt.IsZero() && time.Now().After(r.token.expiresAt) {
		r.token = nil
		return ""
	}

	return r.token.value
}

func (r *registry) setToken(value string, ttl time.Duration) {
	r.tokMu.Lock()
	defer r.tokMu.Unlock()
	// Honor the server-advertised TTL (Docker token endpoints emit
	// expires_in as seconds; OAuth2 the design doc). Apply a 30s safety margin
	// so requests in flight don't get caught by a token expiring
	// between cachedToken and the registry receiving the bearer.
	// Floor at 60s for tokens with absurdly small TTLs and fall back
	// to the historical 5-minute default when the server omits
	// expires_in entirely (DTR, Harbor in some configurations).
	const (
		safetyMargin = 30 * time.Second
		minTTL       = 60 * time.Second
		defaultTTL   = 5 * time.Minute
	)

	effective := ttl - safetyMargin
	switch {
	case ttl <= 0:
		effective = defaultTTL
	case effective < minTTL:
		effective = minTTL
	}

	r.token = &cachedToken{value: value, expiresAt: time.Now().Add(effective)}
}

func (r *registry) clearToken() {
	r.tokMu.Lock()
	defer r.tokMu.Unlock()

	r.token = nil
}

// classify maps an HTTP status to a the design doc FailureClass.
func classify(ref ifaces.OriginRef, resp *http.Response) error {
	var class ifaces.FailureClass

	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		class = ifaces.FailureAuth
	case http.StatusNotFound:
		class = ifaces.FailureNotFound
	case http.StatusTooManyRequests:
		class = ifaces.FailureRateLimited
	default:
		class = ifaces.FailureTransient
	}

	return &ifaces.OriginError{
		Ref:   ref,
		Class: class,
		Err:   fmt.Errorf("upstream returned %s", resp.Status),
	}
}

// tokenError carries a FailureClass through the token-exchange path so that
// pull/head can wrap it in an *OriginError without losing the actual cause
// (auth, rate-limit) under a blanket FailureTransient.
type tokenError struct {
	class ifaces.FailureClass
	err   error
}

func (e *tokenError) Error() string { return e.err.Error() }
func (e *tokenError) Unwrap() error { return e.err }

// classOf recovers a FailureClass from an error returned by r.do. Defaults to
// FailureTransient for plain transport / context errors.
func classOf(err error) ifaces.FailureClass {
	var te *tokenError
	if errors.As(err, &te) {
		return te.class
	}

	return ifaces.FailureTransient
}

// parseChallenge parses a WWW-Authenticate Bearer challenge into its
// key=value parameters. Quotes are stripped.
func parseChallenge(challenge string) map[string]string {
	out := map[string]string{}

	rest := strings.TrimSpace(challenge)
	if !strings.HasPrefix(strings.ToLower(rest), "bearer ") {
		return out
	}

	rest = strings.TrimSpace(rest[len("Bearer "):])
	for len(rest) > 0 {
		eq := strings.IndexByte(rest, '=')
		if eq < 0 {
			break
		}

		key := strings.TrimSpace(rest[:eq])
		rest = rest[eq+1:]

		var val string

		if strings.HasPrefix(rest, `"`) {
			rest = rest[1:]

			end := strings.IndexByte(rest, '"')
			if end < 0 {
				break
			}

			val = rest[:end]
			rest = rest[end+1:]
		} else {
			end := strings.IndexByte(rest, ',')
			if end < 0 {
				val = rest
				rest = ""
			} else {
				val = rest[:end]
				rest = rest[end:]
			}
		}

		out[strings.ToLower(key)] = val
		rest = strings.TrimLeft(rest, ", \t")
	}

	return out
}
