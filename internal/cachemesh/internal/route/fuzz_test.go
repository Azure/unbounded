// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package route

import (
	"errors"
	"sync"
	"testing"

	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view/viewtest"
)

var (
	fuzzOnce sync.Once
	fuzzFix  *fixture
)

// FuzzForward feeds arbitrary headers to Forward on every member of a small
// view: it must never panic, accept only well-formed in-range headers, never
// route to itself, and keep the hop accounting consistent.
func FuzzForward(f *testing.F) {
	f.Add(Header{Format: HeaderFormat, TTL: 4, Partition: 3, Sender: 1}.Append(nil), uint16(0))
	f.Add(Header{Format: HeaderFormat, Flags: FlagBackup, Hops: 3, TTL: 4, Partition: 99, Sender: 7, Term: 9, Seq: 1 << 40}.Append(nil), uint16(5))
	f.Fuzz(func(t *testing.T, b []byte, self uint16) {
		fuzzOnce.Do(func() {
			v := viewtest.Build(t, 200, viewtest.Options{PartitionBits: 12, MaxNeighbors: 30})
			tr := newTracker(0)
			eject(t, tr, v, 3, 50, 77)
			fuzzFix = newFixture(v, newEnv(tr, 1))
		})

		h, err := ParseHeader(b)
		if err != nil {
			return
		}

		st := fuzzFix.st[int(self)%len(fuzzFix.st)]

		d, err := st.Forward(h)
		if err != nil {
			if !errors.Is(err, ErrBadHeader) && !errors.Is(err, ErrTTLExhausted) {
				t.Fatalf("unexpected error %v", err)
			}

			if errors.Is(err, ErrBadHeader) && h.Partition < 1<<12 {
				t.Fatalf("in-range header rejected: %+v", h)
			}

			return
		}

		if h.Partition >= 1<<12 {
			t.Fatalf("out-of-range partition accepted: %+v", h)
		}

		if !d.Next.IsZero() && d.Next.Slot == st.Self() {
			t.Fatalf("routed to self: %+v", d)
		}

		if d.Header.Hops != h.Hops+1 || d.Header.TTL != h.TTL || d.Header.Flags&FlagHedge != h.Flags&FlagHedge {
			t.Fatalf("header %+v from %+v", d.Header, h)
		}

		if !d.Next.IsZero() && d.Next.Slot == h.Sender && d.Next.Slot != d.Owner.Slot && h.Sender != view.NoSlot {
			t.Fatalf("sent back to sender %d: %+v", h.Sender, d)
		}
	})
}
