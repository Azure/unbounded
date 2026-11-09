// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Partition table.
//
// A cluster hashes every key into P = 2^bits partitions. The Table maps each
// partition to an Entry naming its owner slot, the previous owner (for
// read-through handoff), and the epoch of the last move. Tables are immutable
// and stored in 1024-entry chunks; Assign returns a new table that shares
// every chunk it did not touch, so a delta moving k partitions copies at most
// k chunks and every view keeps its own table cheaply. Get is a two-level
// array lookup with no locks and no allocation; entries are stored as
// per-chunk columns so that the owner lookup reads a dense array.
package view

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	"github.com/zeebo/xxh3"
)

// MaxWeight is the full weight of an active, warmed-up member.
const MaxWeight = 1000

// NoMember marks a partition without an owner or an Entry without a previous
// owner. It equals the cachemesh NoSlot value, ^uint32(0).
const NoMember = ^uint32(0)

// Bit bounds for the partition count.
const (
	MinPartitionBits     = 1
	MaxPartitionBits     = 24
	DefaultPartitionBits = 20
)

const (
	chunkShift = 10
	// ChunkSize is the number of entries per copy-on-write chunk.
	ChunkSize = 1 << chunkShift
	chunkMask = ChunkSize - 1
)

// Entry is the placement of one partition.
type Entry struct {
	// Owner is the slot of the owning member, or NoMember.
	Owner uint32
	// Prev is the owner before the last move, or NoMember. It may name a
	// member that has since been removed; consumers must ignore a Prev that
	// is not a current member.
	Prev uint32
	// Moved is the epoch of the last move, truncated to 32 bits.
	Moved uint32
}

// Table is an immutable partition table. A nil *Table is not valid.
//
// Each chunk stores its entries as three columns (owners, then previous
// owners, then move epochs) rather than as an array of Entry, so the owner
// lookup that every routed request makes reads a dense 4-byte array: the
// owners of 2^20 partitions take 4 MiB instead of the 12 MiB of the whole
// table, which keeps far more of them in cache.
type Table struct {
	bits   uint8
	chunks []*chunk
}

// chunk holds the columns of up to ChunkSize partitions in one allocation.
type chunk struct {
	owner, prev, moved []uint32
}

func newChunk(size int) *chunk {
	buf := make([]uint32, 3*size)

	return &chunk{
		owner: buf[:size:size],
		prev:  buf[size : 2*size : 2*size],
		moved: buf[2*size:],
	}
}

func (c *chunk) set(j uint32, e Entry) {
	c.owner[j], c.prev[j], c.moved[j] = e.Owner, e.Prev, e.Moved
}

func (c *chunk) clone() *chunk {
	n := newChunk(len(c.owner))
	copy(n.owner, c.owner)
	copy(n.prev, c.prev)
	copy(n.moved, c.moved)

	return n
}

func (c *chunk) equal(o *chunk) bool {
	return slices.Equal(c.owner, o.owner) && slices.Equal(c.prev, o.prev) && slices.Equal(c.moved, o.moved)
}

// ErrInvalidTable reports invalid arguments to NewTable or Assign.
var ErrInvalidTable = errors.New("view: invalid argument")

// NewTable returns a table of 2^bits partitions all owned by owner (which may
// be NoMember) with Prev NoMember and Moved 0. It panics if bits is outside
// MinBits..MaxBits; use ValidBits to check untrusted input first.
func NewTable(bits uint8, owner uint32) *Table {
	if !ValidPartitionBits(bits) {
		panic(fmt.Sprintf("view: bits %d outside %d..%d", bits, MinPartitionBits, MaxPartitionBits))
	}

	n := 1 << bits
	size := min(n, ChunkSize)

	// Every chunk of a fresh table has the same contents, so share one.
	c := newChunk(size)
	for j := range uint32(size) {
		c.set(j, Entry{Owner: owner, Prev: NoMember})
	}

	t := &Table{bits: bits, chunks: make([]*chunk, (n+size-1)/size)}
	for i := range t.chunks {
		t.chunks[i] = c
	}

	return t
}

// ValidPartitionBits reports whether bits is an allowed partition bit count.
func ValidPartitionBits(bits uint8) bool { return bits >= MinPartitionBits && bits <= MaxPartitionBits }

// Bits returns the partition bit count.
func (t *Table) Bits() uint8 { return t.bits }

