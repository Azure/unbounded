// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
)

// Cachemesh defaults.
const (
	// CachemeshLeaseName is the name of the cachemesh leader Lease.
	CachemeshLeaseName = "gantry-cachemesh"

	DefaultCachemeshChunkSize     = 64 << 20
	DefaultCachemeshContentRoot   = "/var/lib/containerd/io.containerd.content.v1.content"
	DefaultCachemeshInflightBytes = 2 << 30
	DefaultCachemeshMaxFills      = 64

	// DefaultCachemeshMaxNeighbors is gantry's data-plane fan-out. The
	// cachemesh library defaults to 256, sized for 100k-member clusters.
	// In a cluster of a few hundred agents 256 makes every agent a direct
	// neighbor of every other one, so the owner of a popular chunk serves
	// every agent itself. 32 forces a multi-hop relay grid that spreads
	// that load over relay caches.
	DefaultCachemeshMaxNeighbors = 32

	// MinCachemeshChunkSize and MaxCachemeshChunkSize bound chunk_size.
	MinCachemeshChunkSize = 1 << 20
	MaxCachemeshChunkSize = 1 << 30
)

// CachemeshConfig configures the optional cachemesh content path. When
// Enabled is false (the default) nothing in this struct has any effect and
// the agent behaves exactly as without it.
//
// With cachemesh enabled, blobs are split into ChunkSize chunks. Each chunk
// is owned by one agent (the owner of its cachemesh partition), which pulls
// it from the origin registry once; other agents fetch it hop by hop through
// the cachemesh grid. Agents keep chunks only in bounded memory; the only
// on-disk copy is containerd's content store (ContentRoot), which agents
// read back to serve peers. Tag resolutions are cached for 5 minutes.
// See designs/cachemesh.md.
type CachemeshConfig struct {
	// Enabled turns the cachemesh path on (GANTRY_CACHEMESH_ENABLED). The
	// Racer backend ignores it.
	Enabled bool `yaml:"enabled"`

	// Namespace holds the cachemesh leader Lease. Empty uses chair_namespace.
	Namespace string `yaml:"namespace"`

	// PeerKeyFile is the file holding the shared keys that authenticate
	// cachemesh traffic between agents (GANTRY_CACHEMESH_PEER_KEY_FILE).
	// Required when cachemesh is enabled. One key per line, surrounding
	// whitespace trimmed, blank lines ignored; each key is at least 32
	// bytes. The first key signs requests and every key verifies them, so
	// a key rotates by adding the new key on a second line, moving it to
	// the first line once every agent has it, then removing the old one.
	PeerKeyFile string `yaml:"peer_key_file"`

	// ChunkSize is the stripe size in bytes. Every agent in a cluster must
	// use the same value; requests with a different size are refused. Must
	// be a power of two between 1MiB and 1GiB.
	ChunkSize int64 `yaml:"chunk_size"`

	// ExpectedSize is the expected number of agents (cachemesh
	// Config.ExpectedSize). Zero uses the library default.
	ExpectedSize int `yaml:"expected_size"`

	// MaxNeighbors bounds each agent's data-plane neighbors. The default,
	// DefaultCachemeshMaxNeighbors (32), forces multi-hop relays that
	// spread the load of popular chunks over relay caches. 256 (the
	// cachemesh library sizing) makes every agent of a cluster of about
	// 150 a neighbor of every other one.
	MaxNeighbors int `yaml:"max_neighbors"`

	// CacheTags serves manifest-by-tag requests through the cachemesh
	// metadata cache. Only anonymous requests to registries that do not
	// require authentication are served this way; see the design doc for
	// the tag-binding caveat.
	CacheTags bool `yaml:"cache_tags"`

	// VerifyDigest hashes every full-blob response the cachemesh path
	// serves and aborts the response before its last byte on a mismatch,
	// evicting the cached chunks.
	VerifyDigest bool `yaml:"verify_digest"`

	// ContentRoot is the containerd content store directory (holding
	// blobs/ and ingest/), mounted read-only into the agent. Chunks are
	// served to peers from the blobs and in-progress downloads there; the
	// agent never writes image bytes to disk itself. Empty, or a root the
	// agent cannot read, serves peers from memory only.
	ContentRoot string `yaml:"content_root"`

	// InflightBytes bounds memory that fills borrow beyond their 4MiB
	// window: when a reader lags behind a fill that peers also read, so one
	// slow reader does not slow the others down, and to keep chunks that
	// peers read in memory for later peers (for at most 30s unused;
	// reclaimed first). When it is exhausted the slowest reader the content
	// store does not cover paces the fill. Zero disables borrowing.
	InflightBytes int64 `yaml:"inflight_bytes"`

	// MaxConcurrentChunkFills caps chunks this agent fetches from a peer or
	// the origin at once. Peer requests over the cap get 429.
	MaxConcurrentChunkFills int `yaml:"max_concurrent_chunk_fills"`
}

