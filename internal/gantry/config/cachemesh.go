// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"time"
)

// Cachemesh defaults.
const (
	DefaultCachemeshLeaseName      = "gantry-cachemesh"
	DefaultCachemeshChunkSize      = 64 << 20
	DefaultCachemeshChunkDir       = "/var/lib/gantry/cachemesh"
	DefaultCachemeshDiskCacheBytes = 20 << 30
	DefaultCachemeshMemCacheBytes  = 512 << 20
	DefaultCachemeshPageSize       = 1 << 20
	DefaultCachemeshFillPages      = 4
	DefaultCachemeshInflightBytes  = 2 << 30
	DefaultCachemeshPageCache      = "bypass"
	DefaultCachemeshPersist        = "owned"
	DefaultCachemeshMetadataTTL    = 5 * time.Minute

	// DefaultCachemeshMaxNeighbors is gantry's data-plane fan-out. The
	// cachemesh library defaults to 256, sized for 100k-member clusters.
	// In a cluster of a few hundred agents 256 makes every agent a direct
	// neighbor of every other one, so the owner of a popular chunk serves
	// every agent itself. 32 forces a multi-hop relay grid that spreads
	// that load over relay caches; on 142 nodes it pulled a 40GB image in
	// a p50 of 91s against 104s with 256 (designs/gantry-cachemesh-benchmark.md).
	DefaultCachemeshMaxNeighbors = 32

	// MinCachemeshChunkSize and MaxCachemeshChunkSize bound chunk_size.
	MinCachemeshChunkSize = 1 << 20
	MaxCachemeshChunkSize = 1 << 30

	// MinCachemeshPageSize bounds page_size from below; pages are the unit
	// of O_DIRECT I/O, which needs 4KiB alignment.
	MinCachemeshPageSize = 64 << 10

	// MinCachemeshMetadataTTL matches cachemesh.MinTTL. It is repeated here
	// so the config package does not import the routing library.
	MinCachemeshMetadataTTL = 5 * time.Minute
)

