// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"errors"
	"strings"
	"testing"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestPrepareCanonicalEquivalence(t *testing.T) {
	numa := uint32(3)
	members := AcceptedMembers{
		testNodeUID:  {Node: testNodeUID, Shares: 4, PeerEndpoint: "192.0.2.1:7443", RDMANICs: []wire.RDMANIC{{Rail: 2, Device: "β<&>", Port: 1, NUMANode: &numa}, {Rail: 1, Device: "mlx5_0", Port: 1}}},
		testOtherUID: {Node: testOtherUID, Shares: 1, PeerEndpoint: "[2001:db8::1]:7443"},
	}
	caches := []wire.CacheDefinition{{ID: testNodeUID, Name: "cache", ClientSocket: "/run/racer/cache/client/socket", OriginSocket: "/run/racer/cache/origin/socket"}}
	v := wire.Publication{SchemaVersion: wire.SchemaVersion, Cluster: testNodeUID}

	content, membership, err := wire.ContentHashes(v)
	if err != nil {
		t.Fatal(err)
	}

	previous := VersionRecord{Cluster: v.Cluster, Sequence: 1, MembershipVersion: 1, ContentHash: content, MembershipHash: membership}
	p := NewPublications()

	for _, tc := range []struct {
		name       string
		members    AcceptedMembers
		caches     []wire.CacheDefinition
		sequence   wire.Sequence
		membership wire.MembershipVersion
	}{
		{"unchanged empty", nil, nil, 1, 1},
		{"cache only", nil, caches, 2, 1},
		{"membership", members, caches, 3, 2},
		{"unchanged populated", members, caches, 3, 2},
		{"remove cache", members, nil, 4, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prepared, err := p.Prepare(previous, "rv", tc.members, tc.caches)
			if err != nil {
				t.Fatal(err)
			}

			v.Sequence, v.MembershipVersion, v.Caches = tc.sequence, tc.membership, tc.caches

			v.Members = nil
			for _, member := range tc.members {
				v.Members = append(v.Members, member)
			}

			want, err := wire.EncodePublication(v)
			if err != nil {
				t.Fatal(err)
			}

			content, membership, err := wire.ContentHashes(v)
			if err != nil {
				t.Fatal(err)
			}

			wantRecord := VersionRecord{Cluster: v.Cluster, Sequence: tc.sequence, MembershipVersion: tc.membership, ContentHash: content, MembershipHash: membership}
			if prepared.record != wantRecord || prepared.encoded != string(want) || prepared.previous != previous || prepared.resourceVersion != "rv" || prepared.owner != p {
				t.Fatal("prepared bytes, hashes, counters, or commit metadata differ")
			}

			previous = prepared.record
		})
	}
}

func TestPrepareRejectsFinalCounterGrowth(t *testing.T) {
	v := wire.Publication{
		SchemaVersion: wire.SchemaVersion, Cluster: testNodeUID, Sequence: 9, MembershipVersion: 9,
		Members: []wire.Member{{Node: testNodeUID, Shares: 1, PeerEndpoint: "192.0.2.1:1", RDMANICs: []wire.RDMANIC{{Device: "x", Port: 1}}}},
	}

	b, err := wire.EncodePublication(v)
	if err != nil {
		t.Fatal(err)
	}

	v.Members[0].RDMANICs[0].Device += strings.Repeat("x", wire.MaxPublicationBytes-len(b))

	content, membership, err := wire.ContentHashes(v)
	if err != nil {
		t.Fatal(err)
	}

	previous := VersionRecord{Cluster: v.Cluster, Sequence: 9, MembershipVersion: 9, ContentHash: content, MembershipHash: membership}
	members := AcceptedMembers{testNodeUID: v.Members[0]}
	p := NewPublications()

	prepared, err := p.Prepare(previous, "rv", members, nil)
	if err != nil || len(prepared.encoded) != wire.MaxPublicationBytes {
		t.Fatalf("exact final bound: %v", err)
	}

	// Same-width input change grows both assigned counters from 9 to 10. The
	// counter-free hash still fits, but the final publication must be rejected.
	m := members[testNodeUID]
	m.Shares++

	members[testNodeUID] = m
	if prepared, err := p.Prepare(previous, "rv", members, nil); !errors.Is(err, wire.TooLarge) || prepared != nil {
		t.Fatalf("oversized final encoding accepted: %v", err)
	}
}
