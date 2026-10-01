// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package coord implements HTTPS chair content requests and libp2p chair offers.
// Content is never requested over a libp2p coordination stream.
package coord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-msgio"
	"google.golang.org/protobuf/proto"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/oci"
	coordv1 "github.com/Azure/unbounded/internal/gantry/proto/coord/v1"
)

const (
	ProtocolID                     protocol.ID = "/gantry/coord/1.1.0"
	MaxMessageBytes                            = 1 << 20
	DefaultStreamHandshakeTimeout              = 5 * time.Second
	DefaultMaxConcurrentStreams                = 512
	DefaultMaxDigestsPerPleasePull             = 256
)

type MetricsHooks struct {
	OnPleasePullServed   func()
	OnPleasePullStarted  func()
	OnPleasePullDeclined func(reason string)
	OnStreamError        func()
}

type Server struct {
	logger                  *slog.Logger
	hooks                   MetricsHooks
	pullerPump              PullerPump
	streamHandshakeTimeout  time.Duration
	streamSem               chan struct{}
	maxDigestsPerPleasePull int
	chairValidator          ChairValidator
	chairSuccessor          ifaces.ChairSuccessor
}

type ChairValidator interface {
	ValidateChair(context.Context, ifaces.ChairAssignment) bool
}

// PullerPump admits detached background work and must return promptly.
type PullerPump func(context.Context, string, string, digest.Digest, ifaces.OriginRefKind) PumpResult

type PumpStatus int

const (
	PumpDeclined PumpStatus = iota
	PumpStarted
	PumpAlreadyPulling
	PumpRecentlyFailed
)

type PumpResult struct {
	Status        PumpStatus
	StartedAt     time.Time
	CooldownUntil time.Time
	FailureClass  ifaces.FailureClass
	DeclineReason string
}

const (
	DeclineReasonUnspecified   = "unspecified"
	DeclineReasonGateClosed    = "gate_closed"
	DeclineReasonRequestGone   = "request_gone"
	DeclineReasonAdmissionFull = "admission_full"
)

func declineReason(res PumpResult) string {
	if res.DeclineReason == "" {
		return DeclineReasonUnspecified
	}

	return res.DeclineReason
}

type Option func(*Server)

func WithLogger(l *slog.Logger) Option {
	return func(s *Server) {
		if l != nil {
			s.logger = l.With(slog.String("subsystem", "coord"))
		}
	}
}
func WithMetrics(h MetricsHooks) Option  { return func(s *Server) { s.hooks = h } }
func WithPullerPump(p PullerPump) Option { return func(s *Server) { s.pullerPump = p } }
func WithStreamHandshakeTimeout(d time.Duration) Option {
	return func(s *Server) {
		if d > 0 {
			s.streamHandshakeTimeout = d
		}
	}
}

func WithMaxConcurrentStreams(n int) Option {
	return func(s *Server) {
		if n > 0 {
			s.streamSem = make(chan struct{}, n)
		}
	}
}

func WithMaxDigestsPerPleasePull(n int) Option {
	return func(s *Server) {
		if n > 0 {
			s.maxDigestsPerPleasePull = n
		}
	}
}
func WithChairValidator(v ChairValidator) Option { return func(s *Server) { s.chairValidator = v } }
func WithChairSuccessor(v ifaces.ChairSuccessor) Option {
	return func(s *Server) { s.chairSuccessor = v }
}

func NewServer(opts ...Option) *Server {
	s := &Server{
		logger:                  slog.Default().With(slog.String("subsystem", "coord")),
		streamHandshakeTimeout:  DefaultStreamHandshakeTimeout,
		streamSem:               make(chan struct{}, DefaultMaxConcurrentStreams),
		maxDigestsPerPleasePull: DefaultMaxDigestsPerPleasePull,
	}
	for _, opt := range opts {
		opt(s)
	}

	return s
}

