// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package coord implements Gantry's libp2p coordination RPCs.
//
// Wire protocol: `/gantry/coord/1.0.0` (one libp2p stream per
// request/response pair, closed after reply). Framing: length-delimited
// protobuf via `go-msgio` - the design forbids gRPC (the design doc). Forward
// compatibility: additive changes bump the minor (e.g. `1.1.0`);
// breaking changes bump the major.
//
// Two coordinated message families:
//
// - `pull_intent_query` / `pull_intent_response` (the step 4) - a
// stateless probe asking a peer "do you have this digest cached,
// are you pulling it, or have you recently failed to pull it?".
// The responder fills hrw_rank from its own view of cluster
// membership so the requester can detect informer divergence
// (the design doc).
//
// - `please_pull` / `please_pull_response` (the step 6) - asks a
// peer (the designated puller per HRW) to pull one or more digests
// of a single repo. The responder's in-flight map dedupes; we get
// STARTED / ALREADY_PULLING / RECENTLY_FAILED per-digest results
// back.
//
// This package owns both the server-side stream handler and a typed
// client. Higher layers (cold-start orchestrator, mirror) interact
// only via the `ifaces.Coordinator` interface.
package coord

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-msgio"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/hrw"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/inflight"
	coordv1 "github.com/Azure/unbounded/internal/gantry/proto/coord/v1"
)

// ProtocolID is the libp2p stream protocol the coord handler binds.
const ProtocolID protocol.ID = "/gantry/coord/1.0.0"

// MaxMessageBytes caps a single inbound Envelope. PullIntentRequest is
// tiny; PleasePullRequest grows linearly with batch size. 1 MiB is
// orders of magnitude beyond the realistic ceiling but keeps memory
// bounded under malformed input.
const MaxMessageBytes = 1 << 20

// DefaultStreamHandshakeTimeout bounds how long the server is willing
// to wait for the *first* envelope on an inbound stream and how long
// it gives itself to write the response. A peer that opens a stream
// and never sends bytes - accidental (NAT death) or malicious
// (slowloris-style resource exhaustion) - must not pin a goroutine
// indefinitely.
//
// 5s comfortably covers a healthy in-cluster round-trip while still
// bounding the worst case. The deadline is set on the underlying
// libp2p stream so both r.ReadMsg and w.WriteMsg observe it; dispatch
// runs under its own 2s context (see handleStream). Override via
// WithStreamHandshakeTimeout for tests.
const DefaultStreamHandshakeTimeout = 5 * time.Second

// DefaultMaxConcurrentStreams caps simultaneous inbound coord streams. A
// hostile or buggy peer can open thousands of streams; without a cap each
// consumes a goroutine for up to streamHandshakeTimeout. libp2p's resource
// manager is the next defence layer; this is a cheap, predictable, server-
// local gate.
const DefaultMaxConcurrentStreams = 512

// DefaultMaxDigestsPerPleasePull caps a single inbound please_pull batch. It
// is well above normal OCI manifest child counts while preventing a 1 MiB
// coord envelope from expanding into thousands of pump calls.
const DefaultMaxDigestsPerPleasePull = 256

// MetricsHooks lets callers wire Prometheus counters/gauges without
// importing the metrics package. All fields may be nil.
type MetricsHooks struct {
	// OnPullIntentServed fires once per pull_intent_query handled.
	OnPullIntentServed func()
	// OnPullIntentStorageUnavailable fires once per pull_intent_query
	// (wire or local) whose has_cached=false answer was caused by the
	// primary or secondary storage backend returning ifaces.ErrUnavailable
	// rather than a definitive miss. Operators use this to distinguish
	// "DHT routes around us because we genuinely lack the blob" from
	// "DHT routes around us because containerd is unreachable on this
	// node" - the latter should also be caught by readiness, but the
	// metric makes transient storage flaps observable independently of
	// the readyz signal.
	OnPullIntentStorageUnavailable func()
	// OnPleasePullServed fires once per please_pull *request* handled
	// (not per digest in the batch).
	OnPleasePullServed func()
	// OnPleasePullStarted is called once per digest the server
	// transitions into in_flight from a please_pull batch.
	OnPleasePullStarted func()
	// OnPleasePullDeclined fires once per digest the server declines to
	// start (PumpDeclined): the puller-pump refused the work because the
	// node is at its concurrent-pull ceiling or is shutting down. The
	// digest is reported to the requester as OUTCOME_UNSPECIFIED. This is
	// the load-shedding signal operators watch during large rollouts; a
	// sustained nonzero rate means designated pullers are saturated and
	// requesters are falling through to direct-origin fallback (NF5).
	OnPleasePullDeclined func()
	// OnStreamError fires for any malformed or oversized stream.
	OnStreamError func()
}

