// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package catalog

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// lookup reads one node's entry out of the node table.
func lookup(t *testing.T, s *Store, node NodeKey) (Node, bool) {
	t.Helper()

	nodes, err := s.Nodes()
	if err != nil {
		t.Fatalf("read node table: %v", err)
	}

	for _, n := range nodes {
		if n.Key == node {
			return n, true
		}
	}

	return Node{}, false
}

// nodeFor is lookup for a node the test expects to be there.
func nodeFor(t *testing.T, s *Store, node NodeKey) Node {
	t.Helper()

	n, ok := lookup(t, s, node)
	if !ok {
		t.Fatalf("node %s has no entry", node)
	}

	return n
}

// mark publishes a watermark for a named node and fails the test if it cannot.
func mark(t *testing.T, s *Store, name string, generation uint64) NodeKey {
	t.Helper()

	node := NodeKeyFor(name)
	if err := s.SetWatermark(node, generation, DefaultWatermarkGrace); err != nil {
		t.Fatalf("set watermark for %s: %v", name, err)
	}

	return node
}

// The gate is what stands between a segment's pages and being discarded, so
// what it does with a node it has never heard from decides whether a node that
// has only just started can have its mounts trimmed away underneath it.
func TestDrainedPastWaitsForAnExpectedNodeWithNoWatermark(t *testing.T) {
	clock(t)

	_, s := newCatalog(t)

	a := mark(t, s, "node-a", 10)
	b := NodeKeyFor("node-b")

	drained, laggard, err := s.DrainedPast(5, DefaultWatermarkGrace, []NodeKey{a, b})
	if err != nil {
		t.Fatalf("drained past: %v", err)
	}

	if drained {
		t.Fatal("gate opened for a node that has never published a watermark")
	}

	if laggard != b {
		t.Fatalf("laggard is %s, want the silent node %s", laggard, b)
	}
}

// A node whose watermark has gone stale has stopped reporting, which is not the
// same as having drained. Before this it was written off as gone.
func TestDrainedPastWaitsForAStaleExpectedNode(t *testing.T) {
	advance := clock(t)

	_, s := newCatalog(t)

	a := mark(t, s, "node-a", 10)
	b := mark(t, s, "node-b", 10)

	advance(2 * DefaultWatermarkGrace)

	// a keeps reporting; b does not.
	if err := s.SetWatermark(a, 10, DefaultWatermarkGrace); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	drained, laggard, err := s.DrainedPast(5, DefaultWatermarkGrace, []NodeKey{a, b})
	if err != nil {
		t.Fatalf("drained past: %v", err)
	}

	if drained {
		t.Fatal("gate opened while an expected node was not reporting")
	}

	if laggard != b {
		t.Fatalf("laggard is %s, want the silent node %s", laggard, b)
	}
}

// An expected node that is reporting and behind is the ordinary case the gate
// exists for.
func TestDrainedPastWaitsForABehindExpectedNode(t *testing.T) {
	clock(t)

	_, s := newCatalog(t)

	a := mark(t, s, "node-a", 10)
	b := mark(t, s, "node-b", 4)

	drained, laggard, err := s.DrainedPast(5, DefaultWatermarkGrace, []NodeKey{a, b})
	if err != nil {
		t.Fatalf("drained past: %v", err)
	}

	if drained {
		t.Fatal("gate opened with an expected node behind the repoint")
	}

	if laggard != b {
		t.Fatalf("laggard is %s, want %s", laggard, b)
	}
}

func TestDrainedPastOpensWhenEveryExpectedNodeIsPast(t *testing.T) {
	clock(t)

	_, s := newCatalog(t)

	a := mark(t, s, "node-a", 10)
	b := mark(t, s, "node-b", 5)

	drained, laggard, err := s.DrainedPast(5, DefaultWatermarkGrace, []NodeKey{a, b})
	if err != nil {
		t.Fatalf("drained past: %v", err)
	}

	if !drained {
		t.Fatalf("gate held by %s with every expected node past the repoint", laggard)
	}
}

