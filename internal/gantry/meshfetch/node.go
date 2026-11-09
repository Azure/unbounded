// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package meshfetch distributes image content between Gantry agents with the
// cachemesh routing library (internal/cachemesh).
//
// Blobs are striped into fixed-size chunks. Every chunk is a key in the
// immutable "gantry-blob" keyspace, so each chunk has one owner agent that
// pulls it from the origin registry with a bounded Range request. Other
// agents fetch chunks hop by hop through the cachemesh grid: each hop asks
// Mesh.Forward where the request goes next, coalesces concurrent requests for
// the same chunk into one upstream fetch, keeps the chunk in a disk-backed
// store with a small hot page tier in memory, and streams bytes downstream
// as they arrive (cut-through). Tag
// resolutions live in a separate "gantry-meta" keyspace with a TTL.
//
// The cachemesh library only routes; this package owns the data-plane
// transport (HTTP on the transfer listener), the chunk store, coalescing,
// retries, hedging, and metrics. See designs/gantry-cachemesh.md.
package meshfetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

const noSlot = cachemesh.NoSlot

// Endpoint paths on the transfer listener.
const (
	ControlPath = "/cachemesh/control"
	ChunkPath   = "/cachemesh/chunk"
	MetaPath    = "/cachemesh/meta"
)

// Request and response headers of the chunk and metadata endpoints.
const (
	headerRoute       = "Gantry-Cachemesh-Route"
	headerBlobSize    = "Gantry-Blob-Size"
	headerStampExpire = "Gantry-Stamp-Expires"
	headerStampGen    = "Gantry-Stamp-Gen"
	headerDigest      = "Docker-Content-Digest"
)

// Origin is the bounded-range origin capability the chunk owner uses.
type Origin interface {
	PullRange(ctx context.Context, ref ifaces.OriginRef, length int64) (io.ReadCloser, int64, string, error)
}

// TagOrigin resolves a tag at the origin. Optional.
type TagOrigin interface {
	ManifestByTag(ctx context.Context, registry, repo, tag string) ([]byte, string, digest.Digest, error)
}

// Hooks are optional metric callbacks. Every hook may be nil.
type Hooks struct {
	// OnLookup reports a chunk lookup by a local blob request ("local") or
	// a peer ("peer"): result is "hit" (complete in cache), "joined" (an
	// in-progress fill), or "fill" (a new fill started).
	OnLookup func(requester, result string)
	// OnFill reports a finished fill attempt: source is "origin", "peer",
	// "prev_owner", or "owner" (a backup reading through from the owner);
	// outcome is "ok" or "error".
	OnFill func(source, outcome string, bytes int64, d time.Duration)
	// OnPeerResult reports every peer request outcome as given to
	// Mesh.Observe ("success", "failure", "timeout", "overload") with the
	// time to response headers.
	OnPeerResult func(result string, d time.Duration)
	// OnHedge reports hedges: "sent", "won", or "denied".
	OnHedge func(outcome string)
	// OnServe reports a chunk response to a peer: outcome is "ok",
	// "error", "shed", "loop", "no_route", "ttl", "not_found", or
	// "bad_request"; bytes is the body bytes sent.
	OnServe func(outcome string, bytes int64)
	// OnBlob reports a local blob request: outcome is "served",
	// "declined", "aborted", or "verify_failed".
	OnBlob func(outcome string, bytes int64, d time.Duration)
	// OnMeta reports a tag lookup: "hit", "origin", "peer", "error".
	OnMeta func(outcome string)
	// OnEvict reports bytes evicted from the disk chunk store.
	OnEvict func(bytes int64)
}