// Server handles inbound coord streams: pull_intent_query and
// please_pull RPCs. One stream per request, closed after reply.
type Server struct {
	logger   *slog.Logger
	hooks    MetricsHooks
	store    ifaces.LocalContentStore
	members  ifaces.Members
	inflight *inflight.Map
	// negCache is consulted by pull_intent_query to populate
	// recently_failed / cooldown_until / failure_class. lands a
	// real implementation; ships with a nil-safe call site so
	// the wire field round-trips even before the negative cache exists.
	negCache NegativeCache
	// pullerPump is invoked by the please_pull handler for each
	// (registry, repository, digest) we accept. The supplied function
	// is expected to start a background pull (it must not block the
	// stream handler). nil disables please_pull semantically - the
	// handler still acks but with OUTCOME_UNSPECIFIED.
	pullerPump PullerPump
	// streamHandshakeTimeout caps a single inbound stream's wire
	// lifetime (see DefaultStreamHandshakeTimeout).
	streamHandshakeTimeout time.Duration
	// streamSem bounds concurrent inbound stream handlers.
	streamSem chan struct{}
	// maxDigestsPerPleasePull bounds a single inbound please_pull batch.
	maxDigestsPerPleasePull int
}

// NegativeCache is the read interface coord needs from the
// circuit-breaker . Returning ok == false means the digest
// has no negative-cache entry on this node.
type NegativeCache interface {
	Lookup(d digest.Digest) (entry NegativeEntry, ok bool)
}

// NegativeEntry mirrors the design doc state for a single digest.
type NegativeEntry struct {
	CooldownUntil time.Time
	Class         ifaces.FailureClass
}

// PullerPump is invoked by the please_pull handler with a fully-
// classified pull request. It MUST return promptly: the call happens
// inside the stream handler and the server response can't be written
// until pump returns. Long-running work (the actual origin pull) MUST
// be moved to a goroutine inside the pump's implementation.
//
// PullerPump returns a PumpResult describing whether the digest started,
// piggy-backed on existing work, short-circuited on a recent failure, or was
// declined before background work was started.
type PullerPump func(ctx context.Context, registry, repository string, d digest.Digest, kind ifaces.OriginRefKind) PumpResult

// PumpStatus is the in-process status returned by PullerPump.
type PumpStatus int

const (
	PumpStarted PumpStatus = iota
	PumpAlreadyPulling
	PumpRecentlyFailed
	PumpDeclined
)

// PumpResult is the in-process result returned by PullerPump.
type PumpResult struct {
	Status        PumpStatus
	StartedAt     time.Time
	CooldownUntil time.Time
	FailureClass  ifaces.FailureClass
}

// Option configures a Server.
type Option func(*Server)

// WithLogger plumbs a structured logger.
func WithLogger(l *slog.Logger) Option {
	return func(s *Server) {
		if l != nil {
			s.logger = l.With(slog.String("subsystem", "coord"))
		}
	}
}

// WithMetrics attaches metric callbacks.
func WithMetrics(h MetricsHooks) Option {
	return func(s *Server) { s.hooks = h }
}

// WithNegativeCache attaches a the design doc read interface. nil is fine; the
// response just doesn't set recently_failed.
func WithNegativeCache(n NegativeCache) Option {
	return func(s *Server) { s.negCache = n }
}

// WithPullerPump wires the please_pull handler to the local origin
// puller. Required for please_pull to do useful work; without it the
// handler returns OUTCOME_UNSPECIFIED.
func WithPullerPump(p PullerPump) Option {
	return func(s *Server) { s.pullerPump = p }
}

// WithStreamHandshakeTimeout overrides DefaultStreamHandshakeTimeout.
// Intended for tests; non-positive values are ignored.
func WithStreamHandshakeTimeout(d time.Duration) Option {
	return func(s *Server) {
		if d > 0 {
			s.streamHandshakeTimeout = d
		}
	}
}

// WithMaxConcurrentStreams overrides DefaultMaxConcurrentStreams. Non-positive
// values are ignored.
func WithMaxConcurrentStreams(n int) Option {
	return func(s *Server) {
		if n > 0 {
			s.streamSem = make(chan struct{}, n)
		}
	}
}

// WithMaxDigestsPerPleasePull overrides DefaultMaxDigestsPerPleasePull.
// Non-positive values are ignored.
func WithMaxDigestsPerPleasePull(n int) Option {
	return func(s *Server) {
		if n > 0 {
			s.maxDigestsPerPleasePull = n
		}
	}
}

// NewServer constructs a coord server. The store + members + inflight
// dependencies are required (everything else is optional via Option).
func NewServer(store ifaces.LocalContentStore, members ifaces.Members, inflight *inflight.Map, opts ...Option) *Server {
	s := &Server{
		logger:                  slog.Default().With(slog.String("subsystem", "coord")),
		store:                   store,
		members:                 members,
		inflight:                inflight,
		streamHandshakeTimeout:  DefaultStreamHandshakeTimeout,
		streamSem:               make(chan struct{}, DefaultMaxConcurrentStreams),
		maxDigestsPerPleasePull: DefaultMaxDigestsPerPleasePull,
	}
	for _, opt := range opts {
		opt(s)
	}

	return s
}

