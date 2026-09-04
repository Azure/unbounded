// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package discovery wires Gantry's libp2p host and Kademlia DHT.
//
// Design :
//
// - Each agent runs a libp2p host with TCP and (optionally) QUIC transports
// and Noise security. The host's identity is persisted to a hostPath so
// restarts don't churn the DHT routing table.
// - The host participates in a cluster-scoped Kademlia DHT (`/gantry/kad/1`
// protocol prefix) in **server mode**. Server mode is required because
// every Gantry agent is also a content provider; client-mode peers would
// not contribute provider records.
// - Provider records are keyed by a CIDv1 wrapping the OCI digest's raw
// 32 bytes via SHA2-256 multihash. The CID derivation is deterministic
// and documented inline so any agent can re-derive the same CID from
// the same digest.
//
// scope:
//
// - Host + DHT bring-up, Provide / FindProviders.
// - Health returns the geometric-mean health score from
// internal/discovery/health.go (routing-table coverage, p95
// lookup latency, self-test success rate); in test mode where
// no Monitor is wired it returns 1.0.
// - Bootstrap pulls from operator-supplied `Libp2pBootstrapPeers` plus the
// dynamic holder addresses stored in the 64 chair Leases. ConnectPeers dials
// those addresses with the bounded bootstrap cascade.
package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	mathrand "math/rand"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/multiformats/go-multiaddr"
	"github.com/multiformats/go-multihash"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

// Options configures the discovery host.
type Options struct {
	// IdentityPath is the on-disk persistence path for the libp2p key.
	// Empty means generate a fresh ephemeral identity (test mode).
	IdentityPath string

	// ListenAddrs is the list of multiaddrs the host advertises. Empty
	// uses libp2p's defaults (which include /ip4/0.0.0.0/tcp/0).
	ListenAddrs []string

	// BootstrapPeers is the static list of peer multiaddrs to seed the
	// DHT routing table on startup. Each entry must include /p2p/<peer.ID>.
	BootstrapPeers []string

	// ProtocolPrefix is the kad-dht protocol prefix, e.g. "/gantry". Empty
	// uses kad-dht's default ("/ipfs"). Production should set this to
	// isolate the cluster's DHT from other libp2p networks.
	ProtocolPrefix string

	// Logger is the structured logger; nil uses slog.Default.
	Logger *slog.Logger

	// RoutingTableTarget returns the expected steady-state routing-table size.
	// Chair mode derives it from the configured cluster-size estimate, capped
	// by the Kademlia table target. Nil or <=0 disables this health component.
	RoutingTableTarget func() int

	// SelfTestPeriod is the interval between Provide(self_id) ->
	// FindProviders(self_id) self-test cycles. Zero disables the
	// background self-test loop (used in tests). Production default
	// is 60s (the design doc).
	SelfTestPeriod time.Duration

	// TransferPort is the TCP port to suffix onto IP addresses returned
	// by FindProviders. Zero defaults to 5001 (the design-doc value).
	// In a cluster all agents share a transfer port by convention so
	// inferring it from the local config is safe; the value travels
	// through Options so test harnesses and operators that override the
	// port don't get a hardcoded mismatch.
	TransferPort int
}

// DefaultTransferPort is the conventional peer-transfer port used when
// Options.TransferPort is zero. Kept exported so callers building Options
// by hand can spell it explicitly.
const DefaultTransferPort = 5001

// FromConfig builds Options from a *config.Config.
func FromConfig(c *config.Config) Options {
	port := DefaultTransferPort

	if _, p, err := net.SplitHostPort(c.TransferListen); err == nil {
		if n, perr := strconv.Atoi(p); perr == nil && n > 0 {
			port = n
		}
	}

	return Options{
		IdentityPath:   c.Libp2pIdentityPath,
		ListenAddrs:    c.Libp2pListen,
		BootstrapPeers: c.Libp2pBootstrapPeers,
		ProtocolPrefix: "/gantry",
		SelfTestPeriod: 60 * time.Second,
		TransferPort:   port,
	}
}

