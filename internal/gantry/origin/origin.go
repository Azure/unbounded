// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package origin pulls bytes from upstream OCI registries.
//
// scope:
//
// - Multi-registry: one *Client serves every UpstreamRegistry configured
// by the operator.
// - Endpoints: GET /v2/, GET /v2/<repo>/blobs/<digest>,
// GET /v2/<repo>/manifests/<digest>.
// - Auth: request-scoped delegated Basic/Bearer authorization takes precedence
// and is used without caching or identity fallback. Without configured shared
// credentials, the client discovers the upstream /v2/ challenge for containerd.
// An optional "username:password" file retains the legacy shared-identity flow.
// - Failure classification: maps HTTP status and network errors to
// ifaces.FailureClass for the design doc propagation. Tag-resolution requests are
// not handled here (the mirror returns 503 on tag manifests so
// containerd falls through to origin directly - the design doc / the design doc).
//
// Out of scope for (lands later):
//
// - the design doc negative-cache cooldown integration .
// - Per-pull retries with backoff (caller's responsibility for now).
package origin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	dockerauth "github.com/containerd/containerd/v2/core/remotes/docker/auth"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/oci"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
	"github.com/Azure/unbounded/internal/gantry/tracing"
)

// Client is the concrete OriginPuller. It fans out to one *registry per
// configured upstream and routes calls by OriginRef.Registry.
type Client struct {
	logger                   *slog.Logger
	registries               map[string]*registry // keyed by both Name and NSAlias
	metrics                  metricsHooks
	delegatedCredentialsOnly bool
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
	onBytesRead   func(kind string, bytes int64)
	onRequest     func(method string, statusCode int)
}

// Option configures a Client.
type Option func(*Client)

// WithDelegatedCredentialsOnly disables configured shared credentials, including
// endpoint user information. Anonymous Bearer exchange remains available.
// Request-scoped credentials are never replaced with a shared identity.
func WithDelegatedCredentialsOnly() Option {
	return func(c *Client) { c.delegatedCredentialsOnly = true }
}

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
// start fires once at the top of every Pull or PullRange invocation, before the
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

// WithByteMetrics registers a callback for bytes actually read from upstream
// response bodies. It includes partial failed transfers and retries, and does
// not count HEAD requests. The callback fires once when the body reaches a
// terminal read result or is closed. Hooks must support concurrent calls.
func WithByteMetrics(onBytesRead func(kind string, bytes int64)) Option {
	return func(c *Client) {
		c.metrics.onBytesRead = onBytesRead
	}
}

// WithRequestMetrics observes each HTTP transport round trip, including HEAD,
// GET, redirects, manifest fallbacks, and authentication exchanges. statusCode
// is zero for transport failures. It does not expose URLs or credentials.
// Transport-internal retries are part of the same round trip. Hooks must be
// concurrency-safe. This is independent of the logical pull hooks in WithMetrics.
func WithRequestMetrics(onRequest func(method string, statusCode int)) Option {
	return func(c *Client) { c.metrics.onRequest = onRequest }
}

type meteredTransport struct {
	http.RoundTripper
	onRequest func(string, int)
}

func (t meteredTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.RoundTripper.RoundTrip(req)

	status := 0
	if resp != nil {
		status = resp.StatusCode
	}

	t.onRequest(req.Method, status)

	return resp, err
}

// New builds a Client from the operator config. Unless shared credentials are
// disabled, it returns an error if an upstream credentials file cannot be read.
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
		if c.delegatedCredentialsOnly {
			ur.CredentialsPath = ""
		}

		// Build the raw transport first and layer request metering onto it
		// BEFORE tracing is wrapped on, so the metered counter sits inside the
		// trace span and reports the same request the span describes. Wrapping
		// in the other order would leave the metered transport unreachable
		// behind an opaque RoundTripper.
		transport := newRegistryTransport()
		if c.metrics.onRequest != nil {
			transport = meteredTransport{RoundTripper: transport, onRequest: c.metrics.onRequest}
		}

		r, err := newRegistry(ur, transport, c.logger)
		if err != nil {
			return nil, fmt.Errorf("origin: registry %q: %w", ur.Name, err)
		}

		if c.delegatedCredentialsOnly {
			r.base.User = nil
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
	body, size, _, err := c.pull(ctx, ref, 0)
	return body, size, err
}

// PullRange implements ifaces.OriginRangePuller without a metadata round trip.
func (c *Client) PullRange(ctx context.Context, ref ifaces.OriginRef, length int64) (io.ReadCloser, int64, string, error) {
	if length <= 0 || ref.Offset < 0 || length-1 > math.MaxInt64-ref.Offset {
		kind := ref.Kind.MetricLabel()
		if c.metrics.onPullStart != nil {
			c.metrics.onPullStart(kind)
		}

		err := &ifaces.OriginError{Ref: ref, Class: ifaces.FailureTransient, Err: errors.New("invalid bounded origin range")}
		c.recordFailure(kind, err)

		return nil, 0, "", err
	}

	return c.pull(ctx, ref, length)
}