// Bind registers the stream handler on h. After Bind returns, peers
// dialing ProtocolID will be served by s.
func (s *Server) Bind(h host.Host) {
	h.SetStreamHandler(ProtocolID, s.handleStream)
}

// Unbind removes the stream handler registered by Bind. Shutdown calls this
// before waiting on puller-pump goroutines so no new inbound coord stream can
// start another background origin pull while the pump gate is draining.
func (s *Server) Unbind(h host.Host) {
	h.RemoveStreamHandler(ProtocolID)
}

// handleStream is invoked by libp2p for each inbound stream. The
// design pins "one stream per request/response pair" - we read one
// length-delimited envelope, dispatch, write one envelope, close.
func (s *Server) handleStream(str network.Stream) {
	// Bound concurrent handlers. A hostile peer can open many streams in
	// parallel; reject (rather than queue) excess so resource use stays
	// predictable. select-with-default keeps this O(1) and lock-free.
	select {
	case s.streamSem <- struct{}{}:
		defer func() { <-s.streamSem }()
	default:
		s.bumpStreamErr()
		s.logger.Debug("coord: max concurrent streams reached, dropping")

		_ = str.Reset() //nolint:errcheck // best-effort reset

		return
	}

	defer func() { _ = str.Close() }() //nolint:errcheck // best-effort close

	// Bound the entire request/response cycle on the wire. Without
	// this an idle peer can pin a goroutine forever; the dispatch
	// context below only bounds work *after* a full envelope arrives.
	if err := str.SetDeadline(time.Now().Add(s.streamHandshakeTimeout)); err != nil {
		s.bumpStreamErr()
		s.logger.Debug("coord: set stream deadline", slog.Any("err", err))

		return
	}

	r := msgio.NewVarintReaderSize(str, MaxMessageBytes)
	w := msgio.NewVarintWriter(str)

	bytes, err := r.ReadMsg()
	if err != nil {
		s.bumpStreamErr()
		s.logger.Debug("coord: read envelope", slog.Any("err", err))

		return
	}
	defer r.ReleaseMsg(bytes)

	in := &coordv1.Envelope{}
	if err := proto.Unmarshal(bytes, in); err != nil {
		s.bumpStreamErr()
		s.logger.Debug("coord: unmarshal envelope", slog.Any("err", err))

		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	out, err := s.dispatch(ctx, str.Conn().RemotePeer(), in)
	if err != nil {
		s.bumpStreamErr()
		s.logger.Debug("coord: dispatch", slog.Any("err", err))

		return
	}

	if out == nil {
		return
	}

	rb, err := proto.Marshal(out)
	if err != nil {
		s.bumpStreamErr()
		s.logger.Debug("coord: marshal response", slog.Any("err", err))

		return
	}

	if err := w.WriteMsg(rb); err != nil {
		s.bumpStreamErr()
		s.logger.Debug("coord: write response", slog.Any("err", err))

		return
	}
}

func (s *Server) dispatch(ctx context.Context, remote peer.ID, in *coordv1.Envelope) (*coordv1.Envelope, error) {
	switch m := in.GetMsg().(type) {
	case *coordv1.Envelope_PullIntentRequest:
		resp, err := s.servePullIntent(ctx, m.PullIntentRequest)
		if err != nil {
			return nil, err
		}

		if s.hooks.OnPullIntentServed != nil {
			s.hooks.OnPullIntentServed()
		}

		return wrapPullIntentResponse(resp), nil
	case *coordv1.Envelope_PleasePullRequest:
		resp, err := s.servePleasePull(ctx, remote, m.PleasePullRequest)
		if err != nil {
			return nil, err
		}

		if s.hooks.OnPleasePullServed != nil {
			s.hooks.OnPleasePullServed()
		}

		return wrapPleasePullResponse(resp), nil
	case nil:
		return nil, errors.New("coord: empty envelope")
	default:
		return nil, fmt.Errorf("coord: unexpected message %T (this side is a server only)", m)
	}
}

func (s *Server) servePullIntent(ctx context.Context, req *coordv1.PullIntentRequest) (*coordv1.PullIntentResponse, error) {
	d, err := digest.Parse(req.GetDigest())
	if err != nil {
		return nil, fmt.Errorf("pull_intent: %w", err)
	}

	intent := s.computeLocalIntent(ctx, d)

	resp := &coordv1.PullIntentResponse{
		HasCached:      intent.HasCached,
		InFlight:       intent.InFlight,
		HrwRank:        intent.RecipientRank,
		RecentlyFailed: intent.RecentlyFailed,
		FailureClass:   failureClassToProto(intent.FailureClass),
	}
	if !intent.StartedAt.IsZero() {
		resp.StartedAt = timestamppb.New(intent.StartedAt)
	}

	if !intent.CooldownUntil.IsZero() {
		resp.CooldownUntil = timestamppb.New(intent.CooldownUntil)
	}

	return resp, nil
}

// LocalPullIntent implements ifaces.LocalIntentProvider. It returns
// the same PullIntent the wire-level pull_intent_query handler would
// produce for d, but without the libp2p stream round-trip - the
// cold-start orchestrator uses it to include self as a first-class
// participant in the rule cascade.
func (s *Server) LocalPullIntent(ctx context.Context, d digest.Digest) ifaces.PullIntent {
	return s.computeLocalIntent(ctx, d)
}

// computeLocalIntent is the shared implementation behind
// servePullIntent (wire path) and LocalPullIntent (in-process path).
// Both must produce semantically identical results for the same d so
// that the cold-start cascade's HRW-rank-0-on-self decision matches
// what every peer would compute for us. See the step 4 and the
// LocalIntentProvider interface doc.
func (s *Server) computeLocalIntent(ctx context.Context, d digest.Digest) ifaces.PullIntent {
	intent := ifaces.PullIntent{RecipientRank: -1}

	// has_cached. The local content store is the single source of
	// truth: in containerd-only mode it IS the containerd content
	// store (containerdstore.Store.Has uses ReaderAt so a true result
	// guarantees the bytes are openable, not just listed in
	// metadata). ErrUnavailable from the backend is surfaced via the
	// OnPullIntentStorageUnavailable hook so operators can
	// distinguish "genuinely missing" from "storage flap" - we MUST
	// NOT roll a backend error into has_cached=true (the peer would
	// then issue a transfer fetch that also fails).
	if ok, err := s.store.Has(ctx, d); err == nil && ok {
		intent.HasCached = true
	} else if err != nil {
		var unavailable *ifaces.ErrUnavailable
		if errors.As(err, &unavailable) {
			s.logger.Warn("pull_intent: local storage unavailable",
				slog.String("digest", d.String()),
				slog.Any("err", err),
			)

			if s.hooks.OnPullIntentStorageUnavailable != nil {
				s.hooks.OnPullIntentStorageUnavailable()
			}
		} else {
			s.logger.Debug("pull_intent: store.Has failed",
				slog.String("digest", d.String()),
				slog.Any("err", err),
			)
		}
	}

	// in_flight / started_at
	if e, ok := s.inflight.LookupForIntent(d); ok {
		intent.InFlight = true
		intent.StartedAt = e.StartedAt
	}

	// hrw_rank - own rank in own membership view.
	if s.members != nil {
		nodes := s.members.Snapshot()
		intent.RecipientRank = hrw.RankOf(nodes, s.members.Self(), d)
	}

	// the design doc negative-cache fields.
	if s.negCache != nil {
		if e, ok := s.negCache.Lookup(d); ok {
			intent.RecentlyFailed = true
			intent.CooldownUntil = e.CooldownUntil
			intent.FailureClass = e.Class
		}
	}

	return intent
}

// StartLocalPull implements ifaces.LocalPullStarter. It runs the same
// pullerPump-driven path as servePleasePull but skips the libp2p
// stream layer entirely. Used by the cold-start orchestrator when
// rule 7 picks self as the designated puller - Coord.PleasePull(self)
// would round-trip through libp2p (or fail to dial) for no benefit.
//
// Returns one PleasePullOutcome per input digest. A nil/zero pump (no
// WithPullerPump option) yields PleasePullUnspecified entries; that
// matches the server-side behaviour and is what the cold-start
// resolver expects when origin-pull is disabled.
func (s *Server) StartLocalPull(ctx context.Context, registry, repository string, kind ifaces.OriginRefKind, digests []digest.Digest) ([]ifaces.PleasePullOutcome, error) {
	if registry == "" || repository == "" {
		return nil, errors.New("start_local_pull: missing registry/repository")
	}

	if s.maxDigestsPerPleasePull > 0 && len(digests) > s.maxDigestsPerPleasePull {
		out := make([]ifaces.PleasePullOutcome, 0, len(digests))
		for start := 0; start < len(digests); start += s.maxDigestsPerPleasePull {
			end := start + s.maxDigestsPerPleasePull
			if end > len(digests) {
				end = len(digests)
			}

			chunkOut, err := s.StartLocalPull(ctx, registry, repository, kind, digests[start:end])
			if err != nil {
				// Return the outcomes accumulated so far alongside the error.
				// The only error source here is ctx cancellation, and the
				// cold-start caller is per-digest, so partial progress lets
				// already-started digests be observed rather than discarded.
				return out, err
			}

			out = append(out, chunkOut...)
		}

		return out, nil
	}

	out := make([]ifaces.PleasePullOutcome, 0, len(digests))
	for _, d := range digests {
		if err := ctx.Err(); err != nil {
			return out, err
		}

		oc := ifaces.PleasePullOutcome{Digest: d}
		if s.pullerPump == nil {
			out = append(out, oc)
			continue
		}

		res := s.pullerPump(ctx, registry, repository, d, kind)
		switch res.Status {
		case PumpRecentlyFailed:
			oc.Outcome = ifaces.PleasePullRecentlyFailed
			oc.CooldownUntil = res.CooldownUntil
			oc.FailureClass = res.FailureClass
		case PumpAlreadyPulling:
			oc.Outcome = ifaces.PleasePullAlreadyPulling
			oc.StartedAt = res.StartedAt
		case PumpStarted:
			oc.Outcome = ifaces.PleasePullStarted
			oc.StartedAt = res.StartedAt

			if s.hooks.OnPleasePullStarted != nil {
				s.hooks.OnPleasePullStarted()
			}
		case PumpDeclined:
			// Load-shed: the pump refused (at the concurrent-pull ceiling or
			// shutting down). OUTCOME_UNSPECIFIED is overloaded here - it also
			// means "no pump wired" - but the cold-start resolver treats both
			// the same way (give up on this puller for this digest), so the
			// transient-vs-permanent distinction is observable only via the
			// declined counter, not the wire outcome.
			oc.Outcome = ifaces.PleasePullUnspecified

			if s.hooks.OnPleasePullDeclined != nil {
				s.hooks.OnPleasePullDeclined()
			}
		}

		out = append(out, oc)
	}

	return out, nil
}

func (s *Server) servePleasePull(ctx context.Context, _ peer.ID, req *coordv1.PleasePullRequest) (*coordv1.PleasePullResponse, error) {
	// the design doc invariant: one repo per batch. Empty / malformed -> reject.
	if req.GetUpstreamRegistry() == "" || req.GetRepository() == "" {
		return nil, errors.New("please_pull: missing registry/repository")
	}

	if len(req.GetDigests()) == 0 {
		return &coordv1.PleasePullResponse{}, nil
	}

	if s.maxDigestsPerPleasePull > 0 && len(req.GetDigests()) > s.maxDigestsPerPleasePull {
		// Wire path rejects rather than chunks: a well-behaved client (see
		// Client.PleasePull) already splits into <= max batches, so an
		// oversized request on the wire is either a misconfiguration or a
		// client whose max exceeds ours (version skew). Reject loudly instead
		// of silently fanning out work the operator did not size for.
		s.bumpStreamErr()
		return nil, fmt.Errorf("please_pull: too many digests: got %d, max %d", len(req.GetDigests()), s.maxDigestsPerPleasePull)
	}

	results := make([]*coordv1.PleasePullResponse_Result, 0, len(req.GetDigests()))
	for _, raw := range req.GetDigests() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		d, err := digest.Parse(raw)
		if err != nil {
			s.bumpStreamErr()
			s.logger.Debug("please_pull: bad digest",
				slog.String("digest", raw),
				slog.Any("err", err),
			)

			continue
		}

		r := &coordv1.PleasePullResponse_Result{Digest: d.String()}
		if s.pullerPump == nil {
			r.Outcome = coordv1.PleasePullResponse_Result_OUTCOME_UNSPECIFIED
			results = append(results, r)

			continue
		}

		res := s.pullerPump(ctx, req.GetUpstreamRegistry(), req.GetRepository(), d, pleasePullKindFromProto(req.GetKind()))
		switch res.Status {
		case PumpRecentlyFailed:
			r.Outcome = coordv1.PleasePullResponse_Result_OUTCOME_RECENTLY_FAILED
			r.CooldownUntil = timestamppb.New(res.CooldownUntil)
			r.FailureClass = failureClassToProto(res.FailureClass)
		case PumpAlreadyPulling:
			r.Outcome = coordv1.PleasePullResponse_Result_OUTCOME_ALREADY_PULLING
			r.StartedAt = timestamppb.New(res.StartedAt)
		case PumpStarted:
			r.Outcome = coordv1.PleasePullResponse_Result_OUTCOME_STARTED
			r.StartedAt = timestamppb.New(res.StartedAt)

			if s.hooks.OnPleasePullStarted != nil {
				s.hooks.OnPleasePullStarted()
			}
		case PumpDeclined:
			// Load-shed: the pump refused (at the concurrent-pull ceiling or
			// shutting down). OUTCOME_UNSPECIFIED is overloaded here - it also
			// means "no pump wired" - but the cold-start resolver treats both
			// the same way (give up on this puller for this digest), so the
			// transient-vs-permanent distinction is observable only via the
			// declined counter, not the wire outcome.
			r.Outcome = coordv1.PleasePullResponse_Result_OUTCOME_UNSPECIFIED

			if s.hooks.OnPleasePullDeclined != nil {
				s.hooks.OnPleasePullDeclined()
			}
		}

		results = append(results, r)
	}

	return &coordv1.PleasePullResponse{Results: results}, nil
}