// Len returns the number of partitions, 2^Bits().
func (t *Table) Len() int { return 1 << t.bits }

// Get returns the entry of partition p. p must be below Len().
func (t *Table) Get(p uint32) Entry {
	c, j := t.chunks[p>>chunkShift], p&chunkMask

	return Entry{Owner: c.owner[j], Prev: c.prev[j], Moved: c.moved[j]}
}

// Owner returns the owner of partition p. p must be below Len(). It reads
// only the owner column.
func (t *Table) Owner(p uint32) uint32 {
	return t.chunks[p>>chunkShift].owner[p&chunkMask]
}

// NumChunks returns the number of chunks.
func (t *Table) NumChunks() int { return len(t.chunks) }

// Columns returns the owner, previous owner, and move epoch columns of chunk
// i, which holds partitions i*ChunkSize onward. The slices are shared and
// must not be modified.
func (t *Table) Columns(i int) (owner, prev, moved []uint32) {
	c := t.chunks[i]

	return c.owner, c.prev, c.moved
}

// SameChunk reports whether a and b share the storage of chunk i, which
// tests use to check copy-on-write sharing.
func SameChunk(a, b *Table, i int) bool {
	return a.chunks[i] == b.chunks[i]
}

// ForEach calls fn for every partition in increasing order.
func (t *Table) ForEach(fn func(p uint32, e Entry)) {
	for ci, c := range t.chunks {
		base := uint32(ci) << chunkShift
		for j := range c.owner {
			fn(base+uint32(j), Entry{Owner: c.owner[j], Prev: c.prev[j], Moved: c.moved[j]})
		}
	}
}

// Assign returns a table with partition parts[i] owned by owners[i]. Entries
// whose owner changes get Prev = old owner and Moved = epoch; entries already
// owned by the requested owner are left untouched (Prev and Moved keep their
// values). Untouched chunks are shared with t, which is not modified.
//
// Assign rejects mismatched lengths, partitions out of range, NoMember
// owners, and duplicate partitions: a duplicate would make Prev ambiguous, so
// a delta must name each partition at most once.
func (t *Table) Assign(parts, owners []uint32, epoch uint32) (*Table, error) {
	if len(parts) != len(owners) {
		return nil, fmt.Errorf("%w: %d parts, %d owners", ErrInvalidTable, len(parts), len(owners))
	}

	n := uint32(t.Len())
	for i, p := range parts {
		if p >= n {
			return nil, fmt.Errorf("%w: partition %d >= %d", ErrInvalidTable, p, n)
		}

		if owners[i] == NoMember {
			return nil, fmt.Errorf("%w: partition %d assigned to NoMember", ErrInvalidTable, p)
		}
	}

	if len(parts) > 1 {
		s := slices.Clone(parts)
		slices.Sort(s)

		for i := 1; i < len(s); i++ {
			if s[i] == s[i-1] {
				return nil, fmt.Errorf("%w: duplicate partition %d", ErrInvalidTable, s[i])
			}
		}
	}

	nt := &Table{bits: t.bits, chunks: slices.Clone(t.chunks)}

	var copied []bool

	for i, p := range parts {
		ci, j := p>>chunkShift, p&chunkMask

		old := nt.chunks[ci].owner[j]
		if old == owners[i] {
			continue
		}

		if copied == nil {
			copied = make([]bool, len(nt.chunks))
		}

		if !copied[ci] {
			copied[ci] = true
			nt.chunks[ci] = nt.chunks[ci].clone()
		}

		nt.chunks[ci].set(j, Entry{Owner: owners[i], Prev: old, Moved: epoch})
	}

	if copied == nil {
		return t, nil
	}

	return nt, nil
}

// EqualTables reports whether a and b have the same bits and entries. Shared
// chunks are compared by identity.
func EqualTables(a, b *Table) bool {
	if a.bits != b.bits {
		return false
	}

	for i := range a.chunks {
		if SameChunk(a, b, i) {
			continue
		}

		if !a.chunks[i].equal(b.chunks[i]) {
			return false
		}
	}

	return true
}

// Hash returns a 64-bit content hash of the table (bits and every entry).
// Equal tables have equal hashes on every platform.
func (t *Table) Hash() uint64 {
	h := Mix64(uint64(t.bits) + 0x9e3779b97f4a7c15)
	for _, c := range t.chunks {
		for j := range c.owner {
			h = Mix64(h ^ (uint64(c.owner[j]) | uint64(c.prev[j])<<32))
			h = Mix64(h ^ uint64(c.moved[j]))
		}
	}

	return h
}