// CachemeshConfig configures the optional cachemesh content path. When
// Enabled is false (the default) nothing in this struct has any effect and
// the agent behaves exactly as without it.
//
// With cachemesh enabled, blobs are split into ChunkSize chunks. Each chunk
// is owned by one agent (the owner of its cachemesh partition), which pulls
// it from the origin registry once; other agents fetch it hop by hop through
// the cachemesh grid, and every hop may keep a copy in its chunk store: chunk
// files under ChunkDir, plus a small in-memory tier of hot pages. Tag and
// manifest metadata is cached separately with MetadataTTL.
// See designs/gantry-cachemesh.md.
type CachemeshConfig struct {
	// Enabled turns the cachemesh path on (GANTRY_CACHEMESH_ENABLED). The
	// Racer backend ignores it.
	Enabled bool `yaml:"enabled"`

	// Namespace holds the cachemesh leader Lease. Empty uses chair_namespace.
	Namespace string `yaml:"namespace"`

	// LeaseName is the name of the cachemesh leader Lease.
	LeaseName string `yaml:"lease_name"`

	// ChunkSize is the stripe size in bytes. Every agent in a cluster must
	// use the same value; requests with a different size are refused. Must
	// be a power of two between 1MiB and 1GiB.
	ChunkSize int64 `yaml:"chunk_size"`

	// ExpectedSize is the expected number of agents (cachemesh
	// Config.ExpectedSize). Zero uses the library default.
	ExpectedSize int `yaml:"expected_size"`

	// CellSize is the cachemesh grid cell size. Zero uses the library
	// default.
	CellSize int `yaml:"cell_size"`

	// MaxNeighbors bounds each agent's data-plane neighbors. The default is
	// DefaultCachemeshMaxNeighbors (32), which forces multi-hop relays that
	// spread the load of popular chunks over relay caches. An explicit zero
	// uses the cachemesh library default (256), which at about 150 agents
	// makes every agent a neighbor of every other one (a 1-D, fully direct
	// grid).
	MaxNeighbors int `yaml:"max_neighbors"`

	// PartitionBits sets the number of cachemesh partitions (2^bits). Zero
	// uses the library default.
	PartitionBits int `yaml:"partition_bits"`

	// MetadataTTL is how long a cached tag resolution or manifest stays
	// fresh. Must be at least 5m.
	MetadataTTL time.Duration `yaml:"metadata_ttl"`

	// CacheTags serves manifest-by-tag requests through the cachemesh
	// metadata cache. Only anonymous requests to registries that do not
	// require authentication are served this way; see the design doc for
	// the tag-binding caveat.
	CacheTags bool `yaml:"cache_tags"`

	// VerifyDigest hashes every full-blob response the cachemesh path
	// serves and aborts the response before its last byte on a mismatch,
	// evicting the cached chunks.
	VerifyDigest bool `yaml:"verify_digest"`

	// ChunkDir holds chunk files. The agent removes everything in it at
	// startup: chunks are not individually digest-verified, so files from
	// an earlier run are never trusted.
	ChunkDir string `yaml:"chunk_dir"`

	// DiskCacheBytes bounds chunk files in ChunkDir, including chunks
	// being written. Complete chunks are evicted least recently used
	// first, chunks this agent owns last. Fills that find no room stream
	// through memory without a disk copy. Zero disables the disk tier.
	DiskCacheBytes int64 `yaml:"disk_cache_bytes"`

	// MemoryCacheBytes bounds the hot page tier: pages read from disk by
	// two or more concurrent readers that stay in memory for the next
	// ones. Zero disables it. Total chunk memory is about this plus
	// inflight_bytes plus page_size * (fill_pages *
	// max_concurrent_chunk_fills + one page per active reader).
	MemoryCacheBytes int64 `yaml:"memory_cache_bytes"`

	// PageSize is the unit of in-flight buffers, disk I/O, read
	// coalescing, and the hot tier: a power of two between 64KiB and
	// chunk_size.
	PageSize int64 `yaml:"page_size"`

	// FillPages is the number of in-flight pages each chunk fill holds in
	// memory. Readers within this window get bytes from memory as they
	// arrive; slower readers read from disk.
	FillPages int `yaml:"fill_pages"`

	// InflightBytes bounds memory that fills borrow beyond fill_pages: when
	// a reader lags behind a fill that peers also read, so one slow reader
	// does not slow the others down, and to keep chunks that peers read in
	// memory for later peers (for at most 30s unused; reclaimed first).
	// When it is exhausted a fill with a lagging reader spills to disk;
	// without disk the slowest reader paces it. Zero disables borrowing.
	InflightBytes int64 `yaml:"inflight_bytes"`

	// PageCache selects how chunk files use the kernel page cache:
	// "bypass" (O_DIRECT, the default), "drop" (buffered I/O, then
	// posix_fadvise DONTNEED), or "keep" (buffered, left to the kernel).
	// Page cache counts toward the container's working set.
	PageCache string `yaml:"page_cache"`

	// Persist selects which chunks are written to disk: "all", "shared"
	// (only chunks this agent owns or serves to a peer; chunks only this
	// agent consumes stream through memory), or "owned" (only chunks this
	// agent owns; relays keep nothing, and a late peer request refetches
	// the chunk from upstream).
	Persist string `yaml:"persist"`

	// MaxConcurrentChunkFills caps chunks this agent fetches from a peer or
	// the origin at once. Peer requests over the cap get 429.
	MaxConcurrentChunkFills int `yaml:"max_concurrent_chunk_fills"`

	// MaxConcurrentChunkServes caps concurrent chunk responses to peers.
	// Requests over the cap get 429.
	MaxConcurrentChunkServes int `yaml:"max_concurrent_chunk_serves"`

	// Lookahead is the number of chunks of one blob fetched in parallel
	// ahead of the chunk being written to containerd.
	Lookahead int `yaml:"lookahead"`

	// ChunkTimeout bounds one attempt to fetch a chunk from a peer or the
	// origin.
	ChunkTimeout time.Duration `yaml:"chunk_timeout"`

	// ChunkStallTimeout fails a chunk attempt that makes no progress for
	// this long.
	ChunkStallTimeout time.Duration `yaml:"chunk_stall_timeout"`

	// ChunkAttempts is the number of attempts per chunk (first try plus
	// retries to the alternate).
	ChunkAttempts int `yaml:"chunk_attempts"`

	// Hedge sends a copy of a slow chunk request to the alternate peer
	// when the cachemesh hedge budget allows.
	Hedge bool `yaml:"hedge"`
}

