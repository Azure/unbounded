// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package cluster handles peer discovery and rendezvous-hash
// coordinator selection.
//
// Peer discovery: the headless Kubernetes Service backing the Orca
// Deployment publishes Pod IPs in its A-record. We poll DNS at
// cluster.membership_refresh interval (default 5s) and snapshot the
// peer set.
//
// Coordinator selection: rendezvous hashing on (peer_ip, ChunkKey)
// picks one coordinator per chunk across the cluster.
//
// Internal RPC: each replica runs an HTTP/2 client to dial peers'
// internal listeners (mTLS in production, plain in dev). The
// listener side is in the server/internal handler.
//
// # Test seams
//
// Production constructs a DNS-backed PeerSource implicitly from
// cfg.Cluster.Service + net.DefaultResolver. Tests substitute the
// entire mechanism with WithPeerSource (typically a mutable
// StaticPeerSource per replica).
package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/Azure/unbounded/internal/orca/chunk"
	"github.com/Azure/unbounded/internal/orca/config"
)

// Peer represents one replica in the current peer-set snapshot.
//
// In production every Peer has Port == 0 because pod IPs are
// addressed on the same internal-listener port across the
// Deployment. Integration tests with multiple replicas sharing
// 127.0.0.1 set Port to the per-replica OS-assigned port; in that
// mode FillFromPeer dials peer.IP:peer.Port instead of falling back
// to cfg.Cluster.InternalListen's port.
type Peer struct {
	IP   string
	Port int  // 0 = use cfg.Cluster.InternalListen's port (production)
	Self bool // true when this Peer entry represents the local replica
}

// Cluster manages peer discovery, rendezvous hashing, and the
// internal-RPC client.
type Cluster struct {
	cfg config.Cluster

	peers atomic.Pointer[[]Peer]

	httpClient *http.Client
	source     PeerSource

	// consecutiveRefreshErrors counts adjacent failed refresh attempts.
	// Reset on any successful refresh. When the count exceeds
	// maxStalePeerRefreshes the retained-previous fallback gives up
	// and reverts to a self-only peer set.
	consecutiveRefreshErrors atomic.Int64

	cancelFn context.CancelFunc
	done     chan struct{}
}

// maxStalePeerRefreshes is the number of consecutive refresh failures
// after which Cluster.refresh stops retaining the previous peer-set
// snapshot and falls back to [Self]. Bounds how long we route to
// dead peers if peer discovery is permanently broken.
const maxStalePeerRefreshes = 5

// Resolver looks up the host names that back the headless Service.
// Production uses net.DefaultResolver. The interface is exposed so
// the DNS-backed peer source can be tested in isolation; production
// code does not customize it.
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// PeerSource produces the current peer-set snapshot. The DNS-backed
// implementation queries the headless Service's A-record. Tests
// substitute a StaticPeerSource that returns a mutable list of peers
// with explicit Port values (so multiple replicas can share an IP).
//
// Each returned Peer.Self must be authoritatively set by the source
// (the source knows the calling replica's identity at construction
// time, so it is the only place that can stamp Self correctly when
// peers share an IP).
type PeerSource interface {
	Peers(ctx context.Context) ([]Peer, error)
}

// Option configures a Cluster at construction time.
type Option func(*Cluster)

// WithPeerSource replaces the entire peer-discovery mechanism. This
// is the primary test seam; production code constructs the default
// DNS-backed source implicitly from cfg.Cluster.Service.
func WithPeerSource(s PeerSource) Option {
	return func(c *Cluster) { c.source = s }
}

// WithHTTPClient overrides the internal-RPC HTTP client. TEST-ONLY:
// production constructs the default client from cfg via newHTTPClient.
// Used by unit tests that need to inject a client with custom timeouts
// or transport behaviour for deterministic deadline coverage.
func WithHTTPClient(c *http.Client) Option {
	return func(cl *Cluster) { cl.httpClient = c }
}

func newDNSPeerSource(service, selfIP string, resolver Resolver) PeerSource {
	if resolver == nil {
		resolver = net.DefaultResolver
	}

	return &dnsPeerSource{
		service:  service,
		selfIP:   selfIP,
		resolver: resolver,
	}
}

type dnsPeerSource struct {
	service  string
	selfIP   string
	resolver Resolver
}