func (c *Client) pull(ctx context.Context, ref ifaces.OriginRef, length int64) (io.ReadCloser, int64, string, error) {
	kind := ref.Kind.MetricLabel()
	if c.metrics.onPullStart != nil {
		c.metrics.onPullStart(kind)
	}

	if verr := oci.ValidateRepositoryName(ref.Repository); verr != nil {
		err := &ifaces.OriginError{Ref: ref, Class: ifaces.FailureNotFound, Err: verr}
		c.recordFailure(kind, err)

		return nil, 0, "", err
	}

	r, ok := c.registries[ref.Registry]
	if !ok {
		err := &ifaces.OriginError{
			Ref:   ref,
			Class: ifaces.FailureNotFound,
			Err:   fmt.Errorf("unknown registry %q", ref.Registry),
		}
		c.recordFailure(kind, err)

		return nil, 0, "", err
	}

	var (
		rc          io.ReadCloser
		size        int64
		contentType string
		err         error
	)
	if length == 0 {
		rc, size, err = r.pull(ctx, ref)
	} else {
		rc, size, contentType, err = r.pullRange(ctx, ref, length)
	}

	if err != nil {
		c.recordFailure(kind, err)
		return nil, 0, "", err
	}

	if c.metrics.onBytesRead != nil {
		rc = &countingReadCloser{
			ReadCloser: rc,
			onFinish: func(bytes int64) {
				c.metrics.onBytesRead(kind, bytes)
			},
		}
	}

	return rc, size, contentType, nil
}

type countingReadCloser struct {
	io.ReadCloser
	onFinish func(bytes int64)
	mu       sync.Mutex
	bytes    int64
	active   int
	finished bool
	reported bool
}

func (r *countingReadCloser) Read(p []byte) (int, error) {
	r.mu.Lock()
	r.active++
	r.mu.Unlock()
	n, err := r.ReadCloser.Read(p)
	r.mu.Lock()
	r.bytes += int64(n)
	r.active--
	r.finished = r.finished || err != nil
	r.reportLocked()

	return n, err
}

func (r *countingReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.finish()

	return err
}

func (r *countingReadCloser) finish() {
	r.mu.Lock()
	r.finished = true
	r.reportLocked()
}

