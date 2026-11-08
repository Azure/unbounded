// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package leader

import (
	"testing"

	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
)

// TestInputQueuesBounded floods every input queue: each stays within
// MaxPending, and per-target suspicion reports within 64.
func TestInputQueuesBounded(t *testing.T) {
	cfg := smallCfg("a")
	cfg.MaxPending = 8
	l := New(cfg, nil)

	for i := range 10_000 {
		l.Join(0, member(i))
		l.Leave(0, member(i).ID, 1)
		l.Suspect(0, view.Slot(i), uint64(i), view.Slot(i+1))
		l.Invalidate(uint64(i))
	}

	for o := range 10_000 {
		l.Suspect(int64(o), 0, 0, view.Slot(o))
	}

	if j, lv, s, i := l.Pending(); j > 8 || lv > 8 || s > 8 || i > 8 {
		t.Fatalf("pending %d %d %d %d", j, lv, s, i)
	}

	if n := len(l.suspects[0].reports); n > 64 {
		t.Fatalf("%d reports for one target", n)
	}

	l.Reset()

	if j, lv, s, i := l.Pending(); j+lv+s+i != 0 {
		t.Fatal("Reset left queued inputs")
	}
}
