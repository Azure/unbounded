// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package ifaces declares the cross-cutting interfaces that Gantry's
// subsystems implement and depend on.
//
// Each subsystem (cache, members, origin, peer, DHT) is reachable through
// the interfaces defined here so that:
//
// - Unit tests can replace any subsystem with a fake (see internal/ifaces/fakes).
// - The top-level agent wiring in internal/agent depends only on interfaces,
// not on concrete libp2p / Kubernetes / hostPath implementations.
//
// Interfaces are intentionally minimal - only the methods the agent actually
// uses are exposed. Adding a method here should follow real demand from a
// caller, not speculative API surface.
package ifaces

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
)

// ---------------------------------------------------------------------------
// LocalContentStore: on-disk content store for blobs and manifests.
// Implemented by internal/containerdstore (the production backend) and
// internal/ifaces/fakes.Cache (the in-memory test double).
// ---------------------------------------------------------------------------

// LocalContentStore is a content-addressed store keyed by OCI digest.
// Implementations MUST verify the streamed bytes against the digest
// before treating an entry as committed (digest-verification in architecture.md).
//
// In production this is backed by the local containerd content store
// (internal/containerdstore.Store) - the same store kubelet and CRI
// already read from / write to. There is no separate Gantry-owned
// blob cache.
type LocalContentStore interface {
	// Has reports whether the digest is present in the local store.
	Has(ctx context.Context, d digest.Digest) (bool, error)

	// Open returns a reader for the stored bytes plus the content
	// length. Returns ErrNotFound if absent.
	Open(ctx context.Context, d digest.Digest) (io.ReadCloser, int64, error)

	// Writer returns a digest-verifying writer for d. Bytes written
	// are staged; the entry becomes visible to subsequent Has/Open
	// calls only after Commit. Abort discards the staging area.
	Writer(ctx context.Context, d digest.Digest) (ContentWriter, error)
}

// ContentWriter accumulates bytes for a single LocalContentStore
// entry. The implementation computes the digest incrementally; Commit
// fails if the streamed bytes do not match the digest passed to
// LocalContentStore.Writer.
type ContentWriter interface {
	io.Writer

	// Commit finalizes the entry. Fails if the streamed bytes did not hash
	// to the declared digest.
	Commit(ctx context.Context) error

	// Abort discards staged bytes. Idempotent.
	Abort(ctx context.Context) error
}

// NodeID identifies a libp2p peer in cold-start coordination.
type NodeID string

// Node is one closest-peer candidate.
type Node struct {
	ID NodeID

	// Addr is the network address of the peer's transfer endpoint.
	Addr string
}

// ---------------------------------------------------------------------------
// OriginPuller: pulls bytes from the upstream OCI registry.
// Implemented by internal/origin .
// ---------------------------------------------------------------------------

// OriginRef identifies a digest at a specific upstream registry / repository.
// Request-scoped registry authorization is carried through context rather than
// this value so credentials cannot accidentally be persisted or logged with a
// content reference.
type OriginRef struct {
	Registry   string // e.g. "registry.example.com"
	Repository string // e.g. "library/nginx"
	Digest     digest.Digest
	// Offset requests bytes starting at this position when fetching from a
	// peer. Origin registry callers ignore it. Zero requests the full object.
	Offset int64

	// Kind discriminates the OCI Distribution Spec URL family for this
	// reference. Manifests live at /v2/<repo>/manifests/<digest>, blobs at
	// /v2/<repo>/blobs/<digest>. Zero value (KindBlob) is the common case;
	// only the mirror's manifest-by-digest path and cold-start manifest
	// pulls set KindManifest.
	Kind OriginRefKind
}

// OriginRefKind discriminates manifest vs blob URLs at the upstream.
//
// Note on KindConfig: per the OCI Distribution Spec the image-config
// document is fetched from /v2/<repo>/blobs/<digest> - the same URL
// family as KindBlob. KindConfig therefore does NOT change routing;
// it exists purely to tighten metric/log labels so cold-start manifest
// -> config -> blob traversal is distinguishable from regular layer
// fetches when reading dashboards or traces.
type OriginRefKind int