func defaultCachemeshConfig() CachemeshConfig {
	return CachemeshConfig{
		Enabled:                  false,
		LeaseName:                DefaultCachemeshLeaseName,
		ChunkSize:                DefaultCachemeshChunkSize,
		MaxNeighbors:             DefaultCachemeshMaxNeighbors,
		MetadataTTL:              DefaultCachemeshMetadataTTL,
		CacheTags:                true,
		VerifyDigest:             true,
		ChunkDir:                 DefaultCachemeshChunkDir,
		DiskCacheBytes:           DefaultCachemeshDiskCacheBytes,
		MemoryCacheBytes:         DefaultCachemeshMemCacheBytes,
		PageSize:                 DefaultCachemeshPageSize,
		FillPages:                DefaultCachemeshFillPages,
		InflightBytes:            DefaultCachemeshInflightBytes,
		PageCache:                DefaultCachemeshPageCache,
		Persist:                  DefaultCachemeshPersist,
		MaxConcurrentChunkFills:  64,
		MaxConcurrentChunkServes: 512,
		Lookahead:                8,
		ChunkTimeout:             5 * time.Minute,
		ChunkStallTimeout:        30 * time.Second,
		ChunkAttempts:            4,
		Hedge:                    true,
	}
}

// CachemeshNamespace returns the namespace of the cachemesh Lease.
func (c *Config) CachemeshNamespace() string {
	if c.Cachemesh.Namespace != "" {
		return c.Cachemesh.Namespace
	}

	return c.ChairNamespace
}

func (c *Config) loadCachemeshEnv(env func(string) string) []error {
	var errs []error

	m := &c.Cachemesh
	str := func(key string, dst *string) {
		if v, ok := lookup(env, key); ok {
			*dst = v
		}
	}
	num := func(key string, set func(int64)) {
		if v, ok := lookup(env, key); ok {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				errs = append(errs, fmt.Errorf("env GANTRY_%s: %w", key, err))
				return
			}

			set(n)
		}
	}
	boolean := func(key string, dst *bool) {
		if v, ok := lookup(env, key); ok {
			b, err := strconv.ParseBool(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("env GANTRY_%s: %w", key, err))
				return
			}

			*dst = b
		}
	}
	dur := func(key string, dst *time.Duration) {
		if v, ok := lookup(env, key); ok {
			d, err := time.ParseDuration(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("env GANTRY_%s: %w", key, err))
				return
			}

			*dst = d
		}
	}
	setInt := func(dst *int) func(int64) { return func(n int64) { *dst = int(n) } }

	boolean("CACHEMESH_ENABLED", &m.Enabled)
	str("CACHEMESH_NAMESPACE", &m.Namespace)
	str("CACHEMESH_LEASE_NAME", &m.LeaseName)
	num("CACHEMESH_CHUNK_SIZE", func(n int64) { m.ChunkSize = n })
	num("CACHEMESH_EXPECTED_SIZE", setInt(&m.ExpectedSize))
	num("CACHEMESH_CELL_SIZE", setInt(&m.CellSize))
	num("CACHEMESH_MAX_NEIGHBORS", setInt(&m.MaxNeighbors))
	num("CACHEMESH_PARTITION_BITS", setInt(&m.PartitionBits))
	dur("CACHEMESH_METADATA_TTL", &m.MetadataTTL)
	boolean("CACHEMESH_CACHE_TAGS", &m.CacheTags)
	boolean("CACHEMESH_VERIFY_DIGEST", &m.VerifyDigest)
	str("CACHEMESH_CHUNK_DIR", &m.ChunkDir)
	num("CACHEMESH_DISK_CACHE_BYTES", func(n int64) { m.DiskCacheBytes = n })
	num("CACHEMESH_MEMORY_CACHE_BYTES", func(n int64) { m.MemoryCacheBytes = n })
	num("CACHEMESH_PAGE_SIZE", func(n int64) { m.PageSize = n })
	num("CACHEMESH_FILL_PAGES", setInt(&m.FillPages))
	num("CACHEMESH_INFLIGHT_BYTES", func(n int64) { m.InflightBytes = n })
	str("CACHEMESH_PAGE_CACHE", &m.PageCache)
	str("CACHEMESH_PERSIST", &m.Persist)
	num("CACHEMESH_MAX_CONCURRENT_CHUNK_FILLS", setInt(&m.MaxConcurrentChunkFills))
	num("CACHEMESH_MAX_CONCURRENT_CHUNK_SERVES", setInt(&m.MaxConcurrentChunkServes))
	num("CACHEMESH_LOOKAHEAD", setInt(&m.Lookahead))
	dur("CACHEMESH_CHUNK_TIMEOUT", &m.ChunkTimeout)
	dur("CACHEMESH_CHUNK_STALL_TIMEOUT", &m.ChunkStallTimeout)
	num("CACHEMESH_CHUNK_ATTEMPTS", setInt(&m.ChunkAttempts))
	boolean("CACHEMESH_HEDGE", &m.Hedge)

	return errs
}

