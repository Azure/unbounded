// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racer integrates Gantry's HTTP content path and registry origin with
// Racer. Registry credentials travel only in authorization.
package racer

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/metrics"
	"github.com/Azure/unbounded/internal/gantry/oci"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
	"github.com/Azure/unbounded/pkg/racersdk"
)

// CacheName is the fixed cache shared by Gantry's Racer client and origin.
const CacheName = "gantry"

const (
	socketRoot                     = "/run/racer"
	metadataTTL                    = 24 * time.Hour
	authenticationChallengeTimeout = 2 * time.Second
	racerWriteChunk                = 256 * 1024
	// Socket transfers avoid scratch copying; amortize ReadFrom, deadline and pipe
	// setup over a larger bounded batch without changing fallback write granularity.
	racerSocketChunk          = 256 * 1024
	racerFailureLogBurst      = 10
	defaultHTTPMaxConnections = 512
	defaultWriteTimeout       = 30 * time.Second
)

// Configuration and registry origin adapter.

// ClientConfig maps Gantry settings to the SDK without overriding SDK defaults.
func ClientConfig(c *config.Config, cache string) racersdk.ClientConfig {
	return racersdk.ClientConfig{
		Cache: cache, MaxConnections: c.RacerMaxConnections,
	}
}

// OriginConfig maps the registry callback's concurrency and request limits.
func OriginConfig(c *config.Config, cache string) racersdk.OriginConfig {
	return racersdk.OriginConfig{
		Cache: cache, MaxConcurrentRequests: c.RacerOriginConcurrentRequests,
	}
}

// Request mapping and bounded origin reads.

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
		return racersdk.Request{}, err
	}

	auth, err := parseAuthorization(authorization)
	if err != nil {
		return racersdk.Request{}, err
	}

	return racersdk.Request{Key: key, Metadata: string(data), Authorization: auth}, nil
}

func validateRef(ref ifaces.OriginRef) error {
	if ref.Offset != 0 || ref.Digest.IsZero() || ref.Digest.Algorithm() != digest.SHA256 ||
		(ref.Kind != ifaces.KindBlob && ref.Kind != ifaces.KindManifest && ref.Kind != ifaces.KindConfig) ||
		oci.ValidateRepositoryName(ref.Repository) != nil || !validRegistry(ref.Registry) {
		return racersdk.ErrInvalidRequest
	}

	return nil
}

func validRegistry(name string) bool {
	u, err := url.Parse("//" + name)

	return err == nil && u.Host == name && u.Hostname() != "" && u.User == nil &&
		u.Path == "" && u.RawQuery == "" && u.Fragment == "" && !u.ForceQuery
}

func parseAuthorization(value string) (string, error) {
	if value == "" {
		return "", nil
	}

	// Validate the original bytes before normalization can remove control bytes.
	if len(value) > 8192 {
		return "", racersdk.ErrInvalidRequest
	}

	for i := range len(value) {
		if value[i] < 0x20 || value[i] > 0x7e {
			return "", racersdk.ErrInvalidRequest
		}
	}

	normalized := registryauth.Normalize(value)
	if normalized == "" {
		return "", racersdk.ErrInvalidRequest
	}

	return normalized, nil
}

// upstream requires metadata and bounded reads. The adapter never opens an
// unbounded stream and truncates it locally to simulate a range request.
type upstream interface {
	Head(context.Context, ifaces.OriginRef) (int64, string, error)
	ifaces.OriginRangePuller
}

// Origin returns a concurrent callback using only configured registry names and
// aliases. The allowlist is copied at construction; upstream owns registry I/O,
// including authentication, redirects, cancellation, and offset validation.
func Origin(cfg *config.Config, source upstream) racersdk.Origin {
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
		ref, err := decodeReference(request.Key, request.Metadata)
		if err != nil {
			return racersdk.Metadata{}, nil, err
		}

		if !registries[ref.Registry] {
			return racersdk.Metadata{}, nil, racersdk.ErrInvalidRequest
		}

		auth, err := parseAuthorization(request.Authorization)
		if err != nil {
			return racersdk.Metadata{}, nil, classifyError(err)
		}

		ctx = registryauth.WithAuthorization(ctx, auth)

		return open(ctx, source, ref, request)
	}
}