// Recognised OriginRefKind values.
const (
	KindBlob     OriginRefKind = 0
	KindManifest OriginRefKind = 1
	KindConfig   OriginRefKind = 2
)

func (k OriginRefKind) String() string {
	switch k {
	case KindManifest:
		return "manifest"
	case KindConfig:
		return "config"
	default:
		return "blob"
	}
}

// MetricLabel returns the Prometheus label vocabulary the design doc
// commits to for the p2p_origin_pull_total / _success_total /
// _failure_total counters:
//
//	p2p_origin_pull_total{kind="manifest|config|layer"}
//
// MetricLabel is intentionally distinct from String: String returns
// "blob" for KindBlob (the OCI Distribution Spec URL-family term,
// correct in logs and on the wire) while MetricLabel returns "layer"
// (the operator-facing observability term, what dashboards built
// against the design spec expect). The two roles must not be
// conflated - leaking "blob" into Prometheus labels gives dashboards
// an empty "layer" bucket plus an undocumented "blob" series.
//
// KindConfig is preserved as "config" so the per-kind counter
// distinguishes the single image-config blob per manifest from the
// many layer blobs. This is the load-bearing observability invariant
// the work plumbed end-to-end through manifest.TypedChildren
// -> coldstart.PrefetchChildren -> please_pull proto KIND_CONFIG; this
// method is the leaf node of that chain.
func (k OriginRefKind) MetricLabel() string {
	switch k {
	case KindManifest:
		return "manifest"
	case KindConfig:
		return "config"
	default:
		// KindBlob and any future zero-valued / unknown kind both
		// land here. Treating unknown as "layer" matches the OCI
		// reality (layer pulls are the dominant /blobs/ traffic)
		// and keeps the label set bounded.
		return "layer"
	}
}

// OriginPuller fetches a single digest from origin.
type OriginPuller interface {
	// Pull opens a streaming read of the digest's bytes from origin. The
	// returned ReadCloser is digest-unverified; the caller is expected to
	// verify via a Cache writer or equivalent.
	//
	// On terminal failure the returned error is wrapped in an *OriginError
	// carrying the failure classification used by the design doc.
	Pull(ctx context.Context, ref OriginRef) (io.ReadCloser, int64, error)

	// Head fetches metadata for a digest without transferring the body.
	// Used by the mirror's HEAD handler to satisfy distribution-spec
	// metadata requests on a cache miss without doing a full origin pull.
	//
	// Head is deliberately NOT counted in p2p_origin_pull_total or its
	// success/failure siblings. Those counters describe byte-pull
	// attempts: metadata-only HEAD is a different operation class, and
	// folding HEAD calls into pull totals broke the per-pull arithmetic
	// (started == success + failure + in_flight) because HEAD never
	// produces bytes, never commits to cache, and therefore can fire
	// neither success nor downstream-failure. See origin.Client.Head's
	// implementation comment for why HEAD failures also stay out of
	// p2p_origin_pull_failure_total.
	//
	// The returned contentType is the upstream registry's Content-Type
	// header verbatim (may be "" if the upstream omitted it). The mirror
	// needs this so that HEAD responses to containerd carry the same
	// media type the eventual GET will. Without it containerd builds a
	// descriptor with the wrong type at HEAD time and later fails the
	// unpack with "expected manifest but found index" when the body is
	// a manifest list (see mirror.writeBlobHeadersWithPrefix).
	//
	// On terminal failure the returned error is wrapped in an
	// *OriginError carrying the failure classification used by the design doc so
	// the mirror can convert it to the right HTTP status.
	Head(ctx context.Context, ref OriginRef) (size int64, contentType string, err error)
}

// OriginError is the error returned by OriginPuller.Pull for terminal
// failures. The Class field is the classification used by the negative
// cache and propagated via PullIntentResponse.failure_class.
type OriginError struct {
	Ref       OriginRef
	Class     FailureClass
	Challenge string
	Err       error
}

func (e *OriginError) Error() string {
	if e.Err == nil {
		return "origin error: " + string(e.Class)
	}

	return "origin error (" + string(e.Class) + "): " + e.Err.Error()
}