func (s *Server) bumpStreamErr() {
	if s.hooks.OnStreamError != nil {
		s.hooks.OnStreamError()
	}
}

// ---------------------------------------------------------------------------
// Client side. Implements ifaces.Coordinator.
// ---------------------------------------------------------------------------

// Client opens a libp2p stream per RPC. Members is used to resolve a
// ifaces.NodeID to a libp2p peer.ID for the dial. ships a
// minimal NodeID->peer.ID mapping that accepts the libp2p peer.ID string
// form directly as the NodeID (matches what `internal/discovery.Host`
// returns from FindProviders). Real K8s-pod-name -> peer.ID mapping is
// owned by `internal/members` and surfaces through a richer Node type
// in +.
type Client struct {
	h                       host.Host
	dialTimeout             time.Duration
	rpcTimeout              time.Duration
	maxDigestsPerPleasePull int
	logger                  *slog.Logger
	resolveMu               sync.RWMutex
	resolveCache            map[ifaces.NodeID]peer.ID
	// resolveFn is consulted before the static cache. It lets higher
	// layers (members) supply a live NodeID -> peer.ID mapping derived
	// from pod-annotation announcements without polling. Returns
	// (peer.ID, true) on hit; (_, false) lets the lookup fall through
	// to resolveCache and finally peer.Decode(string(NodeID)).
	resolveFn func(ifaces.NodeID) (peer.ID, bool)
}