func decodeReference(key racersdk.Key, metadata string) (ifaces.OriginRef, error) {
	var data originMetadata

	decoder := json.NewDecoder(strings.NewReader(metadata))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&data); err != nil {
		return ifaces.OriginRef{}, racersdk.ErrInvalidRequest
	}

	var extra any
	if decoder.Decode(&extra) != io.EOF || data.Version != 1 {
		return ifaces.OriginRef{}, racersdk.ErrInvalidRequest
	}

	d, err := digest.Parse("sha256:" + key.String())
	if err != nil {
		return ifaces.OriginRef{}, racersdk.ErrInvalidRequest
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
		return ifaces.OriginRef{}, racersdk.ErrInvalidRequest
	}

	return ref, validateRef(ref)
}

func open(ctx context.Context, source upstream, ref ifaces.OriginRef, request racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return racersdk.Metadata{}, nil, classifyError(err)
	}

	tag := `"` + ref.Digest.String() + `"`

	// Digest identity is known before any network I/O, including HEAD.
	if request.ETag != "" && request.ETag != tag {
		return racersdk.Metadata{}, nil, racersdk.ErrVersionMismatch
	}

	if source == nil {
		return racersdk.Metadata{}, nil, errors.New("missing registry origin")
	}

	if !request.Head {
		return openRange(ctx, source, ref, tag, request.Offset, request.Length)
	}

	size, contentType, err := source.Head(ctx, ref)
	if err != nil {
		return racersdk.Metadata{}, nil, classifyError(err)
	}

	if size < 0 {
		return racersdk.Metadata{}, nil, errors.New("negative registry object size")
	}

	metadata := racersdk.Metadata{
		Size: size, ETag: tag,
		ContentType: contentType,
		ExpiresAt:   time.Now().Add(metadataTTL).Truncate(time.Millisecond),
	}

	return metadata, nil, nil
}

func openRange(ctx context.Context, source ifaces.OriginRangePuller, ref ifaces.OriginRef, tag string, offset, length int64) (racersdk.Metadata, io.ReadCloser, error) {
	ref.Offset = offset

	body, size, contentType, err := source.PullRange(ctx, ref, length)
	if err != nil {
		return racersdk.Metadata{}, body, classifyError(err)
	}

	metadata := racersdk.Metadata{
		Size: size, ETag: tag, ContentType: contentType,
		ExpiresAt: time.Now().Add(metadataTTL).Truncate(time.Millisecond),
	}
	if size < 0 {
		return racersdk.Metadata{}, body, errors.New("negative registry object size")
	}

	if offset >= size || length == 0 {
		if body != nil {
			_ = body.Close() //nolint:errcheck // best-effort close of an empty body
		}

		return metadata, nil, nil
	}

	if body == nil {
		return racersdk.Metadata{}, nil, errors.New("missing registry object body")
	}
	// The bounded origin owns framing; the SDK checks exact body length and
	// metadata consistency across pinned pages. Do not hide excess bytes here.
	return metadata, body, nil
}

func classifyError(err error) error {
	var (
		originError *ifaces.OriginError
		kind        error
	)

	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.As(err, &originError):
		switch originError.Class {
		case ifaces.FailureAuth:
			kind = racersdk.ErrUnauthorized
			if originError.StatusCode == 403 {
				kind = racersdk.ErrForbidden
			}
		case ifaces.FailureNotFound:
			kind = racersdk.ErrNotFound
		case ifaces.FailureRateLimited, ifaces.FailureTransient:
			kind = racersdk.ErrUnavailable
		}
	}

	if kind != nil {
		return fmt.Errorf("%w: %w", kind, err)
	}

	return err
}

// Origin socket ownership.

// SocketPath returns the canonical Gantry socket path for an SDK role.
func SocketPath(role string) string {
	return filepath.Join(socketRoot, CacheName, role, "socket")
}

// ServeOrigin prepares Gantry's endpoint and enables recovery of owned stale sockets.
func ServeOrigin(ctx context.Context, config racersdk.OriginConfig, origin racersdk.Origin) error {
	return serveOriginAt(ctx, config, origin, socketRoot, racersdk.ServeOrigin)
}