// Host wraps a libp2p host + kad-dht and implements ifaces.DHT.
type Host struct {
	logger *slog.Logger
	h      host.Host
	d      *dht.IpfsDHT

	// transferPort is the conventional peer-transfer port suffixed to
	// FindProviders results' IP. Captured from Options at New so
	// FindProviders doesn't have to thread Options state.
	transferPort int

	// monitor is the health tracker. Latency samples
	// flow from FindProviders; the self-test loop is owned by
	// New / Close.
	monitor      *Monitor
	selfTestStop context.CancelFunc
	selfTestDone chan struct{}

	closeOnce sync.Once
}

// New builds a Host and joins the DHT in server mode. The returned Host is
// already announcing - Provide/FindProviders are usable immediately, though
// FindProviders may return empty until the routing table converges.
func New(ctx context.Context, opts Options) (*Host, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	logger = logger.With(slog.String("subsystem", "discovery"))

	priv, err := loadOrCreateIdentity(opts.IdentityPath, logger)
	if err != nil {
		return nil, fmt.Errorf("discovery: identity: %w", err)
	}

	var listenOpt libp2p.Option
	if len(opts.ListenAddrs) > 0 {
		listenOpt = libp2p.ListenAddrStrings(opts.ListenAddrs...)
	} else {
		// Sensible defaults: TCP on a random port, plus QUIC.
		listenOpt = libp2p.ListenAddrStrings(
			"/ip4/0.0.0.0/tcp/0",
			"/ip4/0.0.0.0/udp/0/quic-v1",
		)
	}

	h, err := libp2p.New(
		libp2p.Identity(priv),
		listenOpt,
	)
	if err != nil {
		return nil, fmt.Errorf("discovery: libp2p new: %w", err)
	}

	dhtOpts := []dht.Option{dht.Mode(dht.ModeServer)}
	if opts.ProtocolPrefix != "" {
		dhtOpts = append(dhtOpts, dht.ProtocolPrefix(protocol.ID(opts.ProtocolPrefix)))
	}

	d, err := dht.New(h, dhtOpts...)
	if err != nil {
		_ = h.Close() //nolint:errcheck // best-effort close
		return nil, fmt.Errorf("discovery: dht new: %w", err)
	}

	if err := d.Bootstrap(ctx); err != nil {
		// kad-dht bootstrap runs asynchronously; this only checks the
		// initial configuration error path.
		logger.Warn("dht bootstrap kickoff returned err", slog.Any("err", err))
	}

	host := &Host{logger: logger, h: h, d: d}

	host.transferPort = opts.TransferPort
	if host.transferPort == 0 {
		host.transferPort = DefaultTransferPort
	}

	host.monitor = NewMonitor(MonitorOptions{
		RoutingTableSize:   func() int { return d.RoutingTable().Size() },
		RoutingTableTarget: opts.RoutingTableTarget,
	})
	if opts.SelfTestPeriod > 0 {
		stCtx, stCancel := context.WithCancel(context.Background())

		host.selfTestStop = stCancel
		host.selfTestDone = make(chan struct{})

		go func() {
			defer close(host.selfTestDone)

			host.monitor.RunSelfTestLoop(stCtx, opts.SelfTestPeriod, host.runSelfTest)
		}()
	}

	host.dialBootstrap(ctx, opts.BootstrapPeers)

	logger.Info("libp2p host ready",
		slog.String("peer_id", h.ID().String()),
		slog.Int("listen_addrs", len(h.Addrs())),
	)

	return host, nil
}

// Close tears down the DHT and libp2p host. Safe to call multiple times.
func (h *Host) Close() error {
	var err error

	h.closeOnce.Do(func() {
		if h.selfTestStop != nil {
			h.selfTestStop()
		}

		// Wait for the self-test goroutine to finish before
		// closing DHT/host to avoid racing teardown.
		if h.selfTestDone != nil {
			<-h.selfTestDone
		}

		if cerr := h.d.Close(); cerr != nil {
			err = cerr
		}

		if cerr := h.h.Close(); cerr != nil && err == nil {
			err = cerr
		}
	})

	return err
}

// PeerID returns the libp2p peer ID of this host.
func (h *Host) PeerID() peer.ID { return h.h.ID() }

// Addrs returns the libp2p listen multiaddrs (host-local view).
func (h *Host) Addrs() []multiaddr.Multiaddr { return h.h.Addrs() }

// LibP2P returns the underlying libp2p host. Used by the // coord-stream wiring (internal/coord) to attach the gRPC-over-libp2p
// stream handler to the same host that runs the DHT.
func (h *Host) LibP2P() host.Host { return h.h }