// ClientOption configures a Client.
type ClientOption func(*Client)

// WithDialTimeout overrides the per-RPC dial timeout (default 2s).
func WithDialTimeout(d time.Duration) ClientOption {
	return func(c *Client) {
		if d > 0 {
			c.dialTimeout = d
		}
	}
}

// WithRPCTimeout overrides the per-RPC end-to-end timeout (default 2s).
func WithRPCTimeout(d time.Duration) ClientOption {
	return func(c *Client) {
		if d > 0 {
			c.rpcTimeout = d
		}
	}
}

// WithClientMaxDigestsPerPleasePull overrides the client-side chunk size used
// for PleasePull. Non-positive values are ignored.
func WithClientMaxDigestsPerPleasePull(n int) ClientOption {
	return func(c *Client) {
		if n > 0 {
			c.maxDigestsPerPleasePull = n
		}
	}
}

// WithClientLogger overrides the logger.
func WithClientLogger(l *slog.Logger) ClientOption {
	return func(c *Client) {
		if l != nil {
			c.logger = l.With(slog.String("subsystem", "coord-client"))
		}
	}
}

// WithPeerIDResolver installs a function that maps a NodeID to a
// libp2p peer.ID at dial time. The resolver is consulted before the
// static teach-cache populated by ResolvePeerID; returning (_, false)
// falls through to the cache and then to peer.Decode(NodeID). Used by
// main.go to bridge K8s node names -> libp2p peer.IDs published via
// members' pod-annotation announcements (the design doc).
func WithPeerIDResolver(fn func(ifaces.NodeID) (peer.ID, bool)) ClientOption {
	return func(c *Client) {
		if fn != nil {
			c.resolveFn = fn
		}
	}
}

