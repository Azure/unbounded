// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestCanonicalCandidateEquivalence(t *testing.T) {
	vector, err := DecodePublication(bytes.NewReader(fixture(t, "publication.json")))
	if err != nil {
		t.Fatal(err)
	}

	vector.Caches = append(vector.Caches, CacheDefinition{ID: "00000000-0000-4000-8000-000000000000", Name: "cache-b", ClientSocket: "/run/racer/cache-b/client/socket", OriginSocket: "/run/racer/cache-b/origin/socket"})
	slices.Reverse(vector.Members)

	for _, v := range []Publication{
		vector,
		{SchemaVersion: SchemaVersion, Cluster: vector.Cluster},
		{SchemaVersion: SchemaVersion, Cluster: vector.Cluster, Members: []Member{}, Caches: []CacheDefinition{}},
	} {
		before, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}

		candidate, err := NewCanonicalCandidate(v)
		if err != nil {
			t.Fatal(err)
		}

		wantContent, wantMembership, err := ContentHashes(v)
		if err != nil {
			t.Fatal(err)
		}

		sequence, membershipVersion := v.Sequence, v.MembershipVersion
		for _, counters := range [][2]uint64{{1, 1}, {10, 9}, {1, 2}, {^uint64(0), ^uint64(0)}} {
			v.Sequence, v.MembershipVersion = Sequence(counters[0]), MembershipVersion(counters[1])

			want, err := EncodePublication(v)
			if err != nil {
				t.Fatal(err)
			}

			got, err := candidate.EncodePublication(v.Sequence, v.MembershipVersion)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("encoding differs for counters %v: %v", counters, err)
			}

			content, membership, err := candidate.ContentHashes()
			if err != nil || content != wantContent || membership != wantMembership {
				t.Fatalf("hashes changed after assigning counters: %v", err)
			}
		}

		v.Sequence, v.MembershipVersion = sequence, membershipVersion

		after, err := json.Marshal(v)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("candidate mutated its input: %v", err)
		}
	}
}

func TestCanonicalCandidateOwnsNestedStateAndOutput(t *testing.T) {
	v, err := DecodePublication(bytes.NewReader(fixture(t, "publication.json")))
	if err != nil {
		t.Fatal(err)
	}

	want, err := EncodePublication(v)
	if err != nil {
		t.Fatal(err)
	}

	candidate, err := NewCanonicalCandidate(v)
	if err != nil {
		t.Fatal(err)
	}

	// Mutate all caller-owned collections and the pointer nested in a rail before
	// the candidate is first hashed or encoded.
	*v.Members[1].Rails[1].NUMANode = 7
	v.Members[1].Rails[0].Fabric = "changed"
	v.Members[0].Shares = 0
	v.Caches[0].Name = "changed"
	clear(v.Members)
	clear(v.Caches)

	got, err := candidate.EncodePublication(v.Sequence, v.MembershipVersion)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("candidate retained caller state: %v", err)
	}

	var hashes struct{ Content, Membership string }
	if err := json.Unmarshal(fixture(t, "hashes.json"), &hashes); err != nil {
		t.Fatal(err)
	}

	content, membership, err := candidate.ContentHashes()
	if err != nil || content != hashes.Content || membership != hashes.Membership {
		t.Fatalf("candidate hashes differ from shared vectors: %v", err)
	}

	clear(got)

	got, err = candidate.EncodePublication(v.Sequence, v.MembershipVersion)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("encoding retained returned bytes: %v", err)
	}
}

func TestCanonicalCandidateValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Publication)
		want error
	}{
		{"schema", func(v *Publication) { v.SchemaVersion++ }, UnsupportedVersion},
		{"cluster", func(v *Publication) { v.Cluster = "invalid" }, InvalidRequest},
		{"node", func(v *Publication) { v.Members[0].Node = "invalid" }, InvalidRequest},
		{"duplicate node", func(v *Publication) { v.Members = append(v.Members, v.Members[0]) }, InvalidRequest},
		{"shares", func(v *Publication) { v.Members[0].Shares = 0 }, InvalidRequest},
		{"endpoint", func(v *Publication) { v.Members[0].PeerEndpoint = "192.0.2.1:0" }, InvalidRequest},
		{"duplicate rail", func(v *Publication) { v.Members[1].Rails = append(v.Members[1].Rails, v.Members[1].Rails[0]) }, InvalidRequest},
		{"fabric", func(v *Publication) { v.Members[1].Rails[0].Fabric = "\xff" }, InvalidRequest},
		{"duplicate cache", func(v *Publication) { v.Caches = append(v.Caches, v.Caches[0]) }, InvalidRequest},
		{"socket", func(v *Publication) { v.Caches[0].ClientSocket += "x" }, InvalidRequest},
		{"member limit", func(v *Publication) { v.Members = make([]Member, MaxMembers+1) }, TooLarge},
		{"byte lower bound", func(v *Publication) { v.Members[1].Rails[0].Fabric = strings.Repeat("x", MaxPublicationBytes) }, TooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := DecodePublication(bytes.NewReader(fixture(t, "publication.json")))
			if err != nil {
				t.Fatal(err)
			}

			tc.edit(&v)

			candidate, err := NewCanonicalCandidate(v)
			if !errors.Is(err, tc.want) || !reflect.DeepEqual(candidate, CanonicalCandidate{}) {
				t.Fatalf("candidate on invalid input: %+v, %v", candidate, err)
			}

			if _, err := EncodePublication(v); !errors.Is(err, tc.want) {
				t.Fatalf("codec validation differs: %v", err)
			}
		})
	}

	var zero CanonicalCandidate
	if p, m, err := zero.ContentHashes(); !errors.Is(err, UnsupportedVersion) || p != "" || m != "" {
		t.Fatalf("zero candidate hashes: %q, %q, %v", p, m, err)
	}

	if b, err := zero.EncodePublication(1, 1); !errors.Is(err, UnsupportedVersion) || b != nil {
		t.Fatalf("zero candidate encoding: %v", err)
	}

	candidate, err := NewCanonicalCandidate(Publication{SchemaVersion: SchemaVersion, Cluster: "11111111-1111-4111-8111-111111111111"})
	if err != nil {
		t.Fatal(err)
	}

	for _, counters := range [][2]uint64{{0, 0}, {0, 1}, {1, 0}} {
		if b, err := candidate.EncodePublication(Sequence(counters[0]), MembershipVersion(counters[1])); !errors.Is(err, InvalidRequest) || b != nil {
			t.Fatalf("zero counters %v: %v", counters, err)
		}
	}
}

func TestCanonicalCandidateFinalByteBound(t *testing.T) {
	v := Publication{
		SchemaVersion: SchemaVersion, Cluster: "11111111-1111-4111-8111-111111111111", Sequence: 1, MembershipVersion: 1,
		Members: []Member{{Node: "22222222-2222-4222-8222-222222222222", Shares: 1, PeerEndpoint: "192.0.2.1:1", Rails: []Rail{{Fabric: "x"}}}},
	}

	b, err := EncodePublication(v)
	if err != nil {
		t.Fatal(err)
	}

	// Escaping expands tabs to two bytes while staying below the cheap input bound.
	padding := MaxPublicationBytes - len(b)
	v.Members[0].Rails[0].Fabric += strings.Repeat("\t", padding/2) + strings.Repeat("x", padding%2)

	candidate, err := NewCanonicalCandidate(v)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := candidate.ContentHashes(); err != nil {
		t.Fatalf("counter-free content fits: %v", err)
	}

	b, err = candidate.EncodePublication(1, 1)
	if err != nil || len(b) != MaxPublicationBytes {
		t.Fatalf("exact final byte bound: %d, %v", len(b), err)
	}

	for _, counters := range [][2]uint64{{10, 1}, {1, 10}, {^uint64(0), ^uint64(0)}} {
		if b, err := candidate.EncodePublication(Sequence(counters[0]), MembershipVersion(counters[1])); !errors.Is(err, TooLarge) || b != nil {
			t.Fatalf("counter growth exceeded final byte bound %v: %v", counters, err)
		}
	}

	v.Members[0].Rails[0].Fabric += strings.Repeat("\t", 100)

	candidate, err = NewCanonicalCandidate(v)
	if err != nil {
		t.Fatal(err)
	}

	if p, m, err := candidate.ContentHashes(); !errors.Is(err, TooLarge) || p != "" || m != "" {
		t.Fatalf("escaped counter-free byte bound: %v", err)
	}
}
