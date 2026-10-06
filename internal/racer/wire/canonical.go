// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"reflect"
	"slices"
)

// EncodePublication sorts copies of the input collections, never caller state.
func EncodePublication(v Publication) ([]byte, error) {
	c, err := newCanonicalCandidate(v, true)
	if err != nil {
		return nil, err
	}

	return c.EncodePublication(v.Sequence, v.MembershipVersion)
}

func DecodePublication(r io.Reader) (Publication, error) {
	var v Publication
	if err := decode(r, MaxPublicationBytes, &v); err != nil {
		return Publication{}, err
	}

	if err := validatePublication(v, true); err != nil {
		return Publication{}, err
	}

	return canonicalPublication(v), nil
}

func canonicalPublication(v Publication) Publication {
	v.Members = append([]Member{}, v.Members...)

	v.Caches = append([]CacheDefinition{}, v.Caches...)
	for i := range v.Members {
		v.Members[i].RDMANICs = CanonicalRDMANICs(v.Members[i].RDMANICs)
	}

	slices.SortFunc(v.Members, func(a, b Member) int { return cmp.Compare(a.Node, b.Node) })
	slices.SortFunc(v.Caches, func(a, b CacheDefinition) int { return cmp.Compare(a.ID, b.ID) })

	return v
}

// CanonicalRDMANICs copies NICs, including nested pointers, in rail/device/port order.
func CanonicalRDMANICs(nics []RDMANIC) []RDMANIC {
	nics = append([]RDMANIC{}, nics...)
	for i := range nics {
		if n := nics[i].NUMANode; n != nil {
			value := *n
			nics[i].NUMANode = &value
		}
	}

	slices.SortFunc(nics, func(a, b RDMANIC) int {
		if n := cmp.Compare(a.Rail, b.Rail); n != 0 {
			return n
		}

		if n := cmp.Compare(a.Device, b.Device); n != 0 {
			return n
		}

		return cmp.Compare(a.Port, b.Port)
	})

	return nics
}

// CanonicalCandidate owns validated, sorted publication content, including nested
// pointers. Its private state can be reused for hashing and encoding after counter
// assignment without retaining caller-owned mutable state. The zero value is invalid.
type CanonicalCandidate struct {
	publication Publication
}

// NewCanonicalCandidate validates and copies content once. Input counters are
// ignored; final encoding checks the assigned counters and complete byte bound.
func NewCanonicalCandidate(v Publication) (CanonicalCandidate, error) {
	return newCanonicalCandidate(v, false)
}

func newCanonicalCandidate(v Publication, counters bool) (CanonicalCandidate, error) {
	if err := validatePublication(v, counters); err != nil {
		return CanonicalCandidate{}, err
	}

	return CanonicalCandidate{publication: canonicalPublication(v)}, nil
}

// EncodePublication encodes the candidate with nonzero counters. It does not
// mutate the candidate, and each call returns independently owned bytes.
func (c CanonicalCandidate) EncodePublication(sequence Sequence, membership MembershipVersion) ([]byte, error) {
	v := c.publication
	if err := validateHeader(v.SchemaVersion, v.Cluster); err != nil {
		return nil, err
	}

	if sequence == 0 || membership == 0 {
		return nil, InvalidRequest
	}

	v.Sequence, v.MembershipVersion = sequence, membership

	return encode(v, MaxPublicationBytes)
}

// canonicalContent returns counter-free canonical JSON for durable version CAS.
// The membership document includes schema and cluster, and every member input.
// Input counters may be zero because callers hash candidates before assigning them.
func (c CanonicalCandidate) canonicalContent() (content, membership []byte, err error) {
	v := c.publication
	if err := validateHeader(v.SchemaVersion, v.Cluster); err != nil {
		return nil, nil, err
	}

	m := struct {
		SchemaVersion uint32    `json:"schema_version"`
		Cluster       ClusterID `json:"cluster"`
		Members       []Member  `json:"members"`
	}{v.SchemaVersion, v.Cluster, v.Members}
	p := struct {
		SchemaVersion uint32            `json:"schema_version"`
		Cluster       ClusterID         `json:"cluster"`
		Members       []Member          `json:"members"`
		Caches        []CacheDefinition `json:"caches"`
	}{v.SchemaVersion, v.Cluster, v.Members, v.Caches}

	content, err = encode(p, MaxPublicationBytes)
	if err != nil {
		return nil, nil, err
	}

	membership, err = encode(m, MaxPublicationBytes)

	return content, membership, err
}

// ContentHashes returns lowercase SHA-256 hex, suitable for VersionRecord fields.
func ContentHashes(v Publication) (content, membership string, err error) {
	c, err := NewCanonicalCandidate(v)
	if err != nil {
		return "", "", err
	}

	return c.ContentHashes()
}

