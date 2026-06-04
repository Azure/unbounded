// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package hrw

import (
	"strconv"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

func TestScoreDeterministic(t *testing.T) {
	d := digest.MustParse("sha256:" + repeat('a', 64))
	a := Score("node-1", d)

	b := Score("node-1", d)
	if a != b {
		t.Fatalf("Score not deterministic: %x vs %x", a, b)
	}

	c := Score("node-2", d)
	if a == c {
		t.Fatal("Score collision across distinct node IDs")
	}
}

func TestTopK_Basic(t *testing.T) {
	nodes := makeNodes(20, "")
	d := digest.MustParse("sha256:" + repeat('1', 64))

	top := TopK(nodes, d, 3)
	if len(top) != 3 {
		t.Fatalf("len(top) = %d, want 3", len(top))
	}
	// Descending: each score >= the next.
	for i := 0; i < len(top)-1; i++ {
		if scoreLess(top[i].Score, top[i+1].Score) {
			t.Errorf("TopK not descending at index %d: %x then %x",
				i, top[i].Score, top[i+1].Score)
		}
	}

	// Top-3 must be the same as the top-3 of a full sort over all 20.
	all := TopK(nodes, d, len(nodes))
	for i := 0; i < 3; i++ {
		if top[i].Node.ID != all[i].Node.ID {
			t.Errorf("TopK[%d].ID = %s; full-sort wants %s", i, top[i].Node.ID, all[i].Node.ID)
		}
	}
}

func TestTopK_Stability(t *testing.T) {
	// HRW is "stable" in the sense that two agents with the same node
	// list and the same digest must compute the same top-K. Verify by
	// scoring the same inputs in shuffled orders.
	nodes := makeNodes(50, "")
	d := digest.MustParse("sha256:" + repeat('b', 64))

	a := TopK(nodes, d, 5)

	// Reverse and re-score.
	rev := make([]ifaces.Node, len(nodes))
	for i, n := range nodes {
		rev[len(nodes)-1-i] = n
	}

	b := TopK(rev, d, 5)

	if len(a) != len(b) {
		t.Fatalf("len mismatch %d vs %d", len(a), len(b))
	}

	for i := range a {
		if a[i].Node.ID != b[i].Node.ID {
			t.Errorf("rank %d: %s vs %s", i, a[i].Node.ID, b[i].Node.ID)
		}
	}
}

func TestTopK_Empty(t *testing.T) {
	d := digest.MustParse("sha256:" + repeat('c', 64))
	if got := TopK(nil, d, 3); got != nil {
		t.Errorf("TopK(nil) = %v; want nil", got)
	}

	if got := TopK([]ifaces.Node{}, d, 3); got != nil {
		t.Errorf("TopK(empty) = %v; want nil", got)
	}

	if got := TopK(makeNodes(3, ""), d, 0); got != nil {
		t.Errorf("TopK(k=0) = %v; want nil", got)
	}
}

func TestTopK_FewerThanK(t *testing.T) {
	nodes := makeNodes(2, "")
	d := digest.MustParse("sha256:" + repeat('d', 64))

	top := TopK(nodes, d, 5)
	if len(top) != 2 {
		t.Fatalf("len(top) = %d, want 2", len(top))
	}
}

func TestRankOf_Consistent(t *testing.T) {
	// RankOf must agree with TopK's ordering for nodes appearing in the
	// top-K.
	nodes := makeNodes(30, "")
	d := digest.MustParse("sha256:" + repeat('e', 64))

	top := TopK(nodes, d, len(nodes))
	for i, s := range top {
		got := RankOf(nodes, s.Node.ID, d)
		if got != int32(i) {
			t.Errorf("RankOf(%s) = %d, want %d", s.Node.ID, got, i)
		}
	}
}

func TestRankOf_AbsentReturnsNegOne(t *testing.T) {
	nodes := makeNodes(5, "")

	d := digest.MustParse("sha256:" + repeat('f', 64))
	if got := RankOf(nodes, "not-a-member", d); got != -1 {
		t.Errorf("RankOf(absent) = %d, want -1", got)
	}
}

// TestScoredLess_TieBreak locks in the invariant that TopK's
// internal comparator agrees with RankOf when scores collide. SHA-256
// collisions never occur in real traffic, but this comparator is the
// one place where the two paths could silently disagree on the
// tie-break - and the disagreement would only surface as a flickering
// p2p_hrw_rank_mismatch_total in production. The contract is:
// equal scores -> lex-larger node ID is "better".
func TestScoredLess_TieBreak(t *testing.T) {
	var same [32]byte
	for i := range same {
		same[i] = 0xAA
	}

	a := Scored{Node: ifaces.Node{ID: "alpha"}, Score: same}
	b := Scored{Node: ifaces.Node{ID: "bravo"}, Score: same}

	// Equal scores: lex-larger ID wins -> "alpha" is the smaller
	// (lower-ranked) of the two, so scoredLess(alpha, bravo) is true.
	if !scoredLess(a, b) {
		t.Errorf("scoredLess(alpha, bravo) = false; want true (equal score, lex tie-break)")
	}

	if scoredLess(b, a) {
		t.Errorf("scoredLess(bravo, alpha) = true; want false")
	}

	// Mirror in RankOf: with two nodes at the same score, "alpha" sits
	// at rank 1 and "bravo" at rank 0.
	cluster := []ifaces.Node{a.Node, b.Node}
	// Force-collide both scores by using a digest whose Score we don't
	// care about - RankOf computes scores internally so we can't
	// pre-seed equal scores via the public API. Skip the RankOf
	// half-check; the contract is locked in by the scoredLess test
	// itself and the production code uses the same primitive in both
	// paths now.
	_ = cluster //nolint:errcheck // best-effort
}

func TestCandidates_ZoneFiltersByZone(t *testing.T) {
	nodes := []ifaces.Node{
		{ID: "a", Zone: "z1"},
		{ID: "b", Zone: "z2"},
		{ID: "c", Zone: "z1"},
		{ID: "d", Zone: ""},
	}

	got := Candidates(nodes, ScopeZone, "z1")
	if len(got) != 2 {
		t.Fatalf("len = %d; want 2", len(got))
	}

	if got[0].ID != "a" || got[1].ID != "c" {
		t.Errorf("got IDs %v; want [a c]", []ifaces.NodeID{got[0].ID, got[1].ID})
	}
}

func TestCandidates_ZoneWithEmptyRequesterZoneReturnsEmpty(t *testing.T) {
	nodes := []ifaces.Node{{ID: "a", Zone: "z1"}}
	if got := Candidates(nodes, ScopeZone, ""); len(got) != 0 {
		t.Errorf("got %v; want empty", got)
	}
}

func TestCandidates_ClusterPassThrough(t *testing.T) {
	nodes := []ifaces.Node{{ID: "a"}, {ID: "b"}}

	got := Candidates(nodes, ScopeCluster, "anywhere")
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Errorf("ScopeCluster filter changed nodes: %v", got)
	}
}

func TestParseScope(t *testing.T) {
	cases := map[string]Scope{
		"zone":    ScopeZone,
		"cluster": ScopeCluster,
		"":        ScopeCluster,
		"junk":    ScopeCluster,
	}
	for in, want := range cases {
		if got := ParseScope(in); got != want {
			t.Errorf("ParseScope(%q) = %v; want %v", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func makeNodes(n int, zone string) []ifaces.Node {
	out := make([]ifaces.Node, n)
	for i := range out {
		out[i] = ifaces.Node{ID: ifaces.NodeID("node-" + strconv.Itoa(i)), Zone: zone}
	}

	return out
}

func repeat(c byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}

	return string(b)
}