func serveOriginAt(ctx context.Context, config racersdk.OriginConfig, origin racersdk.Origin, root string, serve func(context.Context, racersdk.OriginConfig, racersdk.Origin) error) error {
	if err := prepareRacerOriginDirectory(root); err != nil {
		return fmt.Errorf("prepare origin directory: %w", err)
	}

	config.RecoverStaleSocket = true

	return serve(ctx, config, origin)
}

// Gantry owns the origin endpoint; Racer creates only the client endpoint. The
// shared mount must exist, but Gantry may start before Racer creates the cache
// directory. Match Racer's 0755 directory mode without changing existing modes
// or touching either socket. As with the SDK, ancestors must be trusted against
// concurrent replacement, and symlink traversal is refused.
func prepareRacerOriginDirectory(root string) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return fmt.Errorf("invalid socket root %q", root)
	}

	parent := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(root, parent), parent) {
		parent = filepath.Join(parent, part)
		if err := racerSocketDirectory(parent); err != nil {
			return err
		}
	}

	for _, part := range []string{CacheName, "origin"} {
		parent = filepath.Join(parent, part)
		if err := os.Mkdir(parent, 0o755); err != nil && !os.IsExist(err) {
			return err
		}

		if err := racerSocketDirectory(parent); err != nil {
			return err
		}
	}

	return nil
}

func racerSocketDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}

	if !info.IsDir() {
		return fmt.Errorf("socket directory %q is not a directory or is a symlink", path)
	}

	return nil
}

// HTTP content handler.

// Client is satisfied by the SDK client, including its protocol fake.
type Client interface {
	Get(context.Context, racersdk.Request, ...racersdk.ReadOptions) (*racersdk.Object, error)
	Stat(context.Context, racersdk.Request) (racersdk.Metadata, error)
}

type authenticationChallenger interface {
	AuthenticationChallenge(context.Context, string) (string, bool, error)
}

// repositoryAuthenticationChallenger recovers an origin challenge for the exact
// requested resource without fetching content or forwarding caller credentials.
type repositoryAuthenticationChallenger interface {
	RepositoryAuthenticationChallenge(context.Context, ifaces.OriginRef) (string, bool, error)
}

// Handler serves Racer content without falling back to another backend.
type Handler struct {
	client      Client
	auth        authenticationChallenger
	logger      *slog.Logger
	diagnostics racerFailureDiagnostics
}

// NewHandler preserves the mirror's logging context and optional auth challenges.
func NewHandler(client Client, auth authenticationChallenger, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}

	return &Handler{client: client, auth: auth, logger: logger.With(slog.String("subsystem", "mirror"))}
}

// ServeContent serves a validated reference. Positive blob offsets select a
// pinned resume; offsets for other kinds are ignored. The mirror owns parsing
// the caller's Range header.
func (s *Handler) ServeContent(w http.ResponseWriter, r *http.Request, ref ifaces.OriginRef) {
	writeRacerDownloadHeaders(w)

	// Fresh opaque correlation, including for aborts after headers are committed.
	id := rand.Text()
	w.Header().Set("Gantry-Racer-Request-ID", id)

	if s.client == nil {
		s.logRacerFailure(id, "client", errors.New("unavailable"), -1, 0)
		http.Error(w, "Racer unavailable", http.StatusServiceUnavailable)

		return
	}

	var offset int64
	if ref.Kind == ifaces.KindBlob && ref.Offset > 0 {
		offset = ref.Offset
	}

	ranged := offset > 0
	ref.Offset = 0
	d, kind := ref.Digest, ref.Kind

	request, err := Request(ref, registryauth.Authorization(r.Context()))
	if err != nil {
		s.logRacerFailure(id, "request", err, -1, 0)
		http.Error(w, "invalid Racer request", http.StatusBadRequest)

		return
	}

	metadata, options, done := s.prepareResponse(w, r, id, ref, request, offset)
	if done {
		return
	}

	value, err := s.client.Get(r.Context(), request, options...)
	if err != nil {
		expected := int64(-1)
		if ranged {
			expected = int64(metadata.Size) - offset
		}

		s.logRacerFailure(id, "get", err, expected, 0)
		s.racerError(w, r, ref, err)

		return
	}

	defer func() { _ = value.Close() }() //nolint:errcheck // best-effort close

	actual := value.Metadata()
	if !validRacerMetadata(actual, d) || (ranged && (actual.Size != metadata.Size ||
		actual.ContentType != metadata.ContentType)) {
		s.logRacerFailure(id, "get_metadata", errors.New("invalid metadata"), -1, 0)
		http.Error(w, "invalid Racer metadata", http.StatusBadGateway)

		return
	}

	if kind == ifaces.KindManifest && actual.Size > racersdk.PageSize {
		s.logRacerFailure(id, "manifest_size", errors.New("manifest too large"), int64(actual.Size), 0)
		http.Error(w, "Racer manifest too large", http.StatusBadGateway)

		return
	}

	if ranged {
		// The returned size, digest, and MIME type match the selected snapshot.
		actual = metadata
	}

	s.streamContent(w, id, ref, actual, offset, value)
}