// ConnectPeers dials a set of multiaddr strings through the bounded bootstrap
// cascade. Chair snapshots feed holder addresses here so kad-dht has
// direct-connect seeds without operator-supplied bootstrap peers.
//
// Returns the number of peers that successfully connected. Failures are
// logged at DEBUG and do not fail the call.
func (h *Host) ConnectPeers(ctx context.Context, multiaddrs []string) int {
	if len(multiaddrs) == 0 {
		return 0
	}

	pool := make([]peer.AddrInfo, 0, len(multiaddrs))
	positions := make(map[peer.ID]int, len(multiaddrs))

	for _, p := range multiaddrs {
		ai, err := peer.AddrInfoFromString(p)
		if err != nil {
			h.logger.Debug("ConnectPeers: parse failed",
				slog.String("multiaddr", p),
				slog.Any("err", err),
			)

			continue
		}
		// Filter out self - connecting to our own host is a no-op
		// at best and a confused-state log at worst.
		if ai.ID == h.h.ID() {
			continue
		}

		mergePeerAddrInfo(&pool, positions, *ai)
	}

	if len(pool) == 0 {
		return 0
	}

	return h.dialBootstrapPool(ctx, pool)
}

// RoutingTableSize returns the current kad-dht routing-table size.
// Used by readiness probes and the health score.
func (h *Host) RoutingTableSize() int { return h.d.RoutingTable().Size() }

// Provide implements ifaces.DHT.
func (h *Host) Provide(ctx context.Context, d digest.Digest) error {
	c, err := DigestToCID(d)
	if err != nil {
		return err
	}

	return h.d.Provide(ctx, c, true)
}

// Withdraw implements ifaces.DHT. libp2p kad-dht has no protocol-level
// withdraw - provider records expire at the 24 h TTL. The advertiser
// achieves the same effect by simply not re-calling Provide for
// withdrawn digests on its next refresh tick, so this hook exists
// purely as a cooperation point for the interface contract and is a
// no-op today. Future work may emit a libp2p custom protocol message
// to peers in the routing table to evict the stale record sooner, but
// the plan explicitly accepts TTL drainage as adequate.
func (h *Host) Withdraw(_ context.Context, _ digest.Digest) error {
	return nil
}

// FindProviders implements ifaces.DHT. Returns providers whose multiaddrs
// expose at least one IP-based transport (TCP or QUIC). Provider.NodeID is
// the libp2p peer.ID as a string; Provider.Addr is the first IP-based
// multiaddr's IP, suffixed with the conventional transfer port `:5001`.
// coord layer will reconcile peer.ID with k8s NodeID using
// Members; callers (the mirror miss path) only need a dialable
// transfer URL.
func (h *Host) FindProviders(ctx context.Context, d digest.Digest) ([]ifaces.Provider, error) {
	c, err := DigestToCID(d)
	if err != nil {
		return nil, err
	}
	// kad-dht's FindProviders is bounded by a default count internally;
	// the synchronous variant collects until the routing layer signals
	// done or ctx fires.
	start := time.Now()

	ais, err := h.d.FindProviders(ctx, c)
	if h.monitor != nil && err == nil {
		h.monitor.ObserveLatency(time.Since(start))
	}

	if err != nil {
		return nil, err
	}

	out := make([]ifaces.Provider, 0, len(ais))
	for _, ai := range ais {
		addr := transferAddrWithPort(ai, h.transferPort)
		if addr == "" {
			continue
		}
		// Cache the addr info in the peerstore so subsequent dials don't
		// require another DHT round-trip.
		h.h.Peerstore().AddAddrs(ai.ID, ai.Addrs, peerstore.AddressTTL)
		out = append(out, ifaces.Provider{
			NodeID: ifaces.NodeID(ai.ID.String()),
			Addr:   addr,
		})
	}

	return out, nil
}

// Health returns the geometric-mean health score (routing-table
// coverage, p95 lookup latency, self-test success rate). Returns 1.0
// when no monitor is wired (test mode).
func (h *Host) Health() float64 {
	if h.monitor == nil {
		return 1.0
	}

	return h.monitor.Score()
}

