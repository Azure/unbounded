// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"io"
	"reflect"
)

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
	if err := validatePublication(base, true); err != nil {
		return nil, err
	}

	if err := validatePublication(next, true); err != nil {
		return nil, err
	}

	if base.Cluster != next.Cluster || next.Sequence <= base.Sequence {
		return nil, Conflict
	}

	base = canonicalPublication(base)
	next = canonicalPublication(next)

	bh, _, err := ContentHashes(base)
	if err != nil {
		return nil, err
	}

	nh, _, err := ContentHashes(next)
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

	if err := validatePublication(next, true); err != nil {
		return Publication{}, err
	}

	hash, _, err = ContentHashes(next)
	if err != nil {
		return Publication{}, err
	}

	if hash != d.ContentHash {
		return Publication{}, Conflict
	}

	return canonicalPublication(next), nil
}