// NewClient returns a coord RPC client. h must be a running libp2p
// host already participating in the coord protocol's transports.
func NewClient(h host.Host, opts ...ClientOption) *Client {
	c := &Client{
		h:                       h,
		dialTimeout:             2 * time.Second,
		rpcTimeout:              2 * time.Second,
		maxDigestsPerPleasePull: DefaultMaxDigestsPerPleasePull,
		logger:                  slog.Default().With(slog.String("subsystem", "coord-client")),
		resolveCache:            map[ifaces.NodeID]peer.ID{},
	}
	for _, opt := range opts {
		opt(c)
	}

	return c
}

// PullIntentQuery implements ifaces.Coordinator.
func (c *Client) PullIntentQuery(ctx context.Context, target ifaces.NodeID, d digest.Digest) (ifaces.PullIntent, error) {
	in := &coordv1.Envelope{Msg: &coordv1.Envelope_PullIntentRequest{
		PullIntentRequest: &coordv1.PullIntentRequest{Digest: d.String()},
	}}

	out, err := c.roundTrip(ctx, target, in)
	if err != nil {
		return ifaces.PullIntent{}, err
	}

	resp := out.GetPullIntentResponse()
	if resp == nil {
		return ifaces.PullIntent{}, errors.New("coord: empty pull_intent_response")
	}

	return ifaces.PullIntent{
		HasCached:      resp.GetHasCached(),
		InFlight:       resp.GetInFlight(),
		StartedAt:      resp.GetStartedAt().AsTime(),
		RecipientRank:  resp.GetHrwRank(),
		RecentlyFailed: resp.GetRecentlyFailed(),
		CooldownUntil:  resp.GetCooldownUntil().AsTime(),
		FailureClass:   failureClassFromProto(resp.GetFailureClass()),
	}, nil
}