func (s *dnsPeerSource) Peers(ctx context.Context) ([]Peer, error) {
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	ips, err := s.resolver.LookupHost(rctx, s.service)
	if err != nil {
		return nil, err
	}

	peers := make([]Peer, 0, len(ips))
	for _, ip := range ips {
		peers = append(peers, Peer{IP: ip, Self: ip == s.selfIP})
	}

	return peers, nil
}

// New returns a Cluster and starts the membership-refresh goroutine.
func New(parent context.Context, cfg config.Cluster, opts ...Option) (*Cluster, error) {
	if cfg.Service == "" {
		return nil, fmt.Errorf("cluster: service required (headless Service FQDN)")
	}

	if cfg.SelfPodIP == "" {
		return nil, fmt.Errorf("cluster: self_pod_ip required (set POD_IP env)")
	}

	ctx, cancel := context.WithCancel(parent)
	c := &Cluster{
		cfg:        cfg,
		httpClient: newHTTPClient(cfg),
		source:     newDNSPeerSource(cfg.Service, cfg.SelfPodIP, nil),
		cancelFn:   cancel,
		done:       make(chan struct{}),
	}

	for _, opt := range opts {
		opt(c)
	}
	// Initial refresh; failure is non-fatal (empty peer-set fallback).
	c.refresh(ctx)

	go c.refreshLoop(ctx)

	return c, nil
}

// Close stops the refresh goroutine and waits for it to exit. If ctx
// is canceled before the goroutine exits (e.g. an in-flight DNS
// lookup is taking longer than the caller can tolerate) Close returns
// the context error. The underlying cancellation is always signalled,
// so the goroutine will exit eventually even if the caller stops
// waiting.
func (c *Cluster) Close(ctx context.Context) error {
	c.cancelFn()

	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Peers returns the current peer-set snapshot.
func (c *Cluster) Peers() []Peer {
	p := c.peers.Load()
	if p == nil {
		return []Peer{{IP: c.cfg.SelfPodIP, Self: true}}
	}

	return *p
}

// HasInitialSnapshot reports whether the cluster has loaded at least
// one peer-set snapshot (success or failure path - any value stored
// by refresh counts). Used by the app's /readyz endpoint to gate
// readiness on cluster discovery having completed its initial pass.
// Returns false only during the bootstrap window before refresh
// runs even once.
func (c *Cluster) HasInitialSnapshot() bool {
	return c.peers.Load() != nil
}

// self returns the Peer for this replica.
func (c *Cluster) self() Peer {
	return Peer{IP: c.cfg.SelfPodIP, Self: true}
}

// Coordinator selects the rendezvous-hashed coordinator for a chunk.
//
// Returns the Peer with the highest hash(peer || chunk_path) score.
// On empty peer set returns self (last-replica-standing fallback).
func (c *Cluster) Coordinator(k chunk.Key) Peer {
	peers := c.Peers()
	if len(peers) == 0 {
		return c.self()
	}

	path := []byte(k.Path())

	var (
		best      Peer
		bestScore uint64
	)

	for i, p := range peers {
		score := rendezvousScore(p, path)
		if i == 0 || score > bestScore {
			bestScore = score
			best = p
		}
	}

	return best
}

// IsCoordinator reports whether this replica is the coordinator for k.
// Every code path producing a coord value stamps the Self flag
// authoritatively (dnsPeerSource matches by selfIP; StaticPeerSource
// by (selfIP, selfPort); the empty-peer-set fallback constructs
// c.self()), so checking Self is the single source of truth.
func (c *Cluster) IsCoordinator(k chunk.Key) bool {
	return c.Coordinator(k).Self
}

// FillFromPeer issues GET /internal/fill against the named peer and
// returns the streaming chunk body. Caller closes the returned
// reader. objectSize is the authoritative size of the object the
// chunk belongs to; it is forwarded to the peer so the leader can
// compute the correct per-chunk length (especially for the tail
// chunk) and set Content-Length on its response.
func (c *Cluster) FillFromPeer(ctx context.Context, p Peer, k chunk.Key, objectSize int64) (io.ReadCloser, error) {
	if p.Self {
		return nil, fmt.Errorf("cluster: refusing to FillFromPeer for self")
	}

	scheme := "http"
	if c.cfg.InternalTLS.Enabled {
		scheme = "https"
	}

	port := strconv.Itoa(p.Port)
	if p.Port == 0 {
		_, defaultPort, err := net.SplitHostPort(c.cfg.InternalListen)
		if err != nil {
			defaultPort = "8444"
		}

		port = defaultPort
	}

	target := url.URL{
		Scheme:   scheme,
		Host:     net.JoinHostPort(p.IP, port),
		Path:     "/internal/fill",
		RawQuery: encodeChunkKey(k, objectSize),
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("cluster: build internal-fill request: %w", err)
	}

	req.Header.Set("X-Orca-Internal", "1")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cluster: internal-fill RPC: %w", err)
	}

	if resp.StatusCode == http.StatusConflict {
		_ = resp.Body.Close() //nolint:errcheck // best-effort close on error path
		return nil, ErrPeerNotCoordinator
	}

	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024)) //nolint:errcheck // best-effort error body read
		_ = resp.Body.Close()                                  //nolint:errcheck // best-effort close on error path

		return nil, fmt.Errorf("cluster: internal-fill RPC returned %d: %s",
			resp.StatusCode, string(body))
	}

	// Wrap the response body in a defense-in-depth validator that
	// ensures the peer delivered exactly Content-Length bytes.
	// net/http already raises io.ErrUnexpectedEOF when the body
	// closes short of an explicit Content-Length, but the wrapper
	// makes that contract explicit at the call site (so readers of
	// FillFromPeer do not need to reason about transport internals)
	// and guards against future changes to net/http's behavior.
	if resp.ContentLength > 0 {
		return &validatingReader{
			rc:       resp.Body,
			expected: resp.ContentLength,
		}, nil
	}

	return resp.Body, nil
}

