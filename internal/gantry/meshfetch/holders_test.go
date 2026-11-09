// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package meshfetch

import (
	"slices"
	"testing"
	"time"
)

func TestHoldersRecentFirstBounded(t *testing.T) {
	h := newHolders(2)
	now := time.Unix(1000, 0)
	h.now = func() time.Time { return now }

	tick := func() { now = now.Add(time.Second) }

	for _, a := range []string{"a:1", "b:1", "c:1", "d:1"} {
		h.add(key(1), a)
		tick()
	}

	// Four per chunk: all kept, newest first.
	if got := h.list(key(1), ""); !slices.Equal(got, []string{"d:1", "c:1", "b:1", "a:1"}) {
		t.Fatalf("list = %v", got)
	}

	// A fifth replaces the oldest; a repeat refreshes its entry.
	h.add(key(1), "e:1")
	tick()
	h.add(key(1), "b:1")

	if got := h.list(key(1), ""); !slices.Equal(got, []string{"b:1", "e:1", "d:1", "c:1"}) {
		t.Fatalf("list = %v", got)
	}

	// self is skipped.
	if got := h.list(key(1), "b:1"); slices.Contains(got, "b:1") {
		t.Fatalf("self listed: %v", got)
	}

	// Invalid addresses are ignored.
	h.add(key(2), "no-port")
	h.add(key(2), "")

	if h.len() != 1 {
		t.Fatalf("len = %d", h.len())
	}

	// The directory holds two chunks: a third drops the oldest chunk.
	h.add(key(2), "x:1")
	h.add(key(3), "y:1")

	if h.list(key(1), "") != nil || h.len() != 2 {
		t.Fatalf("oldest chunk kept: len %d", h.len())
	}

	// Entries expire.
	now = now.Add(holderTTL + time.Second)

	if got := h.list(key(3), ""); len(got) != 0 {
		t.Fatalf("expired entries listed: %v", got)
	}
}