func (r *countingReadCloser) reportLocked() {
	if !r.finished || r.active != 0 || r.reported {
		r.mu.Unlock()
		return
	}

	r.reported = true
	n := r.bytes
	r.mu.Unlock()

	if r.onFinish != nil && n > 0 {
		r.onFinish(n)
	}
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

// AuthenticationChallenge returns the configured registry's current
// Basic/Bearer challenge. required is false when the registry's /v2/ endpoint
// permits anonymous access. Results are cached per registry so ordinary mirror
// requests do not add an upstream round trip.
func (c *Client) AuthenticationChallenge(ctx context.Context, registry string) (challenge string, required bool, err error) {
	r, ok := c.registries[registry]
	if !ok {
		return "", false, fmt.Errorf("origin: unknown registry %q", registry)
	}

	return r.authenticationChallenge(ctx)
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
var (
	_ ifaces.OriginPuller      = (*Client)(nil)
	_ ifaces.OriginRangePuller = (*Client)(nil)
)

// ---------------------------------------------------------------------------
// per-registry client
// ---------------------------------------------------------------------------

type registry struct {
	name     string
	base     *url.URL // root of the OCI Distribution v2 API
	username string
	password string
	// transport is the transport hc reaches through its otelhttp wrapper: the
	// raw *http.Transport, or meteredTransport around it when request metrics
	// are wired. Kept alongside hc because otelhttp stores its base in an
	// unexported field, so anything that needs to name or replace the inner
	// transport cannot reach it via hc.Transport.
	transport http.RoundTripper
	hc        *http.Client
	logger    *slog.Logger

	// keeps a single most-recent token per registry. OCI registries
	// usually issue tokens whose scope covers an entire repo's pulls, so one
	// token serves manifest + config + many layer requests. A per-scope map
	// lands when the design doc retries make that worthwhile.
	tokMu sync.Mutex
	token *cachedToken

	challengeMu sync.Mutex
	challenge   *cachedAuthenticationChallenge
}

type cachedToken struct {
	value     string
	expiresAt time.Time
}

type cachedAuthenticationChallenge struct {
	value     string
	required  bool
	expiresAt time.Time
}

const (
	authenticationChallengeTTL  = 30 * time.Minute
	anonymousRegistryTTL        = time.Minute
	originDialTimeout           = 30 * time.Second
	originTLSHandshakeTimeout   = 10 * time.Second
	originResponseHeaderTimeout = 30 * time.Second
	originIdleConnTimeout       = 90 * time.Second
)

// originSpanName names upstream spans after the registry operation they
// perform. The default otelhttp name is the full URL, which floods a trace
// with one uniquely-named span per digest and makes aggregation useless.
func originSpanName(_ string, r *http.Request) string {
	return "gantry.origin " + r.Method
}

// newRegistryTransport builds the per-registry transport.
//
// It returns the concrete *http.Transport unwrapped. Tracing is applied in
// newRegistryHTTPClient, on the way into the http.Client, and NOT onto this
// value. That distinction is load-bearing: in-protocol wiring (the metered
// transport) and transport tuning both operate on the raw type, so a caller that
// reaches for the transport still gets the real one, while every request that
// actually leaves the process still gets traced.
//
// otelhttp injects the W3C traceparent header, which is what lets an
// instrumented upstream registry join the same trace, and it is the hop that
// shows where cold-start time went when the bytes came from outside the
// cluster.
func newRegistryTransport() http.RoundTripper {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: originDialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		MaxConnsPerHost:       128,
		DisableCompression:    true,
		IdleConnTimeout:       originIdleConnTimeout,
		TLSHandshakeTimeout:   originTLSHandshakeTimeout,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: originResponseHeaderTimeout,
	}

	return transport
}

// newRegistryHTTPClient returns the client that performs every upstream request.
//
// The transport is tracingRoundTripper rather than otelhttp.NewTransport, and
// that substitution is deliberate rather than incidental. See
// tracingRoundTripper for the reason, which is a credential leak otelhttp cannot
// be configured around.
func newRegistryHTTPClient(transport http.RoundTripper) *http.Client {
	return &http.Client{
		Transport:     &tracingRoundTripper{base: transport},
		CheckRedirect: checkRedirect,
	}
}

// tracingRoundTripper traces each upstream request while keeping pre-signed
// credentials off the trace.
//
// It replaces otelhttp.NewTransport for this client. otelhttp records url.full
// from the request it is given, strips URL user information and nothing else,
// and then sends that same request. Blob fetches follow registry redirects to
// storage endpoints whose credential is the query string (sv/sig/se for Azure
// Blob Storage, X-Amz-* for S3, X-Goog-* for Google Cloud Storage), so a
// pre-signed signature lands on the redirect span, readable by anyone with
// collector access and replayable until it expires.
//
// No wrapper of otelhttp fixes this, and three placements were tried:
//
//   - Outside otelhttp, rewriting the request: otelhttp builds its attributes
//     before delegating, so a scrubber upstream is invisible to the span.
//   - Inside otelhttp: the request handed to the wrapped transport is the
//     request that reaches the socket, so scrubbing there redacts the wire URL
//     and storage rejects the pull.
//   - After the fact with span.SetAttributes: the wrapper's own span is the
//     parent, so the child carries the clean value while the parent still
//     reports the signature.
//
// otelhttp v0.71.0 exposes no hook to override url.full, and v0.72.0 does not
// add one while forcing otel v1.46.0 to v1.47.0 across the module, which is a
// fleet-wide change out of proportion to this fix. So this type is hand-rolled.
//
// The two URLs are therefore separated on purpose: the span and the outgoing
// traceparent carry the redacted URL, and the socket carries the original. The
// request the caller passed in is never modified.
//
// The tradeoff is real and worth stating: http.Client builds each automatic
// redirect hop without a parent context, so hops the client follows by itself
// are separate root spans rather than children of the first hop. Requests Gantry
// issues itself, including the auth retry and range requests, still share one
// trace. Chained redirect spans would need the caller to follow redirects
// manually, which changes retry, auth, and cancellation behavior across every
// blob fetch to spare one hop of trace nesting.
//
// Metrics otelhttp would have emitted are not emitted here. Gantry's upstream
// observability comes from its own counters, so nothing currently reads those.
type tracingRoundTripper struct {
	base http.RoundTripper
}

func (rt *tracingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, span := tracing.Tracer().Start(
		req.Context(),
		originSpanName("", req),
		trace.WithSpanKind(trace.SpanKindClient),
	)

	// The span is ended explicitly on every return path rather than deferred,
	// because for a successful GET the interesting work happens after this
	// function returns. Ending it here would record a streamed multi-gigabyte
	// layer as the time to first byte, which is precisely the latency a
	// "why was this pull slow" trace is read to answer. Ownership transfers to
	// the response body wrapper on that path; see endSpanOnClose.
	ended := false

	end := func() {
		if !ended {
			ended = true

			span.End()
		}
	}

	// The span and the propagation header both describe the redacted URL. They
	// are built from one clone so what a trace shows and what a downstream peer
	// is told cannot diverge.
	recorded := req.Clone(ctx)

	redacted := false

	if recorded.URL != nil {
		// URL user information is a second credential channel, independent of the
		// query string, and clearing the query says nothing about it. Supported
		// legacy endpoints may carry Basic credentials in the registry endpoint,
		// and urlWithPath copies r.base including its User, so every derived
		// request URL holds the configured username and password.
		//
		// Only the recorded copy is cleared. The wire request keeps its User
		// because that is how those endpoints authenticate, so this cannot change
		// whether a pull is authorized.
		if recorded.URL.User != nil {
			recorded.URL.User = nil

			redacted = true
		}

		if recorded.URL.RawQuery != "" {
			redacted = redactQuery(recorded.URL) || redacted
		}
	}

	span.SetAttributes(
		attribute.String(string(semconv.HTTPRequestMethodKey), req.Method),
		attribute.String(tracing.AttrURLFull, recorded.URL.String()),
		attribute.Bool(tracing.AttrURLRedacted, redacted),
	)

	if target := registryHost(req); target != "" {
		span.SetAttributes(attribute.String(tracing.AttrRegistry, target))
	}

	// The wire request is a separate clone carrying the original URL. Header is
	// cloned rather than shared so injecting traceparent here cannot write into
	// the caller's header map.
	wire := req.Clone(ctx)
	wire.Header = req.Header.Clone()

	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(wire.Header))

	resp, err := rt.base.RoundTrip(wire)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		end()

		return nil, err
	}

	span.SetAttributes(attribute.Int(string(semconv.HTTPResponseStatusCodeKey), resp.StatusCode))

	// Transport errors and HTTP status are different failures, and only the
	// first produces an error here. A 401, 429, or 500 comes back as a
	// successful round trip, so without this the span stays Unset and the failed
	// upstream hop is dropped from every error-filtered trace view. That is the
	// common case for this client: the auth handshake expects a 401 and callers
	// reject non-200 responses, so the spans most worth reading are the ones this
	// would otherwise hide.
	//
	// 3xx is not marked. Redirects are routine, handled by http.Client, and are
	// not failures the caller acts on.
	if resp.StatusCode >= http.StatusBadRequest {
		span.SetStatus(codes.Error, http.StatusText(resp.StatusCode))
	}

	if resp.Body == nil || resp.Body == http.NoBody {
		end()

		return resp, nil
	}

	// Ownership of the span moves to the body. It ends when the body is fully
	// read or closed, whichever happens first, so the recorded duration covers
	// the download rather than the round-trip headers.
	resp.Body = &endSpanOnClose{ReadCloser: resp.Body, span: span, end: end}

	return resp, nil
}