// Monitor returns the underlying health monitor for callers that need
// finer-grained signals (e.g. `InBootstrapWindow`). May be nil when
// the host was constructed without monitoring (test mode).
func (h *Host) Monitor() *Monitor { return h.monitor }

// runSelfTest performs one Provide -> FindProviders cycle on a
// peer-derived CID. Success means the routing layer accepted the
// provider record and the immediate-follow-up FindProviders saw at
// least one provider (which must include this host's own entry).
func (h *Host) runSelfTest(ctx context.Context) bool {
	// Derive a stable self-CID from the libp2p peer ID. Hashing the
	// peer ID bytes yields a 32-byte SHA-256 digest we can wrap into
	// a CID via DigestToCID.
	peerBytes, err := h.h.ID().Marshal()
	if err != nil {
		return false
	}

	hashHex := digestPeerSelfID(peerBytes)

	d, err := digest.Parse("sha256:" + hashHex)
	if err != nil {
		return false
	}

	c, err := DigestToCID(d)
	if err != nil {
		return false
	}

	if err := h.d.Provide(ctx, c, true); err != nil {
		return false
	}

	ais, err := h.d.FindProviders(ctx, c)
	if err != nil {
		return false
	}

	return len(ais) > 0
}

// Compile-time check.
var _ ifaces.DHT = (*Host)(nil)

// dialBootstrap implements the bootstrap cascade: randomly select 8
// peers from the configured pool and dial them in parallel with a 5s
// per-peer timeout. If fewer than 4 successfully connect, draw another
// random subset of 8 from the remaining pool. The total number of dial
// attempts is capped at 32. Failures are logged at DEBUG - kad-dht's own
// bootstrap routine retries periodically.
func (h *Host) dialBootstrap(ctx context.Context, peers []string) {
	if len(peers) == 0 {
		return
	}

	// Parse all peers up-front; drop unparseable ones.
	pool := make([]peer.AddrInfo, 0, len(peers))
	positions := make(map[peer.ID]int, len(peers))

	for _, p := range peers {
		ai, err := peer.AddrInfoFromString(p)
		if err != nil {
			h.logger.Warn("bootstrap peer parse failed",
				slog.String("multiaddr", p),
				slog.Any("err", err),
			)

			continue
		}

		if ai.ID == h.h.ID() {
			continue
		}

		mergePeerAddrInfo(&pool, positions, *ai)
	}

	if len(pool) == 0 {
		return
	}

	h.dialBootstrapPool(ctx, pool)
}

func mergePeerAddrInfo(pool *[]peer.AddrInfo, positions map[peer.ID]int, candidate peer.AddrInfo) {
	if index, ok := positions[candidate.ID]; ok {
		existing := &(*pool)[index]
		seen := make(map[string]struct{}, len(existing.Addrs))

		for _, addr := range existing.Addrs {
			seen[addr.String()] = struct{}{}
		}

		for _, addr := range candidate.Addrs {
			if _, ok := seen[addr.String()]; ok {
				continue
			}

			existing.Addrs = append(existing.Addrs, addr)
			seen[addr.String()] = struct{}{}
		}

		return
	}

	positions[candidate.ID] = len(*pool)
	*pool = append(*pool, candidate)
}

func (h *Host) dialBootstrapPool(ctx context.Context, pool []peer.AddrInfo) int {
	const (
		batchSize       = 8
		successQuorum   = 4
		totalDialBudget = 32
	)

	// Fisher–Yates shuffle for unbiased random subsets.
	rng := newBootstrapRand()
	rng.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })

	dialed := 0
	connected := 0

	cursor := 0
	for cursor < len(pool) && dialed < totalDialBudget {
		end := cursor + batchSize
		if end > len(pool) {
			end = len(pool)
		}

		if dialed+(end-cursor) > totalDialBudget {
			end = cursor + (totalDialBudget - dialed)
		}

		batch := pool[cursor:end]
		cursor = end

		successes := h.dialBatch(ctx, batch)
		dialed += len(batch)
		connected += successes

		if connected >= successQuorum {
			return connected
		}
	}

	return connected
}

