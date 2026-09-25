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
		slices.SortFunc(v.Members[i].Rails, func(a, b Rail) int { return cmp.Compare(a.Rail, b.Rail) })
	}

	slices.SortFunc(v.Members, func(a, b Member) int { return cmp.Compare(a.Node, b.Node) })
	slices.SortFunc(v.Caches, func(a, b CacheDefinition) int { return cmp.Compare(a.ID, b.ID) })

	return v
}

// CanonicalContent returns counter-free canonical JSON for durable version CAS.
// The membership document includes schema and cluster, and every member input.
// Input counters may be zero because callers hash candidates before assigning them.
func CanonicalContent(v Publication) (content, membership []byte, err error) {
	if err := validatePublication(v, false); err != nil {
		return nil, nil, err
	}

	v = canonicalPublication(v)
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
	p, m, err := CanonicalContent(v)
	if err != nil {
		return "", "", err
	}

	ph, mh := sha256.Sum256(p), sha256.Sum256(m)

	return hex.EncodeToString(ph[:]), hex.EncodeToString(mh[:]), nil
}