func (s *Server) Bind(h host.Host)   { h.SetStreamHandler(ProtocolID, s.handleStream) }
func (s *Server) Unbind(h host.Host) { h.RemoveStreamHandler(ProtocolID) }

func (s *Server) handleStream(str network.Stream) {
	select {
	case s.streamSem <- struct{}{}:
		defer func() { <-s.streamSem }()
	default:
		s.bumpStreamErr()

		_ = str.Reset() //nolint:errcheck // best-effort rejection

		return
	}

	defer func() { _ = str.Close() }() //nolint:errcheck // best-effort close

	if err := str.SetDeadline(time.Now().Add(s.streamHandshakeTimeout)); err != nil {
		s.bumpStreamErr()
		return
	}

	r := msgio.NewVarintReaderSize(str, MaxMessageBytes)

	data, err := r.ReadMsg()
	if err != nil {
		s.bumpStreamErr()
		return
	}
	defer r.ReleaseMsg(data)

	in := &coordv1.Envelope{}
	if err := proto.Unmarshal(data, in); err != nil {
		s.bumpStreamErr()
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	out, err := s.dispatch(ctx, str.Conn().RemotePeer(), in)
	if err != nil {
		s.bumpStreamErr()
		return
	}

	data, err = proto.Marshal(out)
	if err != nil {
		s.bumpStreamErr()
		return
	}

	if err := msgio.NewVarintWriter(str).WriteMsg(data); err != nil {
		s.bumpStreamErr()
	}
}

func (s *Server) dispatch(ctx context.Context, remote peer.ID, in *coordv1.Envelope) (*coordv1.Envelope, error) {
	switch m := in.GetMsg().(type) {
	case *coordv1.Envelope_ChairOfferRequest:
		resp := &coordv1.ChairOfferResponse{}

		if s.chairSuccessor != nil && m.ChairOfferRequest.GetAssignment() != nil {
			endpoint, accepted := s.chairSuccessor.AcceptChair(ctx, ifaces.PeerID(remote.String()), chairAssignmentFromProto(m.ChairOfferRequest.GetAssignment()))

			resp.Accepted = accepted
			if accepted {
				resp.PeerId = string(endpoint.PeerID)
				resp.P2PAddrs = endpoint.P2PAddrs
				resp.TransferAddr = endpoint.TransferAddr
			}
		}

		return &coordv1.Envelope{Msg: &coordv1.Envelope_ChairOfferResponse{ChairOfferResponse: resp}}, nil
	default:
		return nil, errors.New("coord: expected chair offer")
	}
}

func (s *Server) bumpStreamErr() {
	if s.hooks.OnStreamError != nil {
		s.hooks.OnStreamError()
	}
}

// StartLocalPull admits local fallback work without requiring a chair lease.
func (s *Server) StartLocalPull(ctx context.Context, registry, repository string, kind ifaces.OriginRefKind, digests []digest.Digest) ([]ifaces.PleasePullOutcome, error) {
	return s.startLocalPull(ctx, registry, repository, kind, digests, nil)
}

func (s *Server) StartLocalChairPull(ctx context.Context, registry, repository string, kind ifaces.OriginRefKind, digests []digest.Digest, assignment ifaces.ChairAssignment) ([]ifaces.PleasePullOutcome, error) {
	return s.startLocalPull(ctx, registry, repository, kind, digests, &assignment)
}

func (s *Server) startLocalPull(ctx context.Context, registry, repository string, kind ifaces.OriginRefKind, digests []digest.Digest, assignment *ifaces.ChairAssignment) ([]ifaces.PleasePullOutcome, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if kind != ifaces.KindBlob && kind != ifaces.KindManifest && kind != ifaces.KindConfig {
		return nil, errors.New("start_local_pull: invalid kind")
	}

	if registry == "" || repository == "" {
		return nil, errors.New("start_local_pull: missing registry/repository")
	}

	if err := oci.ValidateRepositoryName(repository); err != nil {
		return nil, fmt.Errorf("start_local_pull: %w", err)
	}

	if assignment != nil && !s.validChair(ctx, *assignment) {
		return staleChairOutcomes(digests), nil
	}

	if s.maxDigestsPerPleasePull > 0 && len(digests) > s.maxDigestsPerPleasePull {
		out := make([]ifaces.PleasePullOutcome, 0, len(digests))
		for start := 0; start < len(digests); start += s.maxDigestsPerPleasePull {
			end := min(start+s.maxDigestsPerPleasePull, len(digests))

			chunk, err := s.startLocalPull(ctx, registry, repository, kind, digests[start:end], assignment)
			if err != nil {
				return out, err
			}

			out = append(out, chunk...)
		}

		return out, nil
	}

	if s.hooks.OnPleasePullServed != nil {
		s.hooks.OnPleasePullServed()
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
			oc.Outcome, oc.CooldownUntil, oc.FailureClass = ifaces.PleasePullRecentlyFailed, res.CooldownUntil, res.FailureClass
		case PumpAlreadyPulling:
			oc.Outcome, oc.StartedAt = ifaces.PleasePullAlreadyPulling, res.StartedAt
		case PumpStarted:
			oc.Outcome, oc.StartedAt = ifaces.PleasePullStarted, res.StartedAt

			if s.hooks.OnPleasePullStarted != nil {
				s.hooks.OnPleasePullStarted()
			}
		case PumpDeclined:
			if s.hooks.OnPleasePullDeclined != nil {
				s.hooks.OnPleasePullDeclined(declineReason(res))
			}
		}

		out = append(out, oc)
	}

	return out, nil
}