func (c *Config) validateCachemesh() []error {
	m := &c.Cachemesh
	if !m.Enabled {
		return nil
	}

	var errs []error

	bad := func(field, format string, args ...any) {
		errs = append(errs, fmt.Errorf("cachemesh."+field+": "+format, args...))
	}

	if c.CachemeshNamespace() == "" {
		bad("namespace", "required (or set chair_namespace) for the cachemesh Lease")
	}

	if c.PodIP == "" {
		errs = append(errs, errors.New("cachemesh: requires pod_ip for the member address"))
	}

	if m.LeaseName == "" {
		bad("lease_name", "required")
	}

	if m.ChunkSize < MinCachemeshChunkSize || m.ChunkSize > MaxCachemeshChunkSize || m.ChunkSize&(m.ChunkSize-1) != 0 {
		bad("chunk_size", "must be a power of two between %d and %d, got %d", MinCachemeshChunkSize, MaxCachemeshChunkSize, m.ChunkSize)
	}

	if m.ExpectedSize < 0 {
		bad("expected_size", "must be >= 0, got %d", m.ExpectedSize)
	}

	if m.CellSize < 0 {
		bad("cell_size", "must be >= 0, got %d", m.CellSize)
	}

	if m.MaxNeighbors < 0 {
		bad("max_neighbors", "must be >= 0, got %d", m.MaxNeighbors)
	}

	if m.PartitionBits < 0 || m.PartitionBits > 24 {
		bad("partition_bits", "must be between 0 and 24, got %d", m.PartitionBits)
	}

	if m.MetadataTTL < MinCachemeshMetadataTTL {
		bad("metadata_ttl", "must be >= %v, got %v", MinCachemeshMetadataTTL, m.MetadataTTL)
	}

	if m.DiskCacheBytes != 0 && m.DiskCacheBytes < 2*m.ChunkSize {
		bad("disk_cache_bytes", "must be 0 (no disk tier) or hold at least two chunks (%d), got %d", 2*m.ChunkSize, m.DiskCacheBytes)
	}

	if m.DiskCacheBytes != 0 && !filepath.IsAbs(m.ChunkDir) {
		bad("chunk_dir", "must be an absolute path when disk_cache_bytes is set, got %q", m.ChunkDir)
	}

	if m.MemoryCacheBytes < 0 {
		bad("memory_cache_bytes", "must be >= 0, got %d", m.MemoryCacheBytes)
	}

	if m.PageSize < MinCachemeshPageSize || m.PageSize&(m.PageSize-1) != 0 || m.PageSize > m.ChunkSize {
		bad("page_size", "must be a power of two between %d and chunk_size, got %d", MinCachemeshPageSize, m.PageSize)
	}

	if m.InflightBytes < 0 {
		bad("inflight_bytes", "must be >= 0, got %d", m.InflightBytes)
	}

	if m.FillPages < 2 || m.FillPages > 256 {
		bad("fill_pages", "must be between 2 and 256, got %d", m.FillPages)
	}

	switch m.PageCache {
	case "bypass", "drop", "keep":
	default:
		bad("page_cache", "must be bypass, drop, or keep, got %q", m.PageCache)
	}

	switch m.Persist {
	case "all", "shared", "owned":
	default:
		bad("persist", "must be all, shared, or owned, got %q", m.Persist)
	}

	if m.MaxConcurrentChunkFills < 1 {
		bad("max_concurrent_chunk_fills", "must be >= 1, got %d", m.MaxConcurrentChunkFills)
	}

	if m.MaxConcurrentChunkServes < 1 {
		bad("max_concurrent_chunk_serves", "must be >= 1, got %d", m.MaxConcurrentChunkServes)
	}

	if m.Lookahead < 1 || m.Lookahead > 256 {
		bad("lookahead", "must be between 1 and 256, got %d", m.Lookahead)
	}

	if m.ChunkTimeout <= 0 {
		bad("chunk_timeout", "must be > 0, got %v", m.ChunkTimeout)
	}

	if m.ChunkStallTimeout <= 0 {
		bad("chunk_stall_timeout", "must be > 0, got %v", m.ChunkStallTimeout)
	}

	if m.ChunkAttempts < 1 {
		bad("chunk_attempts", "must be >= 1, got %d", m.ChunkAttempts)
	}

	return errs
}
