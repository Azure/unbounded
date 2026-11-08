// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package hashing holds the hash functions every cachemesh member must agree
// on: key hashing, partition selection, member keys, and rendezvous scores.
//
// Everything here is a pure function of its inputs and is stable across
// processes, architectures, and releases. Changing any output is a wire
// format change: members running different versions would disagree on owners
// and routes. Nothing uses floating point or per-process seeds.
package hashing

import (
	"encoding/binary"

	"github.com/zeebo/xxh3"
)

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
