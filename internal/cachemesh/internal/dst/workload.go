// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package dst

import (
	"encoding/binary"
	"math/rand/v2"
	"strconv"

	"github.com/Azure/unbounded/internal/cachemesh/internal/hashing"
)

// KeyKind is the kind of a generated workload key.
type KeyKind uint8

const (
	// KeyBlob is a content-addressed blob: a 32-byte digest.
	KeyBlob KeyKind = iota
	// KeyStripe is a stripe of a layer: a 32-byte digest followed by a
	// 4-byte big-endian stripe index.
	KeyStripe
	// KeyMeta is a metadata key of the form "repo-N:tag-M".
	KeyMeta
)

// String returns the kind name.
func (k KeyKind) String() string {
	switch k {
	case KeyBlob:
		return "blob"
	case KeyStripe:
		return "stripe"
	case KeyMeta:
		return "meta"
	default:
		return "unknown"
	}
}

// WorkloadConfig configures a KeyGen. Zero values select defaults.
type WorkloadConfig struct {
	// Keys is the number of distinct objects per kind. Default 100,000.
	Keys uint64
	// Skew is the Zipf exponent s (> 1). Default 1.1.
	Skew float64
	// Stripes is the number of stripes per layer. Default 16.
	Stripes uint32
	// BlobWeight, StripeWeight and MetaWeight set the kind mix. All zero
	// means 2:5:3.
	BlobWeight, StripeWeight, MetaWeight uint32
}

// KeyGen generates workload keys with Zipf-distributed popularity. The
// returned key slice is reused by the next call and must be copied if
// retained.
type KeyGen struct {
	rng   *rand.Rand
	zipf  *rand.Zipf
	cfg   WorkloadConfig
	total uint32
	buf   []byte
}

// NewKeyGen returns a generator whose draws come from rng.
func NewKeyGen(rng *rand.Rand, cfg WorkloadConfig) *KeyGen {
	if cfg.Keys == 0 {
		cfg.Keys = 100_000
	}

	if cfg.Skew <= 1 {
		cfg.Skew = 1.1
	}

	if cfg.Stripes == 0 {
		cfg.Stripes = 16
	}

	if cfg.BlobWeight+cfg.StripeWeight+cfg.MetaWeight == 0 {
		cfg.BlobWeight, cfg.StripeWeight, cfg.MetaWeight = 2, 5, 3
	}

	return &KeyGen{
		rng:   rng,
		zipf:  rand.NewZipf(rng, cfg.Skew, 1, cfg.Keys-1),
		cfg:   cfg,
		total: cfg.BlobWeight + cfg.StripeWeight + cfg.MetaWeight,
		buf:   make([]byte, 0, 64),
	}
}

// Next returns a random key, its kind, and the object's popularity rank
// (0 is the hottest).
func (g *KeyGen) Next() ([]byte, KeyKind, uint64) {
	rank := g.zipf.Uint64()

	w := g.rng.Uint32N(g.total)

	switch {
	case w < g.cfg.BlobWeight:
		return g.Blob(rank), KeyBlob, rank
	case w < g.cfg.BlobWeight+g.cfg.StripeWeight:
		return g.Stripe(rank, g.rng.Uint32N(g.cfg.Stripes)), KeyStripe, rank
	default:
		return g.Meta(rank), KeyMeta, rank
	}
}

func (g *KeyGen) digest(rank uint64) {
	g.buf = g.buf[:32]

	x := hashing.Mix64(rank ^ 0x9e3779b97f4a7c15)
	for i := 0; i < 32; i += 8 {
		binary.BigEndian.PutUint64(g.buf[i:], x)
		x = hashing.Mix64(x)
	}
}

// Blob returns the digest of blob rank.
func (g *KeyGen) Blob(rank uint64) []byte {
	g.digest(rank)

	return g.buf
}

// Stripe returns the key of stripe idx of layer rank.
func (g *KeyGen) Stripe(rank uint64, idx uint32) []byte {
	g.digest(rank)
	g.buf = binary.BigEndian.AppendUint32(g.buf, idx)

	return g.buf
}

// Meta returns the metadata key of object rank.
func (g *KeyGen) Meta(rank uint64) []byte {
	g.buf = append(g.buf[:0], "repo-"...)
	g.buf = strconv.AppendUint(g.buf, rank/8, 10)
	g.buf = append(g.buf, ":tag-"...)
	g.buf = strconv.AppendUint(g.buf, rank%8, 10)

	return g.buf
}