// A decommissioned node leaves its slot behind. Waiting for it forever would
// mean the volume never reclaimed anything again after the first node was
// removed, so a stale entry nobody expects is written off.
func TestDrainedPastIgnoresAStaleStranger(t *testing.T) {
	advance := clock(t)

	_, s := newCatalog(t)

	a := mark(t, s, "node-a", 10)

	mark(t, s, "departed", 1)

	advance(2 * DefaultWatermarkGrace)

	if err := s.SetWatermark(a, 10, DefaultWatermarkGrace); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	drained, laggard, err := s.DrainedPast(5, DefaultWatermarkGrace, []NodeKey{a})
	if err != nil {
		t.Fatalf("drained past: %v", err)
	}

	if !drained {
		t.Fatalf("gate held by %s for a node that has stopped reporting and is not expected", laggard)
	}
}

// A node the membership view has not caught up with yet is still reporting, and
// is still reading. It holds the gate even though nobody expects it.
func TestDrainedPastWaitsForAFreshStranger(t *testing.T) {
	clock(t)

	_, s := newCatalog(t)

	a := mark(t, s, "node-a", 10)
	stranger := mark(t, s, "unlisted", 1)

	drained, laggard, err := s.DrainedPast(5, DefaultWatermarkGrace, []NodeKey{a})
	if err != nil {
		t.Fatalf("drained past: %v", err)
	}

	if drained {
		t.Fatal("gate opened while an unlisted node was still reporting from behind the repoint")
	}

	if laggard != stranger {
		t.Fatalf("laggard is %s, want %s", laggard, stranger)
	}
}

// Not knowing who is out there is not the same as knowing nobody is.
func TestDrainedPastRefusesAnEmptyExpectedSet(t *testing.T) {
	clock(t)

	_, s := newCatalog(t)

	mark(t, s, "node-a", 10)

	drained, _, err := s.DrainedPast(5, DefaultWatermarkGrace, nil)
	if err == nil {
		t.Fatal("expected an error for an empty expected set")
	}

	if drained {
		t.Fatal("gate opened with no expected nodes")
	}
}

func TestDrainedPastRefusesAZeroExpectedNode(t *testing.T) {
	clock(t)

	_, s := newCatalog(t)

	a := mark(t, s, "node-a", 10)

	if _, _, err := s.DrainedPast(5, DefaultWatermarkGrace, []NodeKey{a, {}}); err == nil {
		t.Fatal("expected an error for a zero key in the expected set")
	}
}

// The grace is a policy of the caller's, not of the table, and a gate asked
// with a longer one has to keep waiting for a node a shorter one would have
// written off.
func TestDrainedPastHonoursTheGrace(t *testing.T) {
	advance := clock(t)

	_, s := newCatalog(t)

	a := mark(t, s, "node-a", 10)
	b := mark(t, s, "node-b", 1)

	advance(2 * time.Minute)

	if err := s.SetWatermark(a, 10, DefaultWatermarkGrace); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	drained, _, err := s.DrainedPast(5, time.Minute, []NodeKey{a})
	if err != nil {
		t.Fatalf("drained past: %v", err)
	}

	if !drained {
		t.Fatal("gate held by a stranger stale under the short grace")
	}

	drained, laggard, err := s.DrainedPast(5, time.Hour, []NodeKey{a})
	if err != nil {
		t.Fatalf("drained past: %v", err)
	}

	if drained {
		t.Fatal("gate opened on a stranger still fresh under the long grace")
	}

	if laggard != b {
		t.Fatalf("laggard is %s, want %s", laggard, b)
	}
}

// A node block is the unit of ownership in the node table: one node writes it,
// and everything that node has to say lives in it. A round trip that lost a
// field would lose a mark answer, and a lost answer reads as a node that never
// claimed a blob it is in fact still mounting.
func TestNodeBlockRoundTrip(t *testing.T) {
	claims := NewClaims()
	claims.Set(0)
	claims.Set(63)
	claims.Set(ClaimBits - 1)

	want := Node{
		Key:        NodeKeyFor("node-a"),
		Generation: 42,
		Updated:    time.Unix(1700000000, 0).UTC(),
		Mark: Mark{
			Segment:    7,
			Generation: 41,
			Ordering:   Digest{1, 2, 3},
			Claims:     claims,
		},
	}

	block := make([]byte, BlockBytes)
	if err := want.MarshalTo(block); err != nil {
		t.Fatalf("marshal: %v", err)
	}

	got, err := UnmarshalNode(block)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Key != want.Key || got.Generation != want.Generation || !got.Updated.Equal(want.Updated) {
		t.Fatalf("node = %+v, want %+v", got, want)
	}

	if got.Mark.Segment != want.Mark.Segment || got.Mark.Generation != want.Mark.Generation {
		t.Fatalf("mark = %+v, want %+v", got.Mark, want.Mark)
	}

	if got.Mark.Ordering != want.Mark.Ordering {
		t.Fatalf("ordering = %x, want %x", got.Mark.Ordering, want.Mark.Ordering)
	}

	for _, i := range []int{0, 63, ClaimBits - 1} {
		if !got.Mark.Claims.Has(i) {
			t.Fatalf("claim %d did not survive the round trip", i)
		}
	}

	if got.Mark.Claims.Has(1) {
		t.Fatal("claim 1 was set by the round trip")
	}
}