// ContentHashes returns the same counter-free hashes as ContentHashes without
// repeating validation, copying, or sorting of the candidate's collections.
func (c CanonicalCandidate) ContentHashes() (content, membership string, err error) {
	p, m, err := c.canonicalContent()
	if err != nil {
		return "", "", err
	}

	ph, mh := sha256.Sum256(p), sha256.Sum256(m)

	return hex.EncodeToString(ph[:]), hex.EncodeToString(mh[:]), nil
}

const (
	DeltaHeader   = "X-Racer-Delta-Base"
	MaxDeltaBytes = 4 * 1024 * 1024
)

// Delta is transported only on the authenticated snapshot endpoint. Hashes use
// ContentHashes, not a second canonical representation or placement authority.
type Delta struct {
	DeltaVersion      uint32            `json:"delta_version"`
	Cluster           ClusterID         `json:"cluster"`
	BaseSequence      Sequence          `json:"base_sequence,string"`
	BaseHash          string            `json:"base_hash"`
	Sequence          Sequence          `json:"sequence,string"`
	MembershipVersion MembershipVersion `json:"membership_version,string"`
	ContentHash       string            `json:"content_hash"`
	UpsertMembers     []Member          `json:"upsert_members"`
	RemoveMembers     []NodeID          `json:"remove_members"`
	Caches            []CacheDefinition `json:"caches"`
}

func EncodeDelta(base, next Publication) ([]byte, error) {
	b, err := newCanonicalCandidate(base, true)
	if err != nil {
		return nil, err
	}

	n, err := newCanonicalCandidate(next, true)
	if err != nil {
		return nil, err
	}

	if base.Cluster != next.Cluster || next.Sequence <= base.Sequence {
		return nil, Conflict
	}

	base, next = b.publication, n.publication

	bh, _, err := b.ContentHashes()
	if err != nil {
		return nil, err
	}

	nh, _, err := n.ContentHashes()
	if err != nil {
		return nil, err
	}

	d := Delta{DeltaVersion: 1, Cluster: next.Cluster, BaseSequence: base.Sequence, BaseHash: bh, Sequence: next.Sequence, MembershipVersion: next.MembershipVersion, ContentHash: nh, UpsertMembers: []Member{}, RemoveMembers: []NodeID{}, Caches: next.Caches}

	old := make(map[NodeID]Member, len(base.Members))
	for _, m := range base.Members {
		old[m.Node] = m
	}

	for _, m := range next.Members {
		if previous, ok := old[m.Node]; !ok || !reflect.DeepEqual(previous, m) {
			d.UpsertMembers = append(d.UpsertMembers, m)
		}

		delete(old, m.Node)
	}

	for _, m := range base.Members {
		if _, ok := old[m.Node]; ok {
			d.RemoveMembers = append(d.RemoveMembers, m.Node)
		}
	}

	return encode(d, MaxDeltaBytes)
}

func ApplyDelta(base Publication, reader io.Reader) (Publication, error) {
	var d Delta
	if err := decode(reader, MaxDeltaBytes, &d); err != nil {
		return Publication{}, err
	}

	hash, _, err := ContentHashes(base)
	if err != nil {
		return Publication{}, err
	}

	if d.DeltaVersion != 1 || d.Cluster != base.Cluster || d.BaseSequence != base.Sequence || d.BaseHash != hash || d.Sequence <= base.Sequence || d.MembershipVersion < base.MembershipVersion {
		return Publication{}, Conflict
	}

	members := make(map[NodeID]Member, len(base.Members))
	for _, m := range base.Members {
		members[m.Node] = m
	}

	seen := make(map[NodeID]bool)
	for _, id := range d.RemoveMembers {
		if _, ok := members[id]; !ok || seen[id] {
			return Publication{}, InvalidRequest
		}

		seen[id] = true
		delete(members, id)
	}

	for _, m := range d.UpsertMembers {
		if seen[m.Node] {
			return Publication{}, InvalidRequest
		}

		seen[m.Node] = true
		members[m.Node] = m
	}

	next := Publication{SchemaVersion: SchemaVersion, Cluster: d.Cluster, Sequence: d.Sequence, MembershipVersion: d.MembershipVersion, Caches: d.Caches, Members: make([]Member, 0, len(members))}
	for _, m := range members {
		next.Members = append(next.Members, m)
	}

	candidate, err := newCanonicalCandidate(next, true)
	if err != nil {
		return Publication{}, err
	}

	hash, _, err = candidate.ContentHashes()
	if err != nil {
		return Publication{}, err
	}

	if hash != d.ContentHash {
		return Publication{}, Conflict
	}

	return candidate.publication, nil
}