// endSpanOnClose ends the owning span when the response body is done.
//
// "Done" is EOF or Close, whichever comes first. Both are needed. Callers that
// read to completion and then close end the span at EOF, which is where the
// transfer actually finished; callers that abandon a body early, which the
// mirror does when a peer wins the race, end it at Close. Watching only Close
// would let a body left dangling by a garbage collector decide when the span
// ended, putting a duration no request experienced on the trace.
type endSpanOnClose struct {
	io.ReadCloser

	span trace.Span
	end  func()
	once sync.Once
}

func (b *endSpanOnClose) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == nil {
		return n, nil
	}

	// io.EOF is the normal end of the body. Any other error is a real read
	// failure and belongs on the span: a truncated download is exactly what a
	// trace gets consulted for, and it never surfaces as a transport error from
	// RoundTrip because the headers had already arrived by then.
	if !errors.Is(err, io.EOF) {
		b.span.SetStatus(codes.Error, err.Error())
	}

	b.finish()

	return n, err
}

func (b *endSpanOnClose) Close() error {
	err := b.ReadCloser.Close()
	b.finish()

	return err
}

// finish ends the span at most once. Close can be called repeatedly, and
// http.Client closes the body itself on some paths in addition to the caller.
func (b *endSpanOnClose) finish() {
	b.once.Do(b.end)
}

// registryHost returns the host the request is actually going to, preferring an
// explicit Host header the same way net/http does.
func registryHost(req *http.Request) string {
	if req.Host != "" {
		return req.Host
	}

	if req.URL != nil {
		return req.URL.Host
	}

	return ""
}

