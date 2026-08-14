// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package catalog

import (
	"testing"
	"time"
)

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