// Client sends chair rotation offers over libp2p. Targets are peer identities,
// never Kubernetes node names or membership aliases.
type (
	Client struct {
		h                       host.Host
		dialTimeout, rpcTimeout time.Duration
	}
	ClientOption func(*Client)
)

func WithDialTimeout(d time.Duration) ClientOption {
	return func(c *Client) {
		if d > 0 {
			c.dialTimeout = d
		}
	}
}

func WithRPCTimeout(d time.Duration) ClientOption {
	return func(c *Client) {
		if d > 0 {
			c.rpcTimeout = d
		}
	}
}

func NewClient(h host.Host, opts ...ClientOption) *Client {
	c := &Client{h: h, dialTimeout: 2 * time.Second, rpcTimeout: 2 * time.Second}
	for _, opt := range opts {
		opt(c)
	}

	return c
}

func (c *Client) OfferChair(ctx context.Context, target ifaces.PeerID, assignment ifaces.ChairAssignment) (ifaces.PeerEndpoint, bool, error) {
	in := &coordv1.Envelope{Msg: &coordv1.Envelope_ChairOfferRequest{ChairOfferRequest: &coordv1.ChairOfferRequest{Assignment: chairAssignmentToProto(assignment)}}}

	out, err := c.roundTrip(ctx, target, in)
	if err != nil {
		return ifaces.PeerEndpoint{}, false, err
	}

	response := out.GetChairOfferResponse()
	if response == nil {
		return ifaces.PeerEndpoint{}, false, errors.New("coord: missing chair offer response")
	}

	if !response.GetAccepted() {
		return ifaces.PeerEndpoint{}, false, nil
	}

	return ifaces.PeerEndpoint{PeerID: ifaces.PeerID(response.GetPeerId()), P2PAddrs: append([]string(nil), response.GetP2PAddrs()...), TransferAddr: response.GetTransferAddr()}, true, nil
}