// prepareResponse handles HEAD and validates the snapshot needed for blob resume.
// A true done result means it already wrote a final response; no Object is open.
// Full GETs get metadata from Get instead of issuing a redundant Stat.
func (s *Handler) prepareResponse(w http.ResponseWriter, r *http.Request, id string, ref ifaces.OriginRef, request racersdk.Request, offset int64) (racersdk.Metadata, []racersdk.ReadOptions, bool) {
	if r.Method != http.MethodHead && offset == 0 {
		if ref.Kind == ifaces.KindManifest {
			return racersdk.Metadata{}, []racersdk.ReadOptions{{SmallObject: true}}, false
		}

		return racersdk.Metadata{}, nil, false
	}

	metadata, err := s.client.Stat(r.Context(), request)
	if err != nil {
		s.logRacerFailure(id, "stat", err, -1, 0)
		s.racerError(w, r, ref, err)

		return racersdk.Metadata{}, nil, true
	}

	if !validRacerMetadata(metadata, ref.Digest) {
		s.logRacerFailure(id, "stat_metadata", errors.New("invalid metadata"), -1, 0)
		http.Error(w, "invalid Racer metadata", http.StatusBadGateway)

		return racersdk.Metadata{}, nil, true
	}

	if r.Method == http.MethodHead {
		writeRacerHeaders(w, ref.Digest, metadata, ref.Kind)
		w.WriteHeader(http.StatusOK)

		return metadata, nil, true
	}

	if offset >= int64(metadata.Size) {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", metadata.Size))
		http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)

		return metadata, nil, true
	}

	// Pin the version and compare returned metadata before serving it.
	options := []racersdk.ReadOptions{{Offset: offset, ETag: metadata.ETag, SmallObject: metadata.Size <= racersdk.PageSize}}

	return metadata, options, false
}

func (s *Handler) streamContent(w http.ResponseWriter, id string, ref ifaces.OriginRef, metadata racersdk.Metadata, offset int64, value *racersdk.Object) {
	size := int64(metadata.Size)
	writeRacerHeaders(w, ref.Digest, metadata, ref.Kind)

	if offset > 0 {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, size-1, size))
		w.Header().Set("Content-Length", strconv.FormatInt(size-offset, 10))
		w.WriteHeader(http.StatusPartialContent)
	}

	remaining := size
	if offset > 0 {
		remaining -= offset
	}
	// Publish nonempty response headers without waiting for payload. Empty
	// responses cannot withhold a final byte, so validate Complete before flushing.
	if remaining > 0 {
		if err := http.NewResponseController(w).Flush(); err != nil {
			s.logRacerFailure(id, "flush_headers", err, remaining, 0)
			panic(http.ErrAbortHandler)
		}
	}
	// The SDK gates the final byte on Complete, including for resumed ranges.
	// Earlier incomplete page prefixes may already be visible on failure: abort
	// rather than append an error body or let net/http complete the response.
	// OCI digest verification belongs to the consumer's assembled object.
	if n, err := value.WriteTo(w); err != nil || n != remaining {
		s.logRacerFailure(id, "write_body", err, remaining, n)
		panic(http.ErrAbortHandler)
	}
}

func validRacerMetadata(metadata racersdk.Metadata, d digest.Digest) bool {
	return metadata.Size >= 0 && metadata.ETag == `"`+d.String()+`"`
}