// Config configures a Node.
type Config struct {
	// Mesh routes requests. Required. The caller runs it.
	Mesh *cachemesh.Mesh
	// Origin fills chunks this node owns. Required.
	Origin Origin
	// Tags resolves tags this node owns. Nil disables tag resolution.
	Tags TagOrigin

	ChunkSize int64
	// ChunkDir holds chunk files. It is wiped by New. Empty disables the
	// disk tier: chunks then only stream through memory.
	ChunkDir string
	// DiskBytes bounds chunk files in ChunkDir. Zero disables the disk
	// tier.
	DiskBytes int64
	// MemoryBytes bounds the hot page tier: pages read from disk by
	// concurrent readers that stay in memory. Zero disables it.
	MemoryBytes int64
	// PageSize is the unit of in-flight buffers, disk I/O, read
	// coalescing, and the hot tier. Zero uses min(1MiB, ChunkSize). It
	// must divide ChunkSize.
	PageSize int
	// FillPages is the number of in-flight pages each fill holds. Zero
	// uses 4.
	FillPages int
	// InflightBytes bounds pages that fills borrow beyond FillPages when a
	// reader lags behind a fill that peers read. When it is exhausted the
	// fill spills to disk. Zero disables borrowing.
	InflightBytes int64
	// PageCache selects page cache handling of chunk files. Empty uses
	// PageCacheBypass.
	PageCache PageCacheMode
	// Persist selects which chunks are written to disk. Empty uses
	// PersistAll.
	Persist PersistPolicy

	MaxFills     int
	MaxServes    int
	Lookahead    int
	ChunkTimeout time.Duration
	StallTimeout time.Duration
	Attempts     int
	Hedge        bool
	Verify       bool
	MetadataTTL  time.Duration

	// Client sends data-plane requests. Nil uses NewDataClient().
	Client *http.Client
	// DataAddr maps a peer to its data-plane host:port. Nil uses the
	// peer's control address (the same transfer listener).
	DataAddr func(cachemesh.PeerRef) string

	Logger *slog.Logger
	Hooks  Hooks
}

// Node serves and fetches chunks and metadata for one agent.
type Node struct {
	cfg    Config
	mesh   *cachemesh.Mesh
	blobKS cachemesh.Keyspace
	metaKS cachemesh.Keyspace
	store  *store
	meta   *metaCache
	client *http.Client
	log    *slog.Logger
	serves chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
}