// sensitiveQueryFragments are the query parameter name substrings that mark a
// value as a credential. Matching is substring and case-insensitive because the
// signed-parameter vocabulary differs per storage provider and several are
// opaque short names (sv, sig, se, rsc) that an exact-match list would miss.
//
// Substring matching is deliberately narrow and multi-character. A fragment like
// "st" or "se" matches a large family of ordinary names: last, first, range,
// name, test. Redacting a tags-list "last" cursor or a "range" makes a trace
// actively misleading about a request that carried no credential at all, so
// those short names are matched exactly instead, in sensitiveQueryExact.
//
// Within that limit, over-redaction is the preferred direction: a swallowed
// diagnostic parameter costs trace detail, while a missed credential publishes a
// usable secret. The upstream request is untouched either way, so
// this can only ever reduce what the trace shows, never what the registry sees.
var sensitiveQueryFragments = []string{
	"sig",        // sig, signature, X-Amz-Signature, X-Goog-Signature
	"credential", // X-Amz-Credential
	"token",
	"secret",
	"password",
	"apikey",
	"api-key",
	"expires", // X-Amz-Expires, X-Goog-Expires
	"goog",    // X-Goog-*, GCS
	"x-amz",   // X-Amz-*, S3
}

// sensitiveQueryExact are opaque signed parameter names too short to match on
// substring. Bare "st", "se", "sv", and "sr" collide with common English words
// as substrings, so they match only when the whole parameter name is one of them.
var sensitiveQueryExact = map[string]struct{}{
	"se":  {}, // Azure SAS signed expiry
	"sv":  {}, // Azure SAS signed version
	"st":  {}, // Azure SAS signed start
	"sr":  {}, // Azure SAS signed resource / GCS signed resource
	"srt": {}, // Azure SAS signed resource type
	"sp":  {}, // Azure SAS signed permissions
	"sk":  {}, // Azure SAS signing key
	"ss":  {}, // Azure SAS signed snapshot
	"spr": {}, // Azure SAS signed protocol
	"ska": {}, // Azure SAS signing algorithm
}

// redactQuery replaces sensitive query values in u with a placeholder and
// reports whether it changed anything. u is expected to be a copy.
func redactQuery(u *url.URL) bool {
	changed := false

	q := u.Query()
	for name := range q {
		if !sensitiveQuery(name) {
			continue
		}

		vals := q[name]
		for i := range vals {
			if vals[i] != redactedQueryValue {
				vals[i] = redactedQueryValue
				changed = true
			}
		}

		q[name] = vals
	}

	if !changed {
		return false
	}

	// Encode re-sorts the parameters, so the recorded URL differs in ordering
	// from the one sent. That is acceptable: url.full is a diagnostic attribute,
	// and the ordering is not something the trace consumer can depend on.
	u.RawQuery = q.Encode()

	return true
}

// redactedQueryValue replaces any query value judged sensitive.
const redactedQueryValue = "REDACTED"

// sensitiveQuery reports whether a query parameter name can carry a credential.
//
// Known limit, accepted deliberately: matching is on the decoded name, so a
// parameter whose name is mangled into something that no longer contains a
// credential fragment slips through, and "si%6Fg" decoding to "sigo" is the
// example. This is not a weakness specific to the substring approach; an
// exact-match list is equally exposed. No name-based matcher closes it. The
// providers also do not emit names like this: ACR, S3, and GCS write fixed
// parameter names, and Gantry does not construct these URLs at all, it follows a
// Location header the provider wrote. Redacting every query value would close it
// at the cost of stripping the cursor from a tags/list and the range from a blob
// fetch, which is the detail a trace is read for. TestRedactQueryEscapedInteriorName
// pins the behavior so a change here is noticed rather than assumed.
//
// No memoization: this runs over the handful of parameters one signed URL
// carries, once per request, and the whole check is a map lookup against
// sensitiveQueryExact plus at most ten substring comparisons. Caching the verdict
// would cost more in synchronization and type assertions than it saves.
func sensitiveQuery(name string) bool {
	lower := strings.ToLower(name)

	if _, ok := sensitiveQueryExact[lower]; ok {
		return true
	}

	for _, frag := range sensitiveQueryFragments {
		if strings.Contains(lower, frag) {
			return true
		}
	}

	return false
}