func writeRacerHeaders(w http.ResponseWriter, d digest.Digest, metadata racersdk.Metadata, kind ifaces.OriginRefKind) {
	w.Header().Set("Content-Type", metadata.ContentType)
	w.Header().Set("Docker-Content-Digest", d.String())
	// Match mirror headers without payload sniffing: Racer owns the body stream.
	if metadata.ContentType == "" {
		switch kind {
		case ifaces.KindManifest:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		case ifaces.KindBlob:
			w.Header().Set("Content-Type", "application/octet-stream")
		}
	}

	if size := int64(metadata.Size); size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}

	w.Header().Set("Gantry-Mirrored", "1")
}

func (s *Handler) racerError(w http.ResponseWriter, r *http.Request, ref ifaces.OriginRef, err error) {
	if s.auth != nil && errors.Is(err, racersdk.ErrUnauthorized) {
		// The rejecting origin callback may run on another node. Its challenge
		// cache is unavailable, and a public /v2/ root says nothing about this repo.
		challengeCtx, cancel := context.WithTimeout(r.Context(), authenticationChallengeTimeout)

		var (
			challenge    string
			required     bool
			challengeErr error
		)
		if auth, ok := s.auth.(repositoryAuthenticationChallenger); ok {
			challenge, required, challengeErr = auth.RepositoryAuthenticationChallenge(challengeCtx, ref)
		} else {
			challenge, required, challengeErr = s.auth.AuthenticationChallenge(challengeCtx, ref.Registry)
		}

		cancel()

		if challengeErr == nil && required && challenge != "" {
			w.Header().Set("WWW-Authenticate", challenge)
		}
	}

	writeRacerError(w, err)
}

func writeRacerError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway

	switch {
	case errors.Is(err, racersdk.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, racersdk.ErrUnauthorized):
		status = http.StatusUnauthorized
	case errors.Is(err, racersdk.ErrForbidden):
		status = http.StatusForbidden
	case errors.Is(err, racersdk.ErrUnavailable), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), errors.Is(err, net.ErrClosed):
		status = http.StatusServiceUnavailable
	}

	http.Error(w, "Racer request failed", status)
}

// Downstream HTTP writes.

// Registry objects are downloads, not browser documents. Attachment also covers
// explicit HTML, SVG, and XML types, which nosniff alone does not make safe.
// Preserve the original media type and bytes for OCI clients and digest checks.
func writeRacerDownloadHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

// HTTPObservation reports actual downstream bytes, final HTTP status, and
// handler duration. Aborted distinguishes incomplete responses after headers.
type HTTPObservation struct {
	Method   string
	Status   int
	Bytes    int64
	Duration time.Duration
	Aborted  bool
}

// WrapHTTP bounds each write and flush independently, so progressing
// responses have no overall deadline. Zero timeout selects 30 seconds. observe
// may be nil; otherwise it must be concurrency-safe and must not block.
func WrapHTTP(next http.Handler, timeout time.Duration, observe func(HTTPObservation)) http.Handler {
	if timeout == 0 {
		timeout = defaultWriteTimeout
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		writer := &racerResponseWriter{ResponseWriter: w, timeout: timeout}
		completed := false

		defer func() {
			if observe != nil {
				status := writer.status
				if status == 0 && completed {
					status = http.StatusOK
				}

				observe(HTTPObservation{Method: r.Method, Status: status, Bytes: writer.bytes, Duration: time.Since(started), Aborted: !completed || writer.failed})
			}
		}()

		next.ServeHTTP(writer, r)
		// Flush the final buffered headers/body under a fresh deadline, including
		// HEAD and short responses. net/http's final flush must not be unbounded.
		if err := writer.FlushError(); err != nil {
			panic(http.ErrAbortHandler)
		}
		// No upstream work remains. Keep final protocol bytes (HTTP/1 chunk
		// terminator or HTTP/2 END_STREAM) bounded when net/http finishes the
		// handler. net/http owns deadline cleanup at that lifecycle boundary.
		if err := writer.deadline(); err != nil {
			panic(http.ErrAbortHandler)
		}

		completed = true
	})
}