// validatingReader wraps an io.ReadCloser and returns
// io.ErrUnexpectedEOF if the underlying stream closes after fewer
// than expected bytes. Used by FillFromPeer to detect truncated
// cross-replica internal-fill responses.
type validatingReader struct {
	rc       io.ReadCloser
	expected int64
	got      int64
}

func (r *validatingReader) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	r.got += int64(n)

	if errors.Is(err, io.EOF) && r.got != r.expected {
		return n, fmt.Errorf("cluster: internal-fill truncated: got %d bytes, expected %d: %w",
			r.got, r.expected, io.ErrUnexpectedEOF)
	}

	return n, err
}

func (r *validatingReader) Close() error { return r.rc.Close() }

// ErrPeerNotCoordinator is returned by FillFromPeer when the peer
// reports it is not the coordinator (membership disagreement).
var ErrPeerNotCoordinator = fmt.Errorf("cluster: peer is not the coordinator (409 Conflict)")

func (c *Cluster) refreshLoop(ctx context.Context) {
	defer close(c.done)

	t := time.NewTicker(c.cfg.MembershipRefresh)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.refresh(ctx)
		}
	}
}

func (c *Cluster) refresh(ctx context.Context) {
	peers, err := c.source.Peers(ctx)
	if err != nil {
		// Discovery failed. Retain the previous snapshot if we have
		// one and we have not exceeded the staleness ceiling; the
		// internal-fill RPC fallback (cluster.ErrPeerNotCoordinator
		// -> local fill in fetch.Coordinator.GetChunk) absorbs
		// pointing at briefly-stale peers. On bootstrap (no previous
		// snapshot) or after too many consecutive errors, fall back
		// to a self-only peer set so we keep making forward progress.
		streak := c.consecutiveRefreshErrors.Add(1)

		if c.peers.Load() != nil && streak <= maxStalePeerRefreshes {
			slog.Default().Warn("cluster: peer discovery failed; retaining previous snapshot",
				"err", err, "consecutive_errors", streak)

			return
		}

		self := []Peer{{IP: c.cfg.SelfPodIP, Self: true}}
		c.peers.Store(&self)

		return
	}

	c.consecutiveRefreshErrors.Store(0)

	if len(peers) == 0 {
		// DNS legitimately reports no peers (e.g. headless Service
		// has no Ready pods other than maybe self). Apply self-only
		// fallback.
		self := []Peer{{IP: c.cfg.SelfPodIP, Self: true}}
		c.peers.Store(&self)

		return
	}
	// Ensure self is always in the set even if discovery hasn't
	// caught up yet.
	hasSelf := false

	for _, p := range peers {
		if p.Self {
			hasSelf = true
			break
		}
	}

	if !hasSelf {
		peers = append(peers, Peer{IP: c.cfg.SelfPodIP, Self: true})
	}

	c.peers.Store(&peers)
}