func newRegistry(ur config.UpstreamRegistry, transport http.RoundTripper, logger *slog.Logger) (*registry, error) {
	u, err := url.Parse(ur.Endpoint)
	if err != nil {
		// Parse errors can include credentials in both the URL and the cause.
		return nil, errors.New("parse endpoint: invalid URL")
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("endpoint: scheme must be http or https")
	}

	if u.Hostname() == "" {
		return nil, errors.New("endpoint: hostname is required")
	}

	r := &registry{
		name:      ur.Name,
		base:      u,
		transport: transport,
		hc:        newRegistryHTTPClient(transport),
		logger:    logger.With(slog.String("registry", ur.Name)),
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

func (r *registry) authenticationChallenge(ctx context.Context) (string, bool, error) {
	r.challengeMu.Lock()
	defer r.challengeMu.Unlock()

	if r.challenge != nil && time.Now().Before(r.challenge.expiresAt) {
		return r.challenge.value, r.challenge.required, nil
	}
	// An explicitly configured credential opts into the legacy shared-identity
	// mode. In that mode Gantry authenticates to origin itself and containerd
	// does not need to negotiate a requester credential with the mirror.
	if r.username != "" {
		return "", false, nil
	}
	// Challenge discovery and delegated credentials are HTTPS-only. Reporting
	// no challenge preserves anonymous HTTP registries; private HTTP registries
	// cannot use requester-delegated authentication.
	if !strings.EqualFold(r.base.Scheme, "https") {
		return "", false, nil
	}

	u := r.urlWithPath("/v2/")
	u.User = nil

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", false, err
	}

	resp, err := r.hc.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("probe registry authentication: %w", err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close

	switch resp.StatusCode {
	case http.StatusOK:
		r.challenge = &cachedAuthenticationChallenge{
			expiresAt: time.Now().Add(anonymousRegistryTTL),
		}

		return "", false, nil
	case http.StatusUnauthorized:
		challenge, err := validatedAuthenticationChallenge(resp)
		if err != nil {
			return "", false, err
		}

		r.challenge = &cachedAuthenticationChallenge{
			value:     challenge,
			required:  true,
			expiresAt: time.Now().Add(authenticationChallengeTTL),
		}

		return challenge, true, nil
	default:
		return "", false, fmt.Errorf("registry authentication probe returned %s", resp.Status)
	}
}

func (r *registry) rememberAuthenticationChallenge(challenge string) {
	if challenge == "" {
		return
	}

	r.challengeMu.Lock()
	r.challenge = &cachedAuthenticationChallenge{
		value:     challenge,
		required:  true,
		expiresAt: time.Now().Add(authenticationChallengeTTL),
	}
	r.challengeMu.Unlock()
}

func (r *registry) pull(ctx context.Context, ref ifaces.OriginRef) (io.ReadCloser, int64, error) {
	if ref.Offset < 0 {
		return nil, 0, &ifaces.OriginError{Ref: ref, Class: ifaces.FailureTransient, Err: fmt.Errorf("origin offset %d is negative", ref.Offset)}
	}

	path := r.urlFor(ref)

	resp, err := r.do(ctx, http.MethodGet, path, ref.Offset)
	if err != nil {
		return nil, 0, &ifaces.OriginError{Ref: ref, Class: classOf(err), StatusCode: statusOf(err), Err: err}
	}

	if resp.StatusCode == http.StatusNotFound && ref.Kind != ifaces.KindManifest && ref.Offset == 0 {
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

		mResp, mErr := r.do(ctx, http.MethodGet, mPath, 0)
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

				return nil, 0, r.classify(mRef, mResp)
			}

			_ = mResp.Body.Close() //nolint:errcheck // best-effort body close
		} else if mErr != nil {
			// Transport error on the manifest fallback (DNS, RST, ctx
			// deadline). Must not poison the cluster-wide negative cache
			// with a definitive NotFound based on a transient blip.
			return nil, 0, &ifaces.OriginError{Ref: ref, Class: classOf(mErr), StatusCode: statusOf(mErr), Err: mErr}
		}

		// Both blob and manifest returned 404 - classify as not-found
		// without re-issuing the original request.
		return nil, 0, &ifaces.OriginError{Ref: ref, Class: ifaces.FailureNotFound, StatusCode: http.StatusNotFound, Err: fmt.Errorf("origin: %s not found (blob 404, manifest 404)", ref.Digest)}
	}

	if ref.Offset > 0 {
		if resp.StatusCode != http.StatusPartialContent {
			defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

			if resp.StatusCode != http.StatusOK {
				return nil, 0, r.classify(ref, resp)
			}

			return nil, 0, &ifaces.OriginError{Ref: ref, Class: ifaces.FailureTransient, Err: &ifaces.ErrRangeUnsupported{Offset: ref.Offset, Reason: "status " + resp.Status}}
		}

		start, end, size, ok := parseOriginContentRange(resp.Header.Get("Content-Range"))
		if !ok || start != ref.Offset || end != size-1 ||
			(resp.ContentLength >= 0 && resp.ContentLength != end-start+1) {
			defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

			return nil, 0, &ifaces.OriginError{Ref: ref, Class: ifaces.FailureTransient, Err: &ifaces.ErrRangeUnsupported{Offset: ref.Offset, Reason: fmt.Sprintf("invalid Content-Range %q", resp.Header.Get("Content-Range"))}}
		}

		return resp.Body, size, nil
	}

	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close
		return nil, 0, r.classify(ref, resp)
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

	resp, err := r.do(ctx, http.MethodHead, path, 0)
	if err != nil {
		return 0, "", &ifaces.OriginError{Ref: ref, Class: classOf(err), StatusCode: statusOf(err), Err: err}
	}

	if resp.StatusCode == http.StatusNotFound && ref.Kind != ifaces.KindManifest {
		_ = resp.Body.Close() //nolint:errcheck // best-effort body close
		mRef := ref
		mRef.Kind = ifaces.KindManifest

		mResp, mErr := r.do(ctx, http.MethodHead, r.urlFor(mRef), 0)
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

				return 0, "", r.classify(mRef, mResp)
			}

			_ = mResp.Body.Close() //nolint:errcheck // best-effort body close
		} else if mErr != nil {
			// Transport error on the manifest fallback HEAD. Must not
			// classify as NotFound from a transient failure.
			return 0, "", &ifaces.OriginError{Ref: ref, Class: classOf(mErr), StatusCode: statusOf(mErr), Err: mErr}
		}

		return 0, "", &ifaces.OriginError{Ref: ref, Class: ifaces.FailureNotFound, StatusCode: http.StatusNotFound, Err: fmt.Errorf("origin HEAD: %s not found (blob 404, manifest 404)", ref.Digest)}
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != http.StatusOK {
		return 0, "", r.classify(ref, resp)
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
	resource := "blobs"

	switch ref.Kind {
	case ifaces.KindManifest:
		resource = "manifests"
	}

	u := r.urlWithPath("/v2/" + ref.Repository + "/" + resource + "/" + ref.Digest.String())

	return u.String()
}

