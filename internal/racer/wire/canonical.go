// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"slices"
)

func canonicalPublication(v Publication) Publication {
	v.Members = append([]Member{}, v.Members...)

	v.Caches = append([]CacheDefinition{}, v.Caches...)
	for i := range v.Members {
		v.Members[i].Rails = append([]Rail{}, v.Members[i].Rails...)
		for j := range v.Members[i].Rails {
			if n := v.Members[i].Rails[j].NUMANode; n != nil {
				value := *n
				v.Members[i].Rails[j].NUMANode = &value
			}
		}

		slices.SortFunc(v.Members[i].Rails, func(a, b Rail) int { return cmp.Compare(a.Rail, b.Rail) })
	}

	slices.SortFunc(v.Members, func(a, b Member) int { return cmp.Compare(a.Node, b.Node) })
	slices.SortFunc(v.Caches, func(a, b CacheDefinition) int { return cmp.Compare(a.ID, b.ID) })

	return v
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
	if err := validatePublication(v, false); err != nil {
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
