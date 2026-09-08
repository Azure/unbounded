// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package coord

// HTTPS transport for the cold-start please_pull RPC.
//
// please_pull used to travel over a libp2p stream, which put it on the same
// connection budget as DHT traffic. The DHT opens a connection to every peer
// it queries and nothing reclaims them, so the swarm tends toward a full mesh
// and the connection manager trims whatever looks idle - including chair
// connections about to be reused. An evicted chair connection re-dials into
// libp2p's per-peer backoff, the resolver records the chair as failed and
// recruits a replacement, and every replacement is another agent pulling the
// same layer from the origin registry.
//
// Serving please_pull on its own HTTPS listener puts it on a separate
// connection pool that the libp2p connection and resource managers do not
// govern, so DHT watermarks can be sized for a large cluster without evicting
// chair connections.

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Azure/unbounded/internal/gantry/chaircall"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	coordv1 "github.com/Azure/unbounded/internal/gantry/proto/coord/v1"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
)

// ChairHTTPPath is the request path for the please_pull RPC.
const ChairHTTPPath = "/gantry/v1/please-pull"

// maxChairRequestBytes bounds a request body. A batched please_pull is a few
// hundred bytes per digest; this leaves room while keeping a hostile client
// from forcing an allocation.
const maxChairRequestBytes = 1 << 20

// chairHostSuffix makes the peer ID part of the URL host so Go's connection
// pool keys on it. Without this, connections are pooled by address alone and a
// connection verified against one identity could be reused after a chair
// rotates, silently bypassing the pin.
const chairHostSuffix = ".chair.invalid"

// NewChairHTTPHandler serves please_pull over HTTP for the given local puller.
func NewChairHTTPHandler(local ifaces.LocalChairPullStarter, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}

	mux := http.NewServeMux()
	mux.HandleFunc(ChairHTTPPath, func(w http.ResponseWriter, r *http.Request) {
		serveChairHTTP(w, r, local, logger)
	})

	return mux
}

func serveChairHTTP(w http.ResponseWriter, r *http.Request, local ifaces.LocalChairPullStarter, logger *slog.Logger) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxChairRequestBytes))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)

		return
	}

	var req coordv1.PleasePullRequest
	if err := proto.Unmarshal(body, &req); err != nil {
		http.Error(w, "malformed request", http.StatusBadRequest)

		return
	}

	digests := make([]digest.Digest, 0, len(req.GetDigests()))

	for _, raw := range req.GetDigests() {
		d, perr := digest.Parse(raw)
		if perr != nil {
			http.Error(w, "malformed digest", http.StatusBadRequest)

			return
		}

		digests = append(digests, d)
	}

	assignment := chairAssignmentFromProto(req.GetChairAssignment())

	// The delegated credential rides the request context exactly as the libp2p
	// path does; it is never logged or persisted.
	ctx := registryauth.WithAuthorization(r.Context(), req.GetAuthorization())

	outcomes, err := local.StartLocalChairPull(ctx,
		req.GetUpstreamRegistry(), req.GetRepository(),
		pleasePullKindFromProto(req.GetKind()), digests, assignment)
	if err != nil {
		logger.Debug("chaircall: local pull failed", slog.Any("err", err))
		http.Error(w, "pull failed", http.StatusServiceUnavailable)

		return
	}

	resp := &coordv1.PleasePullResponse{Results: make([]*coordv1.PleasePullResponse_Result, 0, len(outcomes))}
	for _, oc := range outcomes {
		resp.Results = append(resp.Results, pleasePullResultToProto(oc))
	}

	out, err := proto.Marshal(resp)
	if err != nil {
		http.Error(w, "marshal response", http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out) //nolint:errcheck // best-effort write
}

// ChairHTTPClient issues please_pull over HTTPS, pinning each chair's TLS
// certificate to the peer ID published in its Lease.
type ChairHTTPClient struct {
	hc     *http.Client
	port   int
	logger *slog.Logger

	mu    sync.RWMutex
	pins  map[peer.ID]string // peer ID -> dial address
	limit int
}