func (r *registry) urlWithPath(path string) url.URL {
	u := *r.base
	suffix := url.URL{Path: path}
	u.RawPath = strings.TrimRight(u.EscapedPath(), "/") + suffix.EscapedPath()
	u.Path, _ = url.PathUnescape(u.RawPath) //nolint:errcheck // Both parts come from EscapedPath, so unescaping cannot fail.
	u.RawQuery, u.Fragment, u.RawFragment = "", "", ""
	u.ForceQuery = false

	return u
}

// do issues a request, preferring request-scoped delegated Basic/Bearer auth. A
// delegated credential is used exactly as supplied and is never cached; if it
// is rejected, the 401 is returned rather than silently changing identity to
// this node's configured credentials. Without delegated auth, the legacy
// credentials-file bearer-token flow remains available.
func (r *registry) do(ctx context.Context, method, urlStr string, offset int64) (*http.Response, error) {
	rangeValue := ""
	if offset > 0 {
		rangeValue = fmt.Sprintf("bytes=%d-", offset)
	}

	return r.doRange(ctx, method, urlStr, rangeValue)
}

const manifestAccept = "application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.oci.image.index.v1+json, " +
	"application/vnd.docker.distribution.manifest.v2+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json"

func (r *registry) doRange(ctx context.Context, method, urlStr, rangeValue string) (*http.Response, error) {
	delegatedAuthorization := registryauth.Authorization(ctx)
	if delegatedAuthorization != "" && !r.canSendBasicAuth() {
		return nil, &tokenError{
			class: ifaces.FailureAuth,
			err:   errors.New("delegated registry authorization requires an HTTPS origin endpoint"),
		}
	}

	build := func(authorization string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, method, urlStr, nil)
		if err != nil {
			return nil, err
		}

		if strings.Contains(urlStr, "/manifests/") {
			req.Header.Set("Accept", manifestAccept)
		}

		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}

		if rangeValue != "" {
			req.Header.Set("Range", rangeValue)
		}

		return req, nil
	}

	cachedTok := ""

	initialAuthorization := delegatedAuthorization
	if initialAuthorization == "" {
		cachedTok = r.cachedToken()
		if cachedTok != "" {
			initialAuthorization = "Bearer " + cachedTok
		}
	}

	req, err := build(initialAuthorization)
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
	// A delegated token represents the requesting node's authority. Never
	// replace it with this puller node's identity on rejection.
	if delegatedAuthorization != "" {
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
		retry, err := build("")
		if err != nil {
			return nil, err
		}

		if r.canSendBasicAuth() && r.username != "" {
			retry.SetBasicAuth(r.username, r.password)
		}

		return r.hc.Do(retry)
	}

	tok, ttl, err := r.fetchBearerToken(ctx, challenge)
	if err != nil {
		return nil, err
	}

	r.setToken(tok, ttl)

	req2, err := build("Bearer " + tok)
	if err != nil {
		return nil, err
	}

	return r.hc.Do(req2)
}