func (c *Client) roundTrip(ctx context.Context, target ifaces.PeerID, env *coordv1.Envelope) (*coordv1.Envelope, error) {
	pid, err := peer.Decode(string(target))
	if err != nil {
		return nil, fmt.Errorf("coord: invalid peer ID: %w", err)
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

	stop := context.AfterFunc(ctx, func() { _ = str.Reset() }) //nolint:errcheck // cancellation interrupts I/O
	defer stop()

	if dl, ok := ctx.Deadline(); ok {
		if err := str.SetDeadline(dl); err != nil {
			return nil, err
		}
	}

	data, err := proto.Marshal(env)
	if err != nil {
		return nil, err
	}

	if err := msgio.NewVarintWriter(str).WriteMsg(data); err != nil {
		return nil, err
	}

	if err := str.CloseWrite(); err != nil {
		return nil, err
	}

	r := msgio.NewVarintReaderSize(str, MaxMessageBytes)

	data, err = r.ReadMsg()
	if err != nil {
		return nil, fmt.Errorf("coord: read: %w", err)
	}
	defer r.ReleaseMsg(data)

	out := &coordv1.Envelope{}
	if err := proto.Unmarshal(data, out); err != nil {
		return nil, err
	}

	return out, nil
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

func pleasePullStatusFromProto(s coordv1.PleasePullResponse_Result_Outcome) ifaces.PleasePullStatus {
	switch s {
	case coordv1.PleasePullResponse_Result_OUTCOME_STARTED:
		return ifaces.PleasePullStarted
	case coordv1.PleasePullResponse_Result_OUTCOME_ALREADY_PULLING:
		return ifaces.PleasePullAlreadyPulling
	case coordv1.PleasePullResponse_Result_OUTCOME_RECENTLY_FAILED:
		return ifaces.PleasePullRecentlyFailed
	case coordv1.PleasePullResponse_Result_OUTCOME_STALE_CHAIR:
		return ifaces.PleasePullStaleChair
	default:
		return ifaces.PleasePullUnspecified
	}
}

func (s *Server) validChair(ctx context.Context, a ifaces.ChairAssignment) bool {
	return s.chairValidator != nil && s.chairValidator.ValidateChair(ctx, a)
}

func staleChairOutcomes(digests []digest.Digest) []ifaces.PleasePullOutcome {
	out := make([]ifaces.PleasePullOutcome, 0, len(digests))
	for _, d := range digests {
		out = append(out, ifaces.PleasePullOutcome{Digest: d, Outcome: ifaces.PleasePullStaleChair})
	}

	return out
}

func chairAssignmentToProto(a ifaces.ChairAssignment) *coordv1.ChairAssignment {
	return &coordv1.ChairAssignment{ChairId: a.ChairID, Generation: a.Generation, AssignmentEpoch: a.AssignmentEpoch}
}

func chairAssignmentFromProto(a *coordv1.ChairAssignment) ifaces.ChairAssignment {
	return ifaces.ChairAssignment{ChairID: a.GetChairId(), Generation: a.GetGeneration(), AssignmentEpoch: a.GetAssignmentEpoch()}
}

func pleasePullKindToProto(k ifaces.OriginRefKind) coordv1.PleasePullRequest_Kind {
	switch k {
	case ifaces.KindBlob:
		return coordv1.PleasePullRequest_KIND_BLOB
	case ifaces.KindManifest:
		return coordv1.PleasePullRequest_KIND_MANIFEST
	case ifaces.KindConfig:
		return coordv1.PleasePullRequest_KIND_CONFIG
	default:
		return coordv1.PleasePullRequest_KIND_UNSPECIFIED
	}
}

func pleasePullKindFromProto(k coordv1.PleasePullRequest_Kind) (ifaces.OriginRefKind, error) {
	switch k {
	case coordv1.PleasePullRequest_KIND_BLOB:
		return ifaces.KindBlob, nil
	case coordv1.PleasePullRequest_KIND_MANIFEST:
		return ifaces.KindManifest, nil
	case coordv1.PleasePullRequest_KIND_CONFIG:
		return ifaces.KindConfig, nil
	default:
		return 0, errors.New("please_pull: missing or unknown kind")
	}
}

var _ ifaces.ChairRotationCoordinator = (*Client)(nil)