// PleasePull implements ifaces.Coordinator.
func (c *Client) PleasePull(ctx context.Context, target ifaces.NodeID, registry, repository string, kind ifaces.OriginRefKind, digests []digest.Digest) ([]ifaces.PleasePullOutcome, error) {
	maxDigests := c.maxDigestsPerPleasePull
	if maxDigests <= 0 {
		maxDigests = DefaultMaxDigestsPerPleasePull
	}

	if len(digests) > maxDigests {
		outs := make([]ifaces.PleasePullOutcome, 0, len(digests))
		for start := 0; start < len(digests); start += maxDigests {
			end := start + maxDigests
			if end > len(digests) {
				end = len(digests)
			}

			chunkOut, err := c.PleasePull(ctx, target, registry, repository, kind, digests[start:end])
			if err != nil {
				// Unlike the local StartLocalPull path, a failed chunk here is
				// an RPC-level failure (a partial response from one chunk is
				// not trustworthy), so discard partial results and surface the
				// error; the caller retries the whole request.
				return nil, err
			}

			outs = append(outs, chunkOut...)
		}

		return outs, nil
	}

	raws := make([]string, len(digests))
	for i, d := range digests {
		raws[i] = d.String()
	}

	in := &coordv1.Envelope{Msg: &coordv1.Envelope_PleasePullRequest{
		PleasePullRequest: &coordv1.PleasePullRequest{
			Digests:          raws,
			UpstreamRegistry: registry,
			Repository:       repository,
			Kind:             pleasePullKindToProto(kind),
		},
	}}

	out, err := c.roundTrip(ctx, target, in)
	if err != nil {
		return nil, err
	}

	resp := out.GetPleasePullResponse()
	if resp == nil {
		return nil, errors.New("coord: empty please_pull_response")
	}

	outs := make([]ifaces.PleasePullOutcome, 0, len(resp.GetResults()))
	for _, r := range resp.GetResults() {
		d, err := digest.Parse(r.GetDigest())
		if err != nil {
			c.logger.Debug("please_pull: bad result digest", slog.String("digest", r.GetDigest()), slog.Any("err", err))
			continue
		}

		outs = append(outs, ifaces.PleasePullOutcome{
			Digest:        d,
			Outcome:       pleasePullStatusFromProto(r.GetOutcome()),
			StartedAt:     r.GetStartedAt().AsTime(),
			CooldownUntil: r.GetCooldownUntil().AsTime(),
			FailureClass:  failureClassFromProto(r.GetFailureClass()),
		})
	}

	return outs, nil
}

// ResolvePeerID lets external wiring teach the client how to map a
// NodeID to a libp2p peer.ID. Higher layers (members, discovery) own
// this mapping; we just cache lookups.
func (c *Client) ResolvePeerID(id ifaces.NodeID, pid peer.ID) {
	c.resolveMu.Lock()
	c.resolveCache[id] = pid
	c.resolveMu.Unlock()
}

func (c *Client) lookupPeerID(id ifaces.NodeID) (peer.ID, error) {
	if c.resolveFn != nil {
		if pid, ok := c.resolveFn(id); ok {
			return pid, nil
		}
	}

	c.resolveMu.RLock()

	if pid, ok := c.resolveCache[id]; ok {
		c.resolveMu.RUnlock()
		return pid, nil
	}

	c.resolveMu.RUnlock()
	// Fallback: treat NodeID as a peer.ID string. internal/discovery
	// surfaces providers this way already.
	pid, err := peer.Decode(string(id))
	if err != nil {
		return "", fmt.Errorf("coord: cannot resolve NodeID %q to peer.ID: %w", id, err)
	}

	return pid, nil
}

func (c *Client) roundTrip(ctx context.Context, target ifaces.NodeID, env *coordv1.Envelope) (*coordv1.Envelope, error) {
	pid, err := c.lookupPeerID(target)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, c.rpcTimeout)
	defer cancel()

	dialCtx, dialCancel := context.WithTimeout(ctx, c.dialTimeout)

	str, err := c.h.NewStream(dialCtx, pid, ProtocolID)

	dialCancel()

	if err != nil {
		return nil, fmt.Errorf("coord: open stream: %w", err)
	}

	defer func() { _ = str.Close() }() //nolint:errcheck // best-effort close

	if dl, ok := ctx.Deadline(); ok {
		_ = str.SetDeadline(dl) //nolint:errcheck // best-effort deadline
	}

	bytes, err := proto.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("coord: marshal: %w", err)
	}

	w := msgio.NewVarintWriter(str)
	if err := w.WriteMsg(bytes); err != nil {
		return nil, fmt.Errorf("coord: write: %w", err)
	}
	// Signal end of write side so the server can read EOF if it pages.
	_ = str.CloseWrite() //nolint:errcheck // best-effort close

	r := msgio.NewVarintReaderSize(str, MaxMessageBytes)

	rb, err := r.ReadMsg()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("coord: peer closed stream without response")
		}

		return nil, fmt.Errorf("coord: read: %w", err)
	}
	defer r.ReleaseMsg(rb)

	out := &coordv1.Envelope{}
	if err := proto.Unmarshal(rb, out); err != nil {
		return nil, fmt.Errorf("coord: unmarshal: %w", err)
	}

	return out, nil
}