func parseOriginContentRange(value string) (start, end, size int64, ok bool) {
	if !strings.HasPrefix(value, "bytes ") {
		return 0, 0, 0, false
	}

	rangeAndSize := strings.Split(strings.TrimPrefix(value, "bytes "), "/")
	if len(rangeAndSize) != 2 {
		return 0, 0, 0, false
	}

	bounds := strings.Split(rangeAndSize[0], "-")
	if len(bounds) != 2 {
		return 0, 0, 0, false
	}

	start, err := strconv.ParseInt(bounds[0], 10, 64)
	if err != nil {
		return 0, 0, 0, false
	}

	end, err = strconv.ParseInt(bounds[1], 10, 64)
	if err != nil {
		return 0, 0, 0, false
	}

	size, err = strconv.ParseInt(rangeAndSize[1], 10, 64)
	if err != nil {
		return 0, 0, 0, false
	}

	return start, end, size, start >= 0 && end >= start && size > end
}

// fetchBearerToken parses a Bearer challenge and exchanges it for a token.
// Returns the token and the server-advertised TTL (or 0 if the response
// omitted expires_in, in which case the caller picks a default).
func (r *registry) fetchBearerToken(ctx context.Context, challenge string) (string, time.Duration, error) {
	params := parseChallenge(challenge)

	realm := params["realm"]
	if realm == "" {
		return "", 0, errors.New("bearer challenge missing realm")
	}

	realmURL, err := url.Parse(realm)
	if err != nil {
		return "", 0, errors.New("bearer challenge invalid realm")
	}

	if !realmURL.IsAbs() || realmURL.Host == "" || realmURL.User != nil || !strings.EqualFold(realmURL.Scheme, "https") {
		return "", 0, errors.New("bearer challenge realm: token endpoint must be an absolute https URL without user information")
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
			return "", 0, &tokenError{class: ifaces.FailureAuth, status: resp.StatusCode, err: fmt.Errorf("token endpoint auth failure: %s", resp.Status)}
		case http.StatusTooManyRequests:
			return "", 0, &tokenError{class: ifaces.FailureRateLimited, status: resp.StatusCode, err: fmt.Errorf("token endpoint rate limited: %s", resp.Status)}
		default:
			return "", 0, &tokenError{class: ifaces.FailureTransient, status: resp.StatusCode, err: fmt.Errorf("token endpoint returned %s", resp.Status)}
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

// classify maps an HTTP status to a the design doc FailureClass and preserves
// a validated Basic/Bearer challenge so containerd can refresh rejected
// delegated credentials.
func (r *registry) classify(ref ifaces.OriginRef, resp *http.Response) error {
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

	oe := &ifaces.OriginError{
		Ref:        ref,
		Class:      class,
		StatusCode: resp.StatusCode,
		Err:        fmt.Errorf("upstream returned %s", resp.Status),
	}
	if class == ifaces.FailureAuth {
		if challenge, err := validatedAuthenticationChallenge(resp); err == nil {
			oe.Challenge = challenge
			r.rememberAuthenticationChallenge(challenge)
		}
	}

	return oe
}

func validatedAuthenticationChallenge(resp *http.Response) (string, error) {
	if resp == nil || resp.Request == nil || resp.Request.URL == nil {
		return "", errors.New("authentication challenge has no request URL")
	}

	if !strings.EqualFold(resp.Request.URL.Scheme, "https") {
		return "", errors.New("authentication challenge requires an HTTPS registry endpoint")
	}

	challenges := dockerauth.ParseAuthHeader(resp.Header)
	for _, challenge := range challenges {
		scheme := ""

		switch challenge.Scheme {
		case dockerauth.BearerAuth:
			realm, err := url.Parse(challenge.Parameters["realm"])
			if err != nil || !realm.IsAbs() || realm.Host == "" || realm.User != nil || !strings.EqualFold(realm.Scheme, "https") {
				continue
			}

			scheme = "Bearer"
		case dockerauth.BasicAuth:
			scheme = "Basic"
		default:
			continue
		}

		keys := make([]string, 0, len(challenge.Parameters))
		for key := range challenge.Parameters {
			keys = append(keys, key)
		}

		sort.Strings(keys)

		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			value := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(challenge.Parameters[key])
			parts = append(parts, fmt.Sprintf(`%s="%s"`, key, value))
		}

		if len(parts) == 0 {
			return scheme, nil
		}

		return scheme + " " + strings.Join(parts, ","), nil
	}

	return "", errors.New("registry returned no supported Basic/Bearer authentication challenge")
}

// tokenError carries a FailureClass through the token-exchange path so that
// pull/head can wrap it in an *OriginError without losing the actual cause
// (auth, rate-limit) under a blanket FailureTransient.
type tokenError struct {
	class  ifaces.FailureClass
	status int
	err    error
}

func statusOf(err error) int {
	var te *tokenError
	if errors.As(err, &te) {
		return te.status
	}

	return 0
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