// An unclaimed block has to be distinguishable from a claimed one, because that
// is how a node finds a block to take.
func TestUnmarshalNodeReadsAnEmptyBlockAsUnclaimed(t *testing.T) {
	got, err := UnmarshalNode(make([]byte, BlockBytes))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if !got.Key.IsZero() {
		t.Fatalf("empty block decoded to node %s", got.Key)
	}
}

// A node with nothing to say about a mark round leaves the mark fields alone,
// and a reader must not mistake that for an answer claiming nothing. An empty
// claim set and no answer at all mean opposite things to the cleaner.
func TestNodeWithoutAMarkAnswersNothing(t *testing.T) {
	want := Node{Key: NodeKeyFor("node-a"), Generation: 9, Updated: time.Unix(1700000000, 0).UTC()}

	block := make([]byte, BlockBytes)
	if err := want.MarshalTo(block); err != nil {
		t.Fatalf("marshal: %v", err)
	}

	got, err := UnmarshalNode(block)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Mark.Answered() {
		t.Fatalf("mark = %+v, want no answer", got.Mark)
	}

	if got.Mark.Claims != nil {
		t.Fatal("an unanswered mark carries a claim set")
	}
}

// A block with a checksum that does not match is not a node whose fields happen
// to be wrong, it is a block that must not be read at all.
func TestUnmarshalNodeRejectsACorruptBlock(t *testing.T) {
	n := Node{Key: NodeKeyFor("node-a"), Generation: 1, Updated: time.Unix(1700000000, 0).UTC()}

	block := make([]byte, BlockBytes)
	if err := n.MarshalTo(block); err != nil {
		t.Fatalf("marshal: %v", err)
	}

	block[NodeHeaderBytes]++

	if _, err := UnmarshalNode(block); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

// The watermark refresh and the mark answer are written by different parts of
// the daemon at different times. They share a block, so each has to leave the
// other's fields alone or a heartbeat would erase an answer the cleaner is
// waiting for.
func TestSetWatermarkKeepsTheMark(t *testing.T) {
	clock(t)

	_, s := newCatalog(t)

	node := NodeKeyFor("node-a")

	claims := NewClaims()
	claims.Set(2)

	answer := Mark{Segment: 3, Generation: 12, Ordering: Digest{9}, Claims: claims}
	if err := s.SetMark(node, answer, DefaultWatermarkGrace); err != nil {
		t.Fatalf("set mark: %v", err)
	}

	if err := s.SetWatermark(node, 30, DefaultWatermarkGrace); err != nil {
		t.Fatalf("set watermark: %v", err)
	}

	got := nodeFor(t, s, node)

	if got.Generation != 30 {
		t.Fatalf("generation = %d, want 30", got.Generation)
	}

	if !got.Mark.For(3, 12) || !got.Mark.Claims.Has(2) {
		t.Fatalf("mark = %+v, want the answer for segment 3", got.Mark)
	}
}

// The other direction: answering a mark round must not report a watermark the
// node has not reached, because the drain gate reads the same block.
func TestSetMarkKeepsTheWatermark(t *testing.T) {
	clock(t)

	_, s := newCatalog(t)

	node := mark(t, s, "node-a", 21)

	answer := Mark{Segment: 4, Generation: 20, Ordering: Digest{7}, Claims: NewClaims()}
	if err := s.SetMark(node, answer, DefaultWatermarkGrace); err != nil {
		t.Fatalf("set mark: %v", err)
	}

	got := nodeFor(t, s, node)

	if got.Generation != 21 {
		t.Fatalf("generation = %d, want the watermark to survive at 21", got.Generation)
	}

	if !got.Mark.For(4, 20) {
		t.Fatalf("mark = %+v, want the answer for segment 4", got.Mark)
	}
}

// A mark answer names the round it answers. Anything else is an answer to an
// older question, and the cleaner has to be able to tell.
func TestMarkForNamesItsRound(t *testing.T) {
	m := Mark{Segment: 3, Generation: 12}

	if !m.For(3, 12) {
		t.Fatal("mark does not answer its own round")
	}

	if m.For(3, 13) || m.For(4, 12) {
		t.Fatal("mark answers a round it was not written for")
	}

	if (Mark{}).For(0, 0) {
		t.Fatal("an unanswered mark answers a round")
	}
}

// Reusing a departed node's block is how the table stays finite. What must not
// come with it is the departed node's answer, which would otherwise be counted
// as the new node's claim over blobs it has never seen.
func TestPublishTakesOverAStaleBlockWithoutItsMark(t *testing.T) {
	advance := clock(t)

	dev := newOCCDevice()

	if err := Format(dev.client(), FormatOptions{Bytes: testCatalogBytes, NodeBlocks: 1}); err != nil {
		t.Fatalf("format: %v", err)
	}

	s := open(t, dev)

	gone := NodeKeyFor("node-gone")

	claims := NewClaims()
	claims.Set(1)

	if err := s.SetMark(gone, Mark{Segment: 5, Generation: 4, Claims: claims}, DefaultWatermarkGrace); err != nil {
		t.Fatalf("set mark: %v", err)
	}

	advance(2 * DefaultWatermarkGrace)

	fresh := mark(t, s, "node-fresh", 6)

	got := nodeFor(t, s, fresh)

	if got.Mark.Answered() {
		t.Fatalf("mark = %+v, want the departed node's answer dropped", got.Mark)
	}

	if _, ok := lookup(t, s, gone); ok {
		t.Fatal("the departed node still holds a block")
	}
}

// A node that cannot claim a block cannot be waited for, so running out of
// blocks has to be an error a node refuses to start on rather than something it
// works around.
func TestPublishReportsAFullTable(t *testing.T) {
	clock(t)

	dev := newOCCDevice()

	if err := Format(dev.client(), FormatOptions{Bytes: testCatalogBytes, NodeBlocks: 1}); err != nil {
		t.Fatalf("format: %v", err)
	}

	s := open(t, dev)

	mark(t, s, "node-a", 1)

	err := s.SetWatermark(NodeKeyFor("node-b"), 1, DefaultWatermarkGrace)
	if err == nil {
		t.Fatal("want an error when the node table is full")
	}

	if !strings.Contains(err.Error(), "full") {
		t.Fatalf("err = %v, want it to report a full table", err)
	}
}

// A mark answer with no segment names no round, so writing one would leave a
// block that claims nothing about anything while looking like an answer.
func TestSetMarkRefusesAnEmptyAnswer(t *testing.T) {
	clock(t)

	_, s := newCatalog(t)

	if err := s.SetMark(NodeKeyFor("node-a"), Mark{}, DefaultWatermarkGrace); err == nil {
		t.Fatal("want an error for a mark that answers no round")
	}
}

func TestClaims(t *testing.T) {
	c := NewClaims()

	c.Set(0)
	c.Set(9)

	if !c.Has(0) || !c.Has(9) {
		t.Fatal("set claims do not read back")
	}

	if c.Has(1) {
		t.Fatal("claim 1 reads as set")
	}

	// Out of range is ignored rather than a panic: the bitmap is a fixed
	// size and a segment with more blobs than bits is refused a round
	// before it gets here, but a bounds bug must not take a node down.
	c.Set(-1)
	c.Set(ClaimBits)

	if c.Has(-1) || c.Has(ClaimBits) {
		t.Fatal("an out of range claim reads as set")
	}

	if (Claims)(nil).Has(0) {
		t.Fatal("a nil claim set reads as claiming something")
	}

	other := NewClaims()
	other.Set(3)

	c.Or(other)

	if !c.Has(3) || !c.Has(0) {
		t.Fatal("or lost a claim")
	}
}