// New validates cfg and returns a Node.
func New(cfg Config) (*Node, error) {
	if cfg.Mesh == nil || cfg.Origin == nil {
		return nil, errors.New("meshfetch: Mesh and Origin are required")
	}

	if cfg.ChunkSize <= 0 || cfg.ChunkSize > 1<<30 {
		return nil, fmt.Errorf("meshfetch: bad chunk size %d", cfg.ChunkSize)
	}

	if cfg.DiskBytes < 0 || cfg.MemoryBytes < 0 || cfg.FillPages < 0 || cfg.InflightBytes < 0 {
		return nil, errors.New("meshfetch: negative cache size")
	}

	if cfg.DiskBytes > 0 && cfg.DiskBytes < 2*cfg.ChunkSize {
		return nil, fmt.Errorf("meshfetch: disk budget of %d bytes must hold two chunks of %d", cfg.DiskBytes, cfg.ChunkSize)
	}

	if cfg.PageSize == 0 {
		cfg.PageSize = int(min(cfg.ChunkSize, 1<<20))
	}

	if cfg.PageSize < 0 || int64(cfg.PageSize) > cfg.ChunkSize || cfg.ChunkSize%int64(cfg.PageSize) != 0 {
		return nil, fmt.Errorf("meshfetch: page size %d must divide chunk size %d", cfg.PageSize, cfg.ChunkSize)
	}

	switch cfg.PageCache {
	case "", PageCacheBypass, PageCacheDrop, PageCacheKeep:
	default:
		return nil, fmt.Errorf("meshfetch: unknown page cache mode %q", cfg.PageCache)
	}

	switch cfg.Persist {
	case "", PersistAll, PersistShared, PersistOwned:
	default:
		return nil, fmt.Errorf("meshfetch: unknown persist policy %q", cfg.Persist)
	}

	if cfg.MaxFills <= 0 || cfg.MaxServes <= 0 || cfg.Lookahead <= 0 || cfg.Attempts <= 0 ||
		cfg.ChunkTimeout <= 0 || cfg.StallTimeout <= 0 {
		return nil, errors.New("meshfetch: limits and timeouts must be positive")
	}

	ttl := cfg.MetadataTTL
	if ttl == 0 {
		ttl = cachemesh.MinTTL
	}

	metaKS, err := cachemesh.NewKeyspace("gantry-meta", ttl)
	if err != nil {
		return nil, err
	}

	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	if cfg.Client == nil {
		cfg.Client = NewDataClient()
	}

	if cfg.DataAddr == nil {
		cfg.DataAddr = func(p cachemesh.PeerRef) string { return p.Addr }
	}

	log := cfg.Logger.With(slog.String("subsystem", "meshfetch"))
	st := newStore(storeConfig{
		chunkSize:    cfg.ChunkSize,
		pageSize:     cfg.PageSize,
		window:       cfg.FillPages,
		maxFills:     cfg.MaxFills,
		dir:          cfg.ChunkDir,
		diskBudget:   cfg.DiskBytes,
		hotBudget:    cfg.MemoryBytes,
		extraBudget:  cfg.InflightBytes,
		pageCache:    cfg.PageCache,
		persist:      cfg.Persist,
		stallTimeout: cfg.StallTimeout,
		onEvict:      cfg.Hooks.OnEvict,
		logger:       log,
	})

	ctx, cancel := context.WithCancel(context.Background())
	n := &Node{
		cfg:    cfg,
		mesh:   cfg.Mesh,
		blobKS: cachemesh.MustKeyspace("gantry-blob", 0),
		metaKS: metaKS,
		store:  st,
		meta:   newMetaCache(4096),
		client: cfg.Client,
		log:    log,
		serves: make(chan struct{}, cfg.MaxServes),
		ctx:    ctx,
		cancel: cancel,
	}

	return n, nil
}

// Close stops background fills and releases idle buffers.
func (n *Node) Close() {
	n.cancel()
	n.store.close()
}

// Stats returns the chunk store state.
func (n *Node) Stats() Stats { return n.store.stats() }

// Register adds the chunk and metadata endpoints to mux.
func (n *Node) Register(mux *http.ServeMux) {
	mux.HandleFunc(ChunkPath, n.handleChunk)
	mux.HandleFunc(MetaPath, n.handleMeta)
}

// NewDataClient returns an HTTP/1.1 client tuned for bulk chunk transfer:
// one TCP connection per concurrent request avoids HTTP/2 flow-control
// windows capping per-stream throughput, and large buffers cut syscalls.
func NewDataClient() *http.Client {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)

	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}

	return &http.Client{
		Transport: &http.Transport{
			Protocols:             protocols,
			DialContext:           dialer.DialContext,
			MaxIdleConns:          4096,
			MaxIdleConnsPerHost:   64,
			IdleConnTimeout:       90 * time.Second,
			DisableCompression:    true,
			ReadBufferSize:        256 << 10,
			WriteBufferSize:       64 << 10,
			ExpectContinueTimeout: 0,
		},
	}
}

func (n *Node) observe(p cachemesh.PeerRef, r cachemesh.Result, d time.Duration) {
	n.mesh.Observe(p, r, d)

	if h := n.cfg.Hooks.OnPeerResult; h != nil {
		h(resultName(r), d)
	}
}

func resultName(r cachemesh.Result) string {
	switch r {
	case cachemesh.Success:
		return "success"
	case cachemesh.Timeout:
		return "timeout"
	case cachemesh.Overload:
		return "overload"
	default:
		return "failure"
	}
}

func (n *Node) hook(f func(string), v string) {
	if f != nil {
		f(v)
	}
}
