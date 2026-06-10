// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package hrw implements Rendezvous (Highest-Random-Weight) hashing for
// Gantry's per-digest puller selection (the step 3).
//
// Score function (deterministic across all agents):
//
//	score(node, digest) = SHA256( node_id_utf8 || digest_canonical_utf8 )
//
// Concatenation is a byte-level append, no separator. node_id is the
// stable string identity from ifaces.NodeID; digest_canonical is
// digest.Digest.String (e.g. "sha256:abc..."). Both forms are stable
// and identical across agents per the design doc / the design doc / the design doc.
//
// The top-K selection uses a min-heap of capacity K rather than a full
// sort: with 10k cluster members the algorithmic gain (O(N·log K) vs
// O(N·log N)) is meaningful, and the heap also bounds the temporary
// allocation. The final returned slice is ordered by score *descending*
// - rank 0 is the highest-scoring node, rank K-1 is the lowest of the
// top-K.
//
// Topology-aware mode (the design doc): callers may filter candidates to a single
// availability zone before scoring. The package itself is topology-
// agnostic - TopK takes whatever node slice it is given. Callers
// constructing zone-scoped sets must produce a strictly local view (any
// node with matching zone label, regardless of address state).
package hrw

import (
	"bytes"
	"container/heap"
	"crypto/sha256"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

// Scored pairs a node with its computed HRW score. The score is the
// 32-byte SHA-256 output; lexicographic byte-order comparison defines
// "higher" so all implementations agree on tie-breaking.
type Scored struct {
	Node  ifaces.Node
	Score [sha256.Size]byte
}

// Score computes the rendezvous score for a single (node, digest) pair.
// Exposed for tests and for callers that want to inspect raw scores
// (e.g., to report this node's own rank in PullIntentResponse).
func Score(nodeID ifaces.NodeID, d digest.Digest) [sha256.Size]byte {
	h := sha256.New()
	// Order is `node_id || digest`. Reversing the order would change
	// every score in the cluster - DO NOT swap without a coordinated
	// protocol-version bump.
	_, _ = h.Write([]byte(nodeID))     //nolint:errcheck // best-effort write
	_, _ = h.Write([]byte(d.String())) //nolint:errcheck // best-effort write

	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))

	return out
}

// TopK returns up to k nodes from candidates ranked by descending HRW
// score for d. If len(candidates) <= k, every candidate is returned (still
// in score order). A nil or empty candidates slice returns nil.
//
// The candidate filter - e.g., zone-scoping per the design doc - is the caller's
// responsibility. This function preserves no order from the input.
func TopK(candidates []ifaces.Node, d digest.Digest, k int) []Scored {
	if k <= 0 || len(candidates) == 0 {
		return nil
	}

	h := &minHeap{}
	heap.Init(h)

	for _, n := range candidates {
		s := Scored{Node: n, Score: Score(n.ID, d)}
		if h.Len() < k {
			heap.Push(h, s)
			continue
		}
		// Replace heap root if the new candidate is strictly better than
		// the current minimum (root of the min-heap). "Better" here
		// matches RankOf's tie-break: higher score, OR equal score with a
		// lexicographically larger node ID. Misaligning the two paths
		// would let an equal-score pair appear in TopK on one node but
		// not another, breaking the informer-divergence invariant.
		if scoredLess(h.peek(), s) {
			h.items[0] = s
			heap.Fix(h, 0)
		}
	}

	// Drain min-heap -> ascending order; reverse to descending so rank 0
	// is the top scorer (lowest-index entry).
	out := make([]Scored, h.Len())
	for i := len(out) - 1; i >= 0; i-- {
		out[i] = heap.Pop(h).(Scored) //nolint:errcheck // type assertion always valid for minHeap
	}

	return out
}

// RankOf returns the 0-based rank of nodeID inside cluster for digest d,
// or -1 if nodeID is not in cluster. Used by `pull_intent_query`
// responders to report their own rank back to the requester so the
// requester can detect informer-divergence (the design doc) and emit
// `p2p_hrw_rank_mismatch_total` (the design doc).
//
// Note this scores every member; for large clusters this is intentionally
// O(N) - there is no faster way to learn one's own rank without scoring
// every candidate.
func RankOf(cluster []ifaces.Node, nodeID ifaces.NodeID, d digest.Digest) int32 {
	target := Score(nodeID, d)
	betterCount := int32(0)
	found := false

	for _, n := range cluster {
		if n.ID == nodeID {
			found = true
			continue
		}

		s := Score(n.ID, d)
		// "Better" means strictly higher score. Strict comparison is
		// load-bearing - equal-score collisions are vanishingly rare
		// under SHA-256 but if they occur, the requester and responder
		// must both apply identical tie-breaking. Lexicographic node-ID
		// ordering serves as the tie-break.
		switch scoreCmp(s, target) {
		case +1:
			betterCount++
		case 0:
			if string(n.ID) > string(nodeID) {
				betterCount++
			}
		}
	}

	if !found {
		return -1
	}

	return betterCount
}

// ---------------------------------------------------------------------------
// Internal: min-heap of Scored entries keyed by ascending Score.
// ---------------------------------------------------------------------------

type minHeap struct{ items []Scored }

func (h *minHeap) Len() int { return len(h.items) }
func (h *minHeap) Less(i, j int) bool {
	return scoredLess(h.items[i], h.items[j])
}
func (h *minHeap) Swap(i, j int)      { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *minHeap) Push(x interface{}) { h.items = append(h.items, x.(Scored)) } //nolint:errcheck // type assertion always valid for minHeap
func (h *minHeap) Pop() interface{} {
	n := len(h.items)
	out := h.items[n-1]
	h.items = h.items[:n-1]

	return out
}
func (h *minHeap) peek() Scored { return h.items[0] }

func scoreLess(a, b [sha256.Size]byte) bool { return bytes.Compare(a[:], b[:]) < 0 }
func scoreCmp(a, b [sha256.Size]byte) int   { return bytes.Compare(a[:], b[:]) }

// scoredLess is the ID-aware less-than used by TopK's heap. Equal
// SHA-256 scores resolve by lexicographic node-ID ordering (the
// lex-LARGER ID wins, mirroring RankOf's tie-break). See package doc.
func scoredLess(a, b Scored) bool {
	if c := bytes.Compare(a.Score[:], b.Score[:]); c != 0 {
		return c < 0
	}

	return string(a.Node.ID) < string(b.Node.ID)
}
