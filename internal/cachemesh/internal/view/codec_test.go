// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package view_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view/viewtest"
)

func allOpsDelta(t testing.TB) (*view.View, *view.Delta) {
	t.Helper()

	v := boot(t, 3)
	sh, _ := view.NewShape(2, 2)
	d := view.NewDelta(v,
		view.OpNewTerm(4, 1),
		view.OpEpoch(11, 3*time.Second),
		view.OpInvalidate(1<<63, 2),
		view.OpAddMember(3, member(8)),
		view.OpSetState(3, view.Active, 250),
		view.OpReshape(sh, []uint32{0, 1, 2, 3}),
		view.OpSetCell(2, 0),
		view.OpAssign([]uint32{0, 1, 200}, []uint32{1, 2, 3}),
		view.OpRemoveMember(2),
	)

	return v, d
}

func TestDeltaCodec(t *testing.T) {
	v, d := allOpsDelta(t)
	if _, err := view.Apply(v, d); err == nil {
		t.Fatal("removing an owner should fail")
	}

	d.Ops = d.Ops[:len(d.Ops)-1]
	view.Seal(v, d)

	nv, err := view.Apply(v, d)
	if err != nil {
		t.Fatal(err)
	}

	checkRoundTrips(t, v, nv, d)

	enc := view.EncodeDelta(d)

	dd, err := view.DecodeDelta(enc)
	if err != nil {
		t.Fatal(err)
	}

	if dd.Digest != d.Digest || dd.Version != d.Version || len(dd.Ops) != len(d.Ops) || *nv.Shape() != dd.Ops[5].Shape {
		t.Fatal("decoded delta differs")
	}

	for i := range len(enc) {
		if _, err := view.DecodeDelta(enc[:i]); err == nil {
			t.Fatalf("prefix %d decoded", i)
		}
	}

	if _, err := view.DecodeDelta(append(bytes.Clone(enc), 0)); err == nil {
		t.Fatal("trailing byte accepted")
	}

	bad := bytes.Clone(enc)
	bad[0] = 9

	if _, err := view.DecodeDelta(bad); !errors.Is(err, view.ErrFormat) {
		t.Fatalf("format: %v", err)
	}
}

func TestSnapshotCodec(t *testing.T) {
	for _, v := range []*view.View{
		view.Genesis(),
		boot(t, 1),
		viewtest.Build(t, 300, viewtest.Options{PartitionBits: 12, Joining: 20, Draining: 5}),
	} {
		enc := view.EncodeSnapshot(v)

		sv, err := view.DecodeSnapshot(enc)
		if err != nil {
			t.Fatal(err)
		}

		if !view.Equal(v, sv) {
			t.Fatal("round trip")
		}

		for i := 0; i < len(enc); i += 1 + len(enc)/500 {
			if _, err := view.DecodeSnapshot(enc[:i]); err == nil {
				t.Fatalf("prefix %d decoded", i)
			}
		}
	}

	// Corrupting a member's state is caught.
	v := boot(t, 1)
	enc := view.EncodeSnapshot(v)

	for i := range enc {
		b := bytes.Clone(enc)
		b[i] ^= 0x40

		if sv, err := view.DecodeSnapshot(b); err == nil {
			if verr := view.Validate(sv); verr != nil {
				t.Fatalf("decoded invalid view: %v", verr)
			}
		}
	}
}

func FuzzDecodeDelta(f *testing.F) {
	v, d := allOpsDelta(f)
	f.Add(view.EncodeDelta(d))
	f.Add(view.EncodeDelta(view.NewDelta(v)))
	f.Add([]byte{1, 0, 0, 0, 1, 1, 7, 1, 1})
	f.Fuzz(func(t *testing.T, b []byte) {
		d, err := view.DecodeDelta(b)
		if err != nil {
			return
		}

		if !bytes.Equal(view.EncodeDelta(d), b) {
			t.Fatal("non-canonical delta accepted")
		}

		_, _ = view.Apply(v, d)
	})
}

func FuzzDecodeSnapshot(f *testing.F) {
	f.Add(view.EncodeSnapshot(view.Genesis()))
	f.Add(view.EncodeSnapshot(viewtest.Build(f, 40, viewtest.Options{PartitionBits: 6, Joining: 3})))
	f.Fuzz(func(t *testing.T, b []byte) {
		v, err := view.DecodeSnapshot(b)
		if err != nil {
			return
		}

		if err := view.Validate(v); err != nil {
			t.Fatal(err)
		}

		if !bytes.Equal(view.EncodeSnapshot(v), b) {
			t.Fatal("non-canonical snapshot accepted")
		}
	})
}
