// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestDeltaAddRemoveUpdateAndRejectedBase(t *testing.T) {
	base := Publication{
		SchemaVersion: 1, Cluster: "11111111-1111-4111-8111-111111111111", Sequence: 1, MembershipVersion: 1,
		Members: []Member{
			{Node: "22222222-2222-4222-8222-222222222222", Shares: 4, PeerEndpoint: "127.0.0.1:7443", Rails: []Rail{}, AlignmentEnabled: true},
			{Node: "33333333-3333-4333-8333-333333333333", Shares: 4, PeerEndpoint: "127.0.0.2:7443", Rails: []Rail{}, AlignmentEnabled: true},
		}, Caches: []CacheDefinition{},
	}
	next := base
	next.Sequence, next.MembershipVersion = 2, 2
	next.Members = []Member{base.Members[0], {Node: "44444444-4444-4444-8444-444444444444", Shares: 7, PeerEndpoint: "127.0.0.4:7443", Rails: []Rail{}, AlignmentEnabled: true}}
	next.Members[0].Shares = 9

	encoded, err := EncodeDelta(base, next)
	if err != nil {
		t.Fatal(err)
	}

	golden, err := os.ReadFile("testdata/delta.json")
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(encoded, bytes.TrimSpace(golden)) {
		t.Fatalf("cross-language delta changed: %s", encoded)
	}

	got, err := ApplyDelta(base, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}

	a, _ := EncodePublication(got)

	b, _ := EncodePublication(next)
	if !bytes.Equal(a, b) {
		t.Fatalf("delta mismatch: %s", a)
	}

	if _, err := ApplyDelta(next, bytes.NewReader(encoded)); err == nil {
		t.Fatal("replay accepted")
	}

	bad := strings.Replace(string(encoded), `"shares":9`, `"shares":8`, 1)
	if _, err := ApplyDelta(base, strings.NewReader(bad)); err == nil {
		t.Fatal("bad target hash accepted")
	}

	wrong := base

	wrong.Sequence = 3
	if _, err := ApplyDelta(wrong, bytes.NewReader(encoded)); err == nil {
		t.Fatal("wrong base accepted")
	}
}