type racerResponseWriter struct {
	http.ResponseWriter
	timeout time.Duration
	status  int
	bytes   int64
	failed  bool
	// Only deadline state is shared with the SDK cancellation callback. Status,
	// bytes, and failed remain owned by the handler goroutine.
	deadlineMu       sync.Mutex
	externalDeadline time.Time
	interrupted      bool
}

func (w *racerResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// SetWriteDeadline coordinates SDK deadlines with rolling handler deadlines.
// An immediate interruption is sticky until explicitly cleared. Serializing the
// underlying calls as well as the state prevents a refresh from racing past it.
func (w *racerResponseWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadlineMu.Lock()
	defer w.deadlineMu.Unlock()

	if !w.interrupted || deadline.IsZero() {
		w.externalDeadline = deadline
		w.interrupted = !deadline.IsZero() && !deadline.After(time.Now())
	}

	return http.NewResponseController(w.ResponseWriter).SetWriteDeadline(w.externalDeadline)
}

func (w *racerResponseWriter) deadline() error {
	w.deadlineMu.Lock()
	defer w.deadlineMu.Unlock()

	now := time.Now()
	if w.interrupted || !w.externalDeadline.IsZero() && !w.externalDeadline.After(now) {
		w.failed = true
		return os.ErrDeadlineExceeded
	}

	deadline := now.Add(w.timeout)
	if !w.externalDeadline.IsZero() && w.externalDeadline.Before(deadline) {
		deadline = w.externalDeadline
	}

	err := http.NewResponseController(w.ResponseWriter).SetWriteDeadline(deadline)
	if errors.Is(err, http.ErrNotSupported) {
		return nil // Recorders and non-network writers have no socket to deadline.
	}

	if err != nil {
		w.failed = true
	}

	return err
}

// End a downstream operation without clearing an SDK cancellation interrupt.
// Keep the external bound as state for nested Write calls, but disarm its timer
// during upstream-only waits, including ReadFrom's userspace copy fallback.
func (w *racerResponseWriter) clearDeadline() error {
	w.deadlineMu.Lock()
	defer w.deadlineMu.Unlock()

	if w.interrupted {
		return nil
	}

	err := http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Time{})
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}

	if err != nil {
		w.failed = true
	}

	return err
}

func (w *racerResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}

	writeRacerDownloadHeaders(w.ResponseWriter)

	if w.Header().Get("Content-Type") == "" {
		w.ResponseWriter.Header().Set("Content-Type", "application/octet-stream")
	}

	if err := w.deadline(); err != nil {
		panic(http.ErrAbortHandler)
	}

	if status >= 200 || status == http.StatusSwitchingProtocols {
		w.status = status
	}

	w.ResponseWriter.WriteHeader(status)

	if err := w.clearDeadline(); err != nil {
		panic(http.ErrAbortHandler)
	}
}

func (w *racerResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		// Untyped bodies must not be sniffed as HTML. Explicit header commits
		// apply the same default in WriteHeader, including Flush and ReadFrom.
		if w.Header().Get("Content-Type") == "" {
			w.ResponseWriter.Header().Set("Content-Type", "application/octet-stream")
		}

		w.WriteHeader(http.StatusOK)
	}

	total := 0

	for len(p) > 0 {
		if err := w.deadline(); err != nil {
			return total, err
		}

		chunk := p[:min(len(p), racerWriteChunk)]

		n, err := w.ResponseWriter.Write(chunk)
		if clearErr := w.clearDeadline(); err == nil {
			err = clearErr
		}

		total += n

		w.bytes += int64(n)
		if err == nil && n != len(chunk) {
			err = io.ErrShortWrite
		}

		if err != nil {
			w.failed = true
			return total, err
		}

		p = p[n:]
	}

	return total, nil
}