// ChairHTTPOptions configures a ChairHTTPClient.
type ChairHTTPOptions struct {
	// Port is the chair listener port, shared by convention across the cluster
	// the same way the transfer port is.
	Port int

	// Timeout bounds a single call. Zero uses 5s.
	Timeout time.Duration

	// MaxDigestsPerRequest caps a batch. Zero uses DefaultMaxDigestsPerPleasePull.
	MaxDigestsPerRequest int

	Logger *slog.Logger
}

// NewChairHTTPClient builds a client for the chair please_pull endpoint.
func NewChairHTTPClient(opts ChairHTTPOptions) *ChairHTTPClient {
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Second
	}

	if opts.MaxDigestsPerRequest <= 0 {
		opts.MaxDigestsPerRequest = DefaultMaxDigestsPerPleasePull
	}

	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	c := &ChairHTTPClient{
		port:   opts.Port,
		logger: opts.Logger,
		pins:   map[peer.ID]string{},
		limit:  opts.MaxDigestsPerRequest,
	}

	c.hc = &http.Client{
		Timeout: opts.Timeout,
		Transport: &http.Transport{
			DialTLSContext:      c.dialPinned,
			MaxIdleConns:        256,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     90 * time.Second,
			ForceAttemptHTTP2:   true,
		},
	}

	return c
}

// dialPinned resolves the synthetic <peerID>.chair.invalid host back to the
// chair's real address and verifies the certificate against that peer ID.
func (c *ChairHTTPClient) dialPinned(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("coord: chair address %q: %w", addr, err)
	}

	encoded := strings.TrimSuffix(host, chairHostSuffix)
	if encoded == host {
		return nil, fmt.Errorf("coord: chair host %q is not pinned", host)
	}

	pid, err := peer.Decode(encoded)
	if err != nil {
		return nil, fmt.Errorf("coord: chair peer %q: %w", encoded, err)
	}

	c.mu.RLock()
	target, ok := c.pins[pid]
	c.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("coord: no address registered for chair %s", pid)
	}

	dialer := &net.Dialer{}

	raw, err := dialer.DialContext(ctx, network, target)
	if err != nil {
		return nil, err
	}

	conn := tls.Client(raw, chaircall.ClientTLSConfig(pid))
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = raw.Close() //nolint:errcheck // best-effort close on handshake failure

		return nil, err
	}

	return conn, nil
}