func defaultCachemeshConfig() CachemeshConfig {
	return CachemeshConfig{
		ChunkSize:               DefaultCachemeshChunkSize,
		MaxNeighbors:            DefaultCachemeshMaxNeighbors,
		CacheTags:               true,
		VerifyDigest:            true,
		ContentRoot:             DefaultCachemeshContentRoot,
		InflightBytes:           DefaultCachemeshInflightBytes,
		MaxConcurrentChunkFills: DefaultCachemeshMaxFills,
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
	setInt := func(dst *int) func(int64) { return func(n int64) { *dst = int(n) } }

	boolean("CACHEMESH_ENABLED", &m.Enabled)
	str("CACHEMESH_NAMESPACE", &m.Namespace)
	str("CACHEMESH_PEER_KEY_FILE", &m.PeerKeyFile)
	num("CACHEMESH_CHUNK_SIZE", func(n int64) { m.ChunkSize = n })
	num("CACHEMESH_EXPECTED_SIZE", setInt(&m.ExpectedSize))
	num("CACHEMESH_MAX_NEIGHBORS", setInt(&m.MaxNeighbors))
	boolean("CACHEMESH_CACHE_TAGS", &m.CacheTags)
	boolean("CACHEMESH_VERIFY_DIGEST", &m.VerifyDigest)
	str("CACHEMESH_CONTENT_ROOT", &m.ContentRoot)
	num("CACHEMESH_INFLIGHT_BYTES", func(n int64) { m.InflightBytes = n })
	num("CACHEMESH_MAX_CONCURRENT_CHUNK_FILLS", setInt(&m.MaxConcurrentChunkFills))

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

	if m.PeerKeyFile == "" {
		bad("peer_key_file", "required: cachemesh traffic between agents is authenticated with shared keys")
	} else if !filepath.IsAbs(m.PeerKeyFile) {
		bad("peer_key_file", "must be an absolute path, got %q", m.PeerKeyFile)
	}

	if m.ChunkSize < MinCachemeshChunkSize || m.ChunkSize > MaxCachemeshChunkSize || m.ChunkSize&(m.ChunkSize-1) != 0 {
		bad("chunk_size", "must be a power of two between %d and %d, got %d", MinCachemeshChunkSize, MaxCachemeshChunkSize, m.ChunkSize)
	}

	if m.ExpectedSize < 0 {
		bad("expected_size", "must be >= 0, got %d", m.ExpectedSize)
	}

	if m.MaxNeighbors < 1 {
		bad("max_neighbors", "must be >= 1, got %d", m.MaxNeighbors)
	}

	if m.ContentRoot != "" && !filepath.IsAbs(m.ContentRoot) {
		bad("content_root", "must be empty or an absolute path, got %q", m.ContentRoot)
	}

	if m.InflightBytes < 0 {
		bad("inflight_bytes", "must be >= 0, got %d", m.InflightBytes)
	}

	if m.MaxConcurrentChunkFills < 1 {
		bad("max_concurrent_chunk_fills", "must be >= 1, got %d", m.MaxConcurrentChunkFills)
	}

	return errs
}