// ReadFrom keeps net/http in charge of framing and connection reuse. Only a
// bounded source supplied by the SDK can use the underlying fast path. Preserve
// the concrete socket under a single limiter so net.TCPConn can recognize it.
func (w *racerResponseWriter) ReadFrom(r io.Reader) (int64, error) {
	source, bounded := r.(*io.LimitedReader)

	fast, supported := w.ResponseWriter.(io.ReaderFrom)
	if !bounded || !supported {
		if err := w.clearDeadline(); err != nil {
			return 0, err
		}

		return io.Copy(struct{ io.Writer }{w}, r)
	}

	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}

	var total int64

	for source.N > 0 {
		if err := w.deadline(); err != nil {
			return total, err
		}

		remaining := source.N
		source.N = min(remaining, int64(racerSocketChunk))
		chunk := source.N

		n, err := fast.ReadFrom(source)
		if clearErr := w.clearDeadline(); err == nil {
			err = clearErr
		}

		consumed := chunk - source.N
		source.N += remaining - chunk
		total += n

		w.bytes += n
		if err == nil && (n != chunk || consumed != chunk) {
			err = io.ErrUnexpectedEOF
		}

		if err != nil {
			w.failed = true
			return total, err
		}
	}

	return total, nil
}

func (w *racerResponseWriter) Flush() {
	if err := w.FlushError(); err != nil {
		panic(http.ErrAbortHandler)
	}
}

func (w *racerResponseWriter) FlushError() error {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}

	if err := w.deadline(); err != nil {
		return err
	}

	err := http.NewResponseController(w.ResponseWriter).Flush()
	clearErr := w.clearDeadline()

	if errors.Is(err, http.ErrNotSupported) {
		err = nil
	}

	if err == nil {
		err = clearErr
	}

	if err != nil {
		w.failed = true
	}

	return err
}

// Bounded failure diagnostics.

// One fixed-size limiter per mirror, never keyed by request or object. A burst
// refills only after a minute of inactivity since the last admitted event, so
// adjacent window boundaries cannot double the burst.
type racerFailureDiagnostics struct {
	mu         sync.Mutex
	last       time.Time
	remaining  int
	suppressed uint64
}

func (d *racerFailureDiagnostics) allow(now time.Time) (bool, uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.last.IsZero() || now.Sub(d.last) >= time.Minute {
		d.remaining = racerFailureLogBurst
	}

	if d.remaining == 0 {
		d.suppressed++
		return false, 0
	}

	d.remaining--
	d.last = now
	suppressed := d.suppressed
	d.suppressed = 0

	return true, suppressed
}

// Only fixed classifications enter the log. In particular, neither err.Error()
// nor an unwrapped cause is safe to log. Correlation is generated by the
// handler, not copied from caller headers, and is log-only (not a metric
// label). Expected bytes is -1 until valid response metadata is known; written
// bytes counts payload accepted by the writer, not client receipt.
func (s *Handler) logRacerFailure(id, stage string, err error, expected, written int64) {
	allowed, suppressed := s.diagnostics.allow(time.Now())
	if !allowed {
		return
	}

	s.logger.Warn("Racer mirror failure",
		"request_id", id, "stage", stage,
		"error_kind", racerFailureKind(err),
		"expected_bytes", expected, "written_bytes", written,
		"suppressed", suppressed)
}

// racerFailureKind classifies err without exposing any of its text.
func racerFailureKind(err error) string {
	for _, kind := range []struct {
		target error
		name   string
	}{
		{racersdk.ErrNotFound, "not found"},
		{racersdk.ErrUnauthorized, "unauthorized"},
		{racersdk.ErrForbidden, "forbidden"},
		{racersdk.ErrVersionMismatch, "version mismatch"},
		{racersdk.ErrRangeNotSatisfiable, "range not satisfiable"},
		{racersdk.ErrInvalidRequest, "invalid request"},
		{context.Canceled, "canceled"},
		{context.DeadlineExceeded, "deadline"},
		{net.ErrClosed, "closed"},
		{io.ErrUnexpectedEOF, "unexpected EOF"},
		{racersdk.ErrUnavailable, "unavailable"},
		{io.ErrShortWrite, "short write"},
	} {
		if errors.Is(err, kind.target) {
			return kind.name
		}
	}

	if err == nil {
		return "length mismatch"
	}

	return "unclassified"
}

// HTTP listener and connection admission.

// LimitListener retains TCP ReaderFrom for net/http's Unix-to-TCP splice
// path. Embedding only net.Conn, as netutil.LimitListener does, hides that method.
// Zero selects the default HTTP connection limit.
func LimitListener(listener net.Listener, limit int) net.Listener {
	if limit == 0 {
		limit = defaultHTTPMaxConnections
	}

	return &racerLimitedListener{Listener: listener, slots: make(chan struct{}, limit), done: make(chan struct{})}
}