func (e *OriginError) Unwrap() error { return e.Err }

// FailureClass mirrors coordv1.FailureClass; defined here so non-proto
// callers don't import the generated package.
type FailureClass string

// Recognised the design doc failure classifications.
const (
	FailureUnspecified FailureClass = ""
	FailureAuth        FailureClass = "auth"
	FailureNotFound    FailureClass = "not_found"
	FailureRateLimited FailureClass = "rate_limited"
	FailureTransient   FailureClass = "transient"
)

// ---------------------------------------------------------------------------
// PeerDialer: fetches a digest from another agent's :5001 transfer endpoint.
// Implemented by internal/transfer .
// ---------------------------------------------------------------------------

// PeerDialer fetches a digest from a peer's transfer endpoint with the
// `Gantry-Mirrored: 1` header set (architecture.md the API contract).
type PeerDialer interface {
	// FetchFromPeer streams the digest's bytes from peerAddr's :5001
	// endpoint. The implementation MUST set `Gantry-Mirrored: 1` and MUST
	// forward any request-scoped Basic/Bearer authorization carried by ctx, and MUST
	// surface a NotFound error distinctly from transport errors so the
	// caller can fail over to the next provider. When ref.Offset is non-zero,
	// the implementation MUST request and validate a response beginning at
	// that byte and still return the full object size. A successful response
	// MUST return its Content-Type so the caller can commit outer headers
	// without waiting for body bytes.
	FetchFromPeer(ctx context.Context, peerAddr string, ref OriginRef) (body io.ReadCloser, size int64, contentType string, err error)
}

// ---------------------------------------------------------------------------
// DHT: digest-keyed discovery layer.
// Implemented by internal/discovery .
// ---------------------------------------------------------------------------

// Provider is one entry returned by DHT.FindProviders.
type Provider struct {
	NodeID NodeID
	Addr   string
}

// DHT exposes the libp2p Kademlia operations Gantry needs.
type DHT interface {
	// FindProviders returns providers of d. Returning an empty slice and a
	// nil error is the "DHT-empty" case: the caller MUST NOT treat it as
	// ground truth and SHOULD fall through to the closest-peer probe.
	FindProviders(ctx context.Context, d digest.Digest) ([]Provider, error)

	// Provide advertises that this node holds d. Idempotent at the DHT
	// level; refreshing is the implementation's responsibility (libp2p
	// default 12 h refresh, 24 h TTL - the design doc).
	Provide(ctx context.Context, d digest.Digest) error

	// Withdraw is a soft "stop advertising" hint sent by the advertiser
	// when the digest is no longer present in the local content store
	// (e.g. containerd GC'd it). libp2p has no protocol-level withdraw
	// - existing provider records expire at the 24 h TTL - so this is
	// implementation-defined cooperation: at minimum the local agent
	// MUST stop re-Providing the digest on the next refresh cycle so
	// the stale record drains naturally. Returning a non-nil error
	// signals the advertiser to retry; nil means "acknowledged, do not
	// re-announce".
	Withdraw(ctx context.Context, d digest.Digest) error

	// Health returns the current DHT health score in [0,1] as defined by
	// the design doc (geometric mean of routing-table coverage, lookup-latency
	// score, and self-test success rate).
	Health() float64
}

// ---------------------------------------------------------------------------
// Coordinator: libp2p coordination RPC client (caller side).
// Implemented by internal/coord .
// ---------------------------------------------------------------------------

// PullIntent is the requester-side view of a PullIntentResponse.
type PullIntent struct {
	HasCached      bool
	InFlight       bool
	StartedAt      time.Time
	RecipientRank  int32
	RecentlyFailed bool
	CooldownUntil  time.Time
	FailureClass   FailureClass
}

// PleasePullOutcome is the requester-side view of a single
// PleasePullResponse.Result.
type PleasePullOutcome struct {
	Digest        digest.Digest
	Outcome       PleasePullStatus
	StartedAt     time.Time
	CooldownUntil time.Time
	FailureClass  FailureClass
}