// dialBatch fans out parallel Connect attempts against batch with a 5s
// timeout each and returns the number that succeeded.
func (h *Host) dialBatch(ctx context.Context, batch []peer.AddrInfo) int {
	var (
		wg   sync.WaitGroup
		succ atomicInt
	)

	for _, ai := range batch {
		wg.Add(1)

		go func(ai peer.AddrInfo) {
			defer wg.Done()

			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()

			if err := h.h.Connect(cctx, ai); err != nil {
				h.logger.Debug("bootstrap connect failed",
					slog.String("peer", ai.ID.String()),
					slog.Any("err", err),
				)

				return
			}

			succ.add(1)
			h.logger.Info("bootstrap peer connected", slog.String("peer", ai.ID.String()))
		}(ai)
	}

	wg.Wait()

	return succ.load()
}

type atomicInt struct {
	mu sync.Mutex
	v  int
}

func (a *atomicInt) add(d int) { a.mu.Lock(); a.v += d; a.mu.Unlock() }
func (a *atomicInt) load() int { a.mu.Lock(); defer a.mu.Unlock(); return a.v }

// newBootstrapRand returns a seeded *math/rand.Rand. We deliberately
// avoid global rand to keep tests deterministic when needed.
func newBootstrapRand() *mathrand.Rand {
	return mathrand.New(mathrand.NewSource(time.Now().UnixNano()))
}

// DigestToCID maps an OCI digest to the CIDv1 used as the DHT provider key.
// CID derivation: Multihash(sha256, raw_digest_bytes) wrapped in CID v1 with
// codec=raw. The derivation is deterministic so any agent re-derives the
// same CID from the same digest.
func DigestToCID(d digest.Digest) (cid.Cid, error) {
	if d.Algorithm() != digest.SHA256 {
		return cid.Undef, fmt.Errorf("discovery: unsupported digest algo %q", d.Algorithm())
	}

	raw, err := hex.DecodeString(d.Hex())
	if err != nil {
		return cid.Undef, fmt.Errorf("discovery: decode digest hex: %w", err)
	}

	mh, err := multihash.Encode(raw, multihash.SHA2_256)
	if err != nil {
		return cid.Undef, fmt.Errorf("discovery: multihash encode: %w", err)
	}

	return cid.NewCidV1(cid.Raw, mh), nil
}

// transferAddrWithPort extracts the dial address for the peer's transfer
// endpoint by walking its multiaddrs for a remotely usable IPv4/IPv6
// component and appending the configured port. Returns the empty string
// if no usable IP-based multiaddr is present.
func transferAddrWithPort(ai peer.AddrInfo, port int) string {
	for _, ma := range ai.Addrs {
		value, ok := extractIP(ma)
		if !ok {
			continue
		}

		ip := net.ParseIP(value)
		if ip == nil || !ip.IsGlobalUnicast() {
			continue
		}

		return net.JoinHostPort(value, strconv.Itoa(port))
	}

	return ""
}

func extractIP(ma multiaddr.Multiaddr) (string, bool) {
	if v, err := ma.ValueForProtocol(multiaddr.P_IP4); err == nil {
		return v, true
	}

	if v, err := ma.ValueForProtocol(multiaddr.P_IP6); err == nil {
		return v, true
	}

	return "", false
}

// loadOrCreateIdentity loads a libp2p private key from disk, generating
// and saving a fresh Ed25519 key if the file doesn't exist.
func loadOrCreateIdentity(path string, logger *slog.Logger) (crypto.PrivKey, error) {
	if path == "" {
		// Ephemeral.
		priv, _, err := crypto.GenerateEd25519Key(nil)
		return priv, err
	}

	if b, err := os.ReadFile(path); err == nil {
		k, err := crypto.UnmarshalPrivateKey(b)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}

		logger.Info("loaded libp2p identity", slog.String("path", path))

		return k, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}

	priv, _, err := crypto.GenerateEd25519Key(nil)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}

	b, err := crypto.MarshalPrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("marshal key: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}

	if err := os.WriteFile(path, b, 0o600); err != nil {
		return nil, fmt.Errorf("write %s: %w", path, err)
	}

	logger.Info("generated libp2p identity", slog.String("path", path))

	return priv, nil
}

// digestPeerSelfID returns the lower-case hex sha-256 of `peerBytes`,
// used as the OCI-style digest hex for the self-test CID.
func digestPeerSelfID(peerBytes []byte) string {
	sum := sha256.Sum256(peerBytes)
	return hex.EncodeToString(sum[:])
}