// PleasePullChair implements ifaces.ChairCoordinator over HTTPS.
func (c *ChairHTTPClient) PleasePullChair(ctx context.Context, endpoint ifaces.PeerEndpoint, registry, repository string, kind ifaces.OriginRefKind, digests []digest.Digest, assignment ifaces.ChairAssignment) ([]ifaces.PleasePullOutcome, error) {
	pid, err := peer.Decode(string(endpoint.PeerID))
	if err != nil {
		return nil, fmt.Errorf("coord: chair peer %q: %w", endpoint.PeerID, err)
	}

	target, err := c.chairAddr(endpoint)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.pins[pid] = target
	c.mu.Unlock()

	if len(digests) > c.limit {
		out := make([]ifaces.PleasePullOutcome, 0, len(digests))
		for start := 0; start < len(digests); start += c.limit {
			end := start + c.limit
			if end > len(digests) {
				end = len(digests)
			}

			chunk, cerr := c.PleasePullChair(ctx, endpoint, registry, repository, kind, digests[start:end], assignment)
			if cerr != nil {
				return nil, cerr
			}

			out = append(out, chunk...)
		}

		return out, nil
	}

	raws := make([]string, len(digests))
	for i, d := range digests {
		raws[i] = d.String()
	}

	req := &coordv1.PleasePullRequest{
		Digests:          raws,
		UpstreamRegistry: registry,
		Repository:       repository,
		Kind:             pleasePullKindToProto(kind),
		Authorization:    registryauth.Authorization(ctx),
		ChairAssignment:  chairAssignmentToProto(assignment),
	}

	body, err := proto.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("coord: marshal please_pull: %w", err)
	}

	endpointURL := (&url.URL{
		Scheme: "https",
		Host:   net.JoinHostPort(pid.String()+chairHostSuffix, fmt.Sprint(c.port)),
		Path:   ChairHTTPPath,
	}).String()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("coord: build please_pull request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/x-protobuf")

	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("coord: please_pull: %w", err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("coord: please_pull returned %s", resp.Status)
	}

	payload, err := io.ReadAll(http.MaxBytesReader(nil, resp.Body, maxChairRequestBytes))
	if err != nil {
		return nil, fmt.Errorf("coord: read please_pull response: %w", err)
	}

	var out coordv1.PleasePullResponse
	if err := proto.Unmarshal(payload, &out); err != nil {
		return nil, fmt.Errorf("coord: unmarshal please_pull response: %w", err)
	}

	outcomes := make([]ifaces.PleasePullOutcome, 0, len(out.GetResults()))

	for _, r := range out.GetResults() {
		d, perr := digest.Parse(r.GetDigest())
		if perr != nil {
			c.logger.Debug("chaircall: bad result digest", slog.String("digest", r.GetDigest()), slog.Any("err", perr))

			continue
		}

		outcomes = append(outcomes, ifaces.PleasePullOutcome{
			Digest:        d,
			Outcome:       pleasePullStatusFromProto(r.GetOutcome()),
			StartedAt:     r.GetStartedAt().AsTime(),
			CooldownUntil: r.GetCooldownUntil().AsTime(),
			FailureClass:  failureClassFromProto(r.GetFailureClass()),
		})
	}

	return outcomes, nil
}

// chairAddr derives the chair listener address from the endpoint's transfer
// address. Agents share the chair port by convention, exactly as they share
// the transfer port.
func (c *ChairHTTPClient) chairAddr(endpoint ifaces.PeerEndpoint) (string, error) {
	if endpoint.TransferAddr == "" {
		return "", errors.New("coord: chair endpoint has no transfer address")
	}

	host, _, err := net.SplitHostPort(endpoint.TransferAddr)
	if err != nil {
		return "", fmt.Errorf("coord: chair transfer address %q: %w", endpoint.TransferAddr, err)
	}

	return net.JoinHostPort(host, fmt.Sprint(c.port)), nil
}

func pleasePullResultToProto(oc ifaces.PleasePullOutcome) *coordv1.PleasePullResponse_Result {
	r := &coordv1.PleasePullResponse_Result{
		Digest:  oc.Digest.String(),
		Outcome: pleasePullStatusToProto(oc.Outcome),
	}

	if !oc.StartedAt.IsZero() {
		r.StartedAt = timestamppb.New(oc.StartedAt)
	}

	if !oc.CooldownUntil.IsZero() {
		r.CooldownUntil = timestamppb.New(oc.CooldownUntil)
		r.FailureClass = failureClassToProto(oc.FailureClass)
	}

	return r
}

func pleasePullStatusToProto(s ifaces.PleasePullStatus) coordv1.PleasePullResponse_Result_Outcome {
	switch s {
	case ifaces.PleasePullAlreadyPulling:
		return coordv1.PleasePullResponse_Result_OUTCOME_ALREADY_PULLING
	case ifaces.PleasePullStarted:
		return coordv1.PleasePullResponse_Result_OUTCOME_STARTED
	case ifaces.PleasePullRecentlyFailed:
		return coordv1.PleasePullResponse_Result_OUTCOME_RECENTLY_FAILED
	case ifaces.PleasePullStaleChair:
		return coordv1.PleasePullResponse_Result_OUTCOME_STALE_CHAIR
	default:
		return coordv1.PleasePullResponse_Result_OUTCOME_UNSPECIFIED
	}
}
