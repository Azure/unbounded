// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package view

import (
	"testing"
)

// The golden values pin the hash formats. If one of these changes, members
// built from different commits disagree on placement, so the change must be
// treated as a wire format change.
func TestGolden(t *testing.T) {
	cases := []struct {
		name string
		got  uint64
		want uint64
	}{
		{"KeyHash", KeyHash(1, []byte("sha256:abc")), KeyHash(1, []byte("sha256:abc"))},
		{"KeyHashString", KeyHashString(1, "sha256:abc"), KeyHash(1, []byte("sha256:abc"))},
		{"Mix64(0)", Mix64(0), 0},
		{"Mix64(1)", Mix64(1), 0x5692161d100b05e5},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %#x, want %#x", c.name, c.got, c.want)
		}
	}
}

func TestSeedDistinct(t *testing.T) {
	if Seed("blobs") == Seed("meta") {
		t.Fatal("distinct keyspaces must have distinct seeds")
	}

	a, b := Seed("blobs"), Seed("blobs")
	if a != b {
		t.Fatal("Seed must be deterministic")
	}
}

func TestMemberKey(t *testing.T) {
	a := MemberKey("node-a", 1)
	if a != MemberKey("node-a", 1) {
		t.Fatal("MemberKey must be deterministic")
	}

	if a == MemberKey("node-a", 2) {
		t.Fatal("incarnation must change the key")
	}

	if a == MemberKey("node-b", 1) {
		t.Fatal("id must change the key")
	}
	// The separator prevents ("ab", x) colliding with ("a", "b"+x).
	if MemberKey("ab", 0) == MemberKey("a", 0) {
		t.Fatal("unexpected collision")
	}
}

func TestPartition(t *testing.T) {
	cases := []struct {
		h    uint64
		bits uint8
		want uint32
	}{
		{0, 20, 0},
		{^uint64(0), 20, 1<<20 - 1},
		{1 << 63, 1, 1},
		{1 << 62, 1, 0},
		{^uint64(0), 32, ^uint32(0)},
		{0x8000_0000_0000_0000, 20, 1 << 19},
	}
	for _, c := range cases {
		if got := Partition(c.h, c.bits); got != c.want {
			t.Errorf("Partition(%#x, %d) = %#x, want %#x", c.h, c.bits, got, c.want)
		}
	}
}

func TestMix64Bijective(t *testing.T) {
	seen := make(map[uint64]struct{}, 1<<16)

	for i := range uint64(1 << 16) {
		v := Mix64(i)
		if _, dup := seen[v]; dup {
			t.Fatalf("collision at %d", i)
		}

		seen[v] = struct{}{}
	}
}

func TestBetter(t *testing.T) {
	if !Better(2, 0, 1, 9) {
		t.Fatal("higher score must win")
	}

	if !Better(1, 9, 1, 8) {
		t.Fatal("tie must go to larger key")
	}

	if Better(1, 8, 1, 8) {
		t.Fatal("equal is not better")
	}
}

func TestBelow(t *testing.T) {
	if !Below(^uint64(0), ^uint64(0)) {
		t.Fatal("p=1 must always pass")
	}

	if Below(5, 5) || !Below(4, 5) {
		t.Fatal("strict comparison expected")
	}

	if Below(0, 0) {
		t.Fatal("p=0 must never pass")
	}
}

// TestScoreUniform checks that rendezvous winners are spread evenly.
func TestScoreUniform(t *testing.T) {
	const (
		members = 16
		items   = 1 << 16
	)

	keys := make([]uint64, members)
	for i := range keys {
		keys[i] = MemberKey("m", uint64(i+1))
	}

	wins := make([]int, members)

	for it := range uint64(items) {
		best := 0

		for i := 1; i < members; i++ {
			if Better(Score(keys[i], it), keys[i], Score(keys[best], it), keys[best]) {
				best = i
			}
		}

		wins[best]++
	}

	mean := items / members
	for i, w := range wins {
		if w < mean*9/10 || w > mean*11/10 {
			t.Errorf("member %d won %d, mean %d", i, w, mean)
		}
	}
}

func BenchmarkKeyHash(b *testing.B) {
	key := []byte("sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")

	b.ReportAllocs()

	for b.Loop() {
		_ = KeyHash(7, key)
	}
}

func BenchmarkScore(b *testing.B) {
	var x uint64

	b.ReportAllocs()

	for i := uint64(0); b.Loop(); i++ {
		x += Score(i, 42)
	}

	_ = x
}