// PleasePullStatus mirrors coordv1.PleasePullResponse.Result.Outcome.
type PleasePullStatus int

// Recognised PleasePull outcome values.
const (
	PleasePullUnspecified PleasePullStatus = iota
	PleasePullAlreadyPulling
	PleasePullStarted
	PleasePullRecentlyFailed
)

// Coordinator issues coordination RPCs to peers. Implementations are
// expected to open one libp2p stream per call.
type Coordinator interface {
	PullIntentQuery(ctx context.Context, peer NodeID, d digest.Digest) (PullIntent, error)
	PleasePull(ctx context.Context, peer NodeID, registry, repository string, kind OriginRefKind, digests []digest.Digest) ([]PleasePullOutcome, error)
}

// LocalIntentProvider computes the PullIntent for self synchronously,
// without going through a libp2p coord stream. The cold-start
// orchestrator uses it to include self as a first-class participant
// in the rule cascade so that self can pull when it is the nearest
// candidate instead of delegating to the next peer.
type LocalIntentProvider interface {
	LocalPullIntent(ctx context.Context, d digest.Digest) PullIntent
}

// LocalPullStarter starts an origin pull on the local node without
// going through a libp2p coord stream. The cold-start orchestrator
// invokes this when rule 7 selects self as the designated puller;
// the wire-level alternative (Coord.PleasePull(self, ...)) would
// either fail to dial self or - worse - round-trip through libp2p
// and burn a stream slot. Semantics MUST match the server-side
// please_pull handler: each digest either starts a new origin pull,
// piggybacks on an already-in-flight one (PleasePullAlreadyPulling),
// or short-circuits on the negative cache (PleasePullRecentlyFailed).
type LocalPullStarter interface {
	StartLocalPull(ctx context.Context, registry, repository string, kind OriginRefKind, digests []digest.Digest) ([]PleasePullOutcome, error)
}

// ---------------------------------------------------------------------------
// Errors.
// ---------------------------------------------------------------------------

// ErrNotFound is returned by Cache and PeerDialer to signal a digest is not
// locally available. Distinct from transport-level errors so callers can
// distinguish "fall back to next provider" from "definitively missing here".
type ErrNotFound struct {
	Digest digest.Digest
}

func (e *ErrNotFound) Error() string { return "not found: " + e.Digest.String() }

// ErrUnavailable signals that the local storage backend (typically
// containerd) is currently unreachable or otherwise unable to answer
// presence/open requests. Per callers
// MUST distinguish this from ErrNotFound:
//
// - mirror/coord must NOT report "cache miss" (which would trigger
// DHT lookup + cold-start + origin fallback on data that may
// still be on-node);
// - transfer must respond HTTP 503 (not 404), so a peer treats this
// node as temporarily down rather than as definitive proof the
// digest is absent;
// - advertise must pause Withdraw on an Inventory failure, since
// an empty inventory caused by an unavailable backend would
// otherwise look like "everything was evicted".
//
// Cause is the underlying error (typically a gRPC Unavailable, a
// connection refused, or a context.DeadlineExceeded). Op is a short
// operation name ("Info"/"ReaderAt"/"Writer"/"Walk") for log
// correlation.
type ErrUnavailable struct {
	Op    string
	Cause error
}

func (e *ErrUnavailable) Error() string {
	if e.Op == "" {
		return "storage backend unavailable: " + e.Cause.Error()
	}

	return "storage backend unavailable (" + e.Op + "): " + e.Cause.Error()
}

func (e *ErrUnavailable) Unwrap() error { return e.Cause }

// ErrPeerHTTPStatus is returned by PeerDialer implementations when a peer
// transfer endpoint responds with an unexpected HTTP status. Callers can use
// StatusCode to classify failures (auth/config, server error, protocol error)
// without parsing error strings.
type ErrPeerHTTPStatus struct {
	PeerAddr   string
	StatusCode int
	RetryAfter time.Duration
}

func (e *ErrPeerHTTPStatus) Error() string {
	return fmt.Sprintf("peer %s returned %d", e.PeerAddr, e.StatusCode)
}