// ---------------------------------------------------------------------------
// proto helpers
// ---------------------------------------------------------------------------

func wrapPullIntentResponse(r *coordv1.PullIntentResponse) *coordv1.Envelope {
	return &coordv1.Envelope{Msg: &coordv1.Envelope_PullIntentResponse{PullIntentResponse: r}}
}

func wrapPleasePullResponse(r *coordv1.PleasePullResponse) *coordv1.Envelope {
	return &coordv1.Envelope{Msg: &coordv1.Envelope_PleasePullResponse{PleasePullResponse: r}}
}

func failureClassToProto(c ifaces.FailureClass) coordv1.FailureClass {
	switch c {
	case ifaces.FailureAuth:
		return coordv1.FailureClass_FAILURE_CLASS_AUTH
	case ifaces.FailureNotFound:
		return coordv1.FailureClass_FAILURE_CLASS_NOT_FOUND
	case ifaces.FailureRateLimited:
		return coordv1.FailureClass_FAILURE_CLASS_RATE_LIMITED
	case ifaces.FailureTransient:
		return coordv1.FailureClass_FAILURE_CLASS_TRANSIENT
	default:
		return coordv1.FailureClass_FAILURE_CLASS_UNSPECIFIED
	}
}

func failureClassFromProto(c coordv1.FailureClass) ifaces.FailureClass {
	switch c {
	case coordv1.FailureClass_FAILURE_CLASS_AUTH:
		return ifaces.FailureAuth
	case coordv1.FailureClass_FAILURE_CLASS_NOT_FOUND:
		return ifaces.FailureNotFound
	case coordv1.FailureClass_FAILURE_CLASS_RATE_LIMITED:
		return ifaces.FailureRateLimited
	case coordv1.FailureClass_FAILURE_CLASS_TRANSIENT:
		return ifaces.FailureTransient
	default:
		return ifaces.FailureUnspecified
	}
}

func pleasePullStatusFromProto(o coordv1.PleasePullResponse_Result_Outcome) ifaces.PleasePullStatus {
	switch o {
	case coordv1.PleasePullResponse_Result_OUTCOME_ALREADY_PULLING:
		return ifaces.PleasePullAlreadyPulling
	case coordv1.PleasePullResponse_Result_OUTCOME_STARTED:
		return ifaces.PleasePullStarted
	case coordv1.PleasePullResponse_Result_OUTCOME_RECENTLY_FAILED:
		return ifaces.PleasePullRecentlyFailed
	default:
		return ifaces.PleasePullUnspecified
	}
}

// pleasePullKindToProto maps the in-process OriginRefKind enum to the
// wire-form Kind enum. Unknown / zero is sent as KIND_UNSPECIFIED so a
// pre-Kind responder still defaults to blob semantics.
//
// KIND_CONFIG is bytes-equivalent to KIND_BLOB at the OCI Distribution
// Spec level (both pull /v2/<repo>/blobs/<digest>) but is preserved on
// the wire so per-kind metrics ("manifest | config | layer") agree
// end-to-end across the please_pull boundary. A pre-KIND_CONFIG peer
// receiving KIND_CONFIG will treat it as KIND_BLOB (the default branch
// in pleasePullKindFromProto below) - correct bytes, only the metric
// label downgrades on that peer.
func pleasePullKindToProto(k ifaces.OriginRefKind) coordv1.PleasePullRequest_Kind {
	switch k {
	case ifaces.KindManifest:
		return coordv1.PleasePullRequest_KIND_MANIFEST
	case ifaces.KindConfig:
		return coordv1.PleasePullRequest_KIND_CONFIG
	case ifaces.KindBlob:
		return coordv1.PleasePullRequest_KIND_BLOB
	default:
		return coordv1.PleasePullRequest_KIND_UNSPECIFIED
	}
}

// pleasePullKindFromProto maps the wire-form Kind enum back to the
// in-process OriginRefKind. KIND_UNSPECIFIED is treated as KindBlob for
// back-compat with peers that have not been recompiled.
func pleasePullKindFromProto(k coordv1.PleasePullRequest_Kind) ifaces.OriginRefKind {
	switch k {
	case coordv1.PleasePullRequest_KIND_MANIFEST:
		return ifaces.KindManifest
	case coordv1.PleasePullRequest_KIND_CONFIG:
		return ifaces.KindConfig
	default:
		return ifaces.KindBlob
	}
}

// Compile-time conformance.
var (
	_ ifaces.Coordinator = (*Client)(nil)
	_ net.Addr           = (*nopAddr)(nil)
)

type nopAddr struct{}

func (nopAddr) Network() string { return "coord" }
func (nopAddr) String() string  { return "coord" }