func newHTTPClient(cfg config.Cluster) *http.Client {
	tr := &http.Transport{
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     30 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	// TLS configuration deliberately omitted for prototype dev mode
	// (cluster.internal_tls.enabled=false). Production will populate
	// tr.TLSClientConfig from cfg.InternalTLS.
	_ = cfg

	// No http.Client.Timeout: it is the request-total wall clock and
	// would clamp long-running internal-fill body streams (an 8 MiB
	// chunk on a degraded inter-pod link can exceed 60s). The caller's
	// ctx (an edge request ctx for client-driven fills, the 5-minute
	// detached fill ctx in fetch.runFill for leader-side ones) is the
	// sole deadline.
	return &http.Client{
		Transport: tr,
	}
}

// Score returns the rendezvous-hash score for (peer, key). Exposed so
// integration tests can craft phantom peers that deterministically
// win or lose against a real peer for a given key (used to induce
// membership disagreement scenarios).
func Score(p Peer, key []byte) uint64 {
	return rendezvousScore(p, key)
}

func rendezvousScore(p Peer, key []byte) uint64 {
	h := sha256.New()
	h.Write([]byte(p.IP))
	h.Write([]byte{0})

	if p.Port != 0 {
		// In production every peer has Port=0 so this branch never
		// fires and the score is identical to historical behavior
		// (sha256(ip || 0 || key)). Tests with multiple peers sharing
		// 127.0.0.1 set distinct Ports so the score differentiates
		// replicas.
		var pb [4]byte
		binary.BigEndian.PutUint32(pb[:], uint32(p.Port))
		h.Write(pb[:])
		h.Write([]byte{0})
	}

	h.Write(key)
	sum := h.Sum(nil)

	return binary.BigEndian.Uint64(sum[:8])
}

func encodeChunkKey(k chunk.Key, objectSize int64) string {
	v := url.Values{}
	v.Set("origin_id", k.OriginID)
	v.Set("bucket", k.Bucket)
	v.Set("key", k.ObjectKey)
	v.Set("etag", k.ETag)
	v.Set("chunk_size", strconv.FormatInt(k.ChunkSize, 10))
	v.Set("index", strconv.FormatInt(k.Index, 10))
	v.Set("object_size", strconv.FormatInt(objectSize, 10))

	return v.Encode()
}

// DecodeChunkKey parses query params into a Key plus the authoritative
// object size. Used by the internal listener (server/internal/fill).
func DecodeChunkKey(values url.Values) (chunk.Key, int64, error) {
	chunkSize, err := strconv.ParseInt(values.Get("chunk_size"), 10, 64)
	if err != nil {
		return chunk.Key{}, 0, fmt.Errorf("invalid chunk_size: %w", err)
	}

	if chunkSize <= 0 {
		return chunk.Key{}, 0, fmt.Errorf("invalid chunk_size: must be > 0, got %d", chunkSize)
	}

	idx, err := strconv.ParseInt(values.Get("index"), 10, 64)
	if err != nil {
		return chunk.Key{}, 0, fmt.Errorf("invalid index: %w", err)
	}

	if idx < 0 {
		return chunk.Key{}, 0, fmt.Errorf("invalid index: must be >= 0, got %d", idx)
	}

	objectSize, err := strconv.ParseInt(values.Get("object_size"), 10, 64)
	if err != nil {
		return chunk.Key{}, 0, fmt.Errorf("invalid object_size: %w", err)
	}

	if objectSize < 0 {
		return chunk.Key{}, 0, fmt.Errorf("invalid object_size: must be >= 0, got %d", objectSize)
	}

	originID := values.Get("origin_id")
	bucket := values.Get("bucket")
	key := values.Get("key")
	etag := values.Get("etag")

	if originID == "" || key == "" {
		return chunk.Key{}, 0, fmt.Errorf("missing required key fields")
	}

	return chunk.Key{
		OriginID:  originID,
		Bucket:    bucket,
		ObjectKey: key,
		ETag:      etag,
		ChunkSize: chunkSize,
		Index:     idx,
	}, objectSize, nil
}