// Loads writes per-owner partition counts into dst, indexed by owner slot,
// growing it as needed and zeroing it first. Partitions owned by NoMember are
// not counted.
func (t *Table) Loads(dst []uint32) []uint32 {
	clear(dst[:cap(dst)])

	for _, c := range t.chunks {
		for _, o := range c.owner {
			if o == NoMember {
				continue
			}

			if int(o) >= len(dst) {
				dst = slices.Grow(dst, int(o)+1-len(dst))[:int(o)+1]
			}

			dst[o]++
		}
	}

	return dst
}

// NewTableFunc returns a table of 2^bits partitions whose entry p is fn(p),
// called once per partition in increasing order. Consecutive chunks with
// identical contents share storage, so a uniform table costs one chunk. It
// panics if bits is outside MinBits..MaxBits.
func NewTableFunc(bits uint8, fn func(p uint32) Entry) *Table {
	if !ValidPartitionBits(bits) {
		panic(fmt.Sprintf("view: bits %d outside %d..%d", bits, MinPartitionBits, MaxPartitionBits))
	}

	n := 1 << bits
	size := min(n, ChunkSize)
	t := &Table{bits: bits, chunks: make([]*chunk, (n+size-1)/size)}

	var prev, cur *chunk

	for ci := range t.chunks {
		if cur == nil {
			cur = newChunk(size)
		}

		base := uint32(ci) * uint32(size)
		for j := range uint32(size) {
			cur.set(j, fn(base+j))
		}

		if prev != nil && prev.equal(cur) {
			t.chunks[ci] = prev

			continue
		}

		t.chunks[ci] = cur
		prev, cur = cur, nil
	}

	return t
}

// Hashing. These are the hash functions every cachemesh member must agree
// on: key hashing, partition selection, member keys, and rendezvous scores.
//
// Everything here is a pure function of its inputs and is stable across
// processes, architectures, and releases. Changing any output is a wire
// format change: members running different versions would disagree on owners
// and routes. Nothing uses floating point or per-process seeds.

// KeyHash hashes key under a keyspace seed.
func KeyHash(seed uint64, key []byte) uint64 {
	return xxh3.HashSeed(key, seed)
}

// KeyHashString hashes key under a keyspace seed without converting to bytes.
func KeyHashString(seed uint64, key string) uint64 {
	return xxh3.HashStringSeed(key, seed)
}

// Seed derives a keyspace seed from its name.
func Seed(name string) uint64 {
	return Mix64(xxh3.HashString("cachemesh/keyspace/v1/" + name))
}

// MemberKey derives the rendezvous key of a member incarnation. Scores use
// this key, never slot numbers, so they survive slot reuse.
func MemberKey(id string, incarnation uint64) uint64 {
	const prefix = "cachemesh/member/v1/"

	buf := make([]byte, 0, len(prefix)+len(id)+9)
	buf = append(buf, prefix...)
	buf = append(buf, id...)
	buf = append(buf, 0)
	buf = binary.LittleEndian.AppendUint64(buf, incarnation)

	return Mix64(xxh3.Hash(buf))
}

// Partition returns the top bits of h, a partition index in [0, 2^bits).
// bits must be in 1..32.
func Partition(h uint64, bits uint8) uint32 {
	return uint32(h >> (64 - uint64(bits)))
}

// Mix64 is the SplitMix64 finalizer, a bijection on uint64.
func Mix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31

	return x
}

// Score is the rendezvous score of member key a for item b. Larger wins;
// callers break ties with Better.
func Score(a, b uint64) uint64 {
	return Mix64(a ^ Mix64(b))
}

// Better reports whether (scoreA, keyA) ranks above (scoreB, keyB): the larger
// score wins, and equal scores go to the larger key.
func Better(scoreA, keyA, scoreB, keyB uint64) bool {
	if scoreA != scoreB {
		return scoreA > scoreB
	}

	return keyA > keyB
}

// Below reports whether Score(a, b) falls under probability p expressed as a
// fraction of 2^64 (p64 = p * 2^64, saturating at MaxUint64 for p >= 1).
func Below(score, p64 uint64) bool {
	return p64 == ^uint64(0) || score < p64
}