type racerLimitedListener struct {
	net.Listener
	slots     chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

func (l *racerLimitedListener) Accept() (net.Conn, error) {
	select {
	case <-l.done:
		return nil, net.ErrClosed
	case l.slots <- struct{}{}:
	}

	c, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}

	select {
	case <-l.done:
		err := c.Close()

		<-l.slots

		return nil, errors.Join(net.ErrClosed, err)
	default:
	}

	limited := &racerLimitedConn{Conn: c, release: func() { <-l.slots }}
	if reader, ok := c.(io.ReaderFrom); ok {
		return &racerLimitedReaderConn{racerLimitedConn: limited, reader: reader}, nil
	}

	return limited, nil
}

func (l *racerLimitedListener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return l.Listener.Close()
}

type racerLimitedConn struct {
	net.Conn
	releaseOnce sync.Once
	release     func()
}

func (c *racerLimitedConn) Close() error {
	err := c.Conn.Close()
	c.releaseOnce.Do(c.release)

	return err
}

type racerLimitedReaderConn struct {
	*racerLimitedConn
	reader io.ReaderFrom
}

func (c *racerLimitedReaderConn) ReadFrom(r io.Reader) (int64, error) {
	return c.reader.ReadFrom(r)
}

// Mirror and origin metrics.

// Metrics records bounded-label HTTP observations.
type Metrics struct {
	requests        *prometheus.CounterVec
	bytes           *prometheus.CounterVec
	durations       *prometheus.HistogramVec
	originRequests  *prometheus.CounterVec
	originBodyBytes *prometheus.CounterVec
}

// NewMetrics registers mirror and origin metrics.
func NewMetrics(reg *metrics.Registry) *Metrics {
	m := &Metrics{
		requests:        reg.NewCounterVec("racer", prometheus.CounterOpts{Name: "gantry_racer_mirror_requests_total", Help: "Racer mirror responses by method, HTTP status, and completion."}, []string{"method", "status", "outcome"}),
		bytes:           reg.NewCounterVec("racer", prometheus.CounterOpts{Name: "gantry_racer_mirror_bytes_total", Help: "Body bytes written downstream, including partial responses."}, []string{"method"}),
		durations:       reg.NewHistogramVec("racer", prometheus.HistogramOpts{Name: "gantry_racer_mirror_duration_seconds", Help: "Racer mirror handler duration including downstream writes.", Buckets: prometheus.ExponentialBuckets(0.001, 4, 11)}, []string{"method"}),
		originRequests:  reg.NewCounterVec("racer", prometheus.CounterOpts{Name: "gantry_racer_origin_requests_total", Help: "Upstream HTTP round trips, including HEAD, GET, and authentication; status zero denotes transport failure."}, []string{"method", "status"}),
		originBodyBytes: reg.NewCounterVec("racer", prometheus.CounterOpts{Name: "gantry_racer_origin_bytes_total", Help: "Upstream GET body bytes read, including partial transfers."}, []string{"kind"}),
	}

	return m
}

func racerMetricMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead:
		return method
	default:
		return "other"
	}
}

// MirrorResponse records actual bytes and completion, including aborted streams.
func (m *Metrics) MirrorResponse(o HTTPObservation) {
	method := racerMetricMethod(o.Method)

	outcome := "complete"
	if o.Aborted {
		outcome = "aborted"
	}

	m.requests.WithLabelValues(method, strconv.Itoa(o.Status), outcome).Inc()
	m.bytes.WithLabelValues(method).Add(float64(o.Bytes))
	m.durations.WithLabelValues(method).Observe(o.Duration.Seconds())
}

// OriginRequest records upstream round trips, including authentication requests.
func (m *Metrics) OriginRequest(method string, status int) {
	m.originRequests.WithLabelValues(racerMetricMethod(method), strconv.Itoa(status)).Inc()
}

// OriginBytes records upstream bytes, including partial transfers.
func (m *Metrics) OriginBytes(kind string, bytes int64) {
	m.originBodyBytes.WithLabelValues(kind).Add(float64(bytes))
}
