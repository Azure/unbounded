// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package view

import (
	"crypto/sha256"
	"strconv"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/topology"
)

// OpKind identifies the kind of an Op. Its numeric value is part of the
// encoding.
type OpKind uint8

// Op kinds. See Op for the fields each kind uses.
const (
	// KindNewTerm starts a leader term: Term, Slot (the leader).
	KindNewTerm OpKind = 1
	// KindConfig sets the cluster constants: Config.
	KindConfig OpKind = 2
	// KindAddMember admits a member: Slot, Member.
	KindAddMember OpKind = 3
	// KindRemoveMember removes a member and frees its slot: Slot.
	KindRemoveMember OpKind = 4
	// KindSetState changes state and weight: Slot, State, Weight.
	KindSetState OpKind = 5
	// KindSetCell moves a member to another cell: Slot, Cell.
	KindSetCell OpKind = 6
	// KindReshape replaces the grid: Shape, Cells (indexed by slot).
	KindReshape OpKind = 7
	// KindAssign moves partitions: Parts, Owners.
	KindAssign OpKind = 8
	// KindEpoch sets the epoch anchor: Epoch, Period.
	KindEpoch OpKind = 9
	// KindInvalidate bumps a keyspace generation: Seed, Gen.
	KindInvalidate OpKind = 10
)

// String returns the kind name.
func (k OpKind) String() string {
	switch k {
	case KindNewTerm:
		return "NewTerm"
	case KindConfig:
		return "Config"
	case KindAddMember:
		return "AddMember"
	case KindRemoveMember:
		return "RemoveMember"
	case KindSetState:
		return "SetState"
	case KindSetCell:
		return "SetCell"
	case KindReshape:
		return "Reshape"
	case KindAssign:
		return "Assign"
	case KindEpoch:
		return "Epoch"
	case KindInvalidate:
		return "Invalidate"
	default:
		return "OpKind(" + strconv.Itoa(int(k)) + ")"
	}
}

// Op is one change in a Delta. It is a tagged struct: Kind selects which
// fields are meaningful, and the others are ignored by Apply and by the
// encoding. Build ops with the Op* constructors.
type Op struct {
	Kind OpKind
	// Slot is the leader (NewTerm) or the subject member (AddMember,
	// RemoveMember, SetState, SetCell).
	Slot Slot
	// Term is the new term (NewTerm).
	Term uint64
	// Member is the admitted member (AddMember). Joined and Since are
	// ignored and set from the view epoch.
	Member Member
	// State and Weight are the new state and weight (SetState).
	State  MemberState
	Weight uint32
	// Cell is the new cell (SetCell).
	Cell uint32
	// Config is the cluster constants (Config).
	Config Config
	// Shape is the new grid and Cells the new cell of every slot below
	// SlotCap, topology.NoCell for slots without a member (Reshape).
	Shape topology.Shape
	Cells []uint32
	// Parts[i] moves to Owners[i] (Assign).
	Parts, Owners []uint32
	// Epoch and Period are the new epoch anchor (Epoch).
	Epoch  uint64
	Period time.Duration
	// Seed and Gen are the keyspace and its new generation (Invalidate).
	Seed uint64
	Gen  uint32
}

// OpNewTerm starts term with the given leader slot.
func OpNewTerm(term uint64, leader Slot) Op { return Op{Kind: KindNewTerm, Term: term, Slot: leader} }

// OpConfig sets the cluster constants.
func OpConfig(c Config) Op { return Op{Kind: KindConfig, Config: c} }

// OpAddMember admits m at slot.
func OpAddMember(slot Slot, m Member) Op { return Op{Kind: KindAddMember, Slot: slot, Member: m} }

// OpRemoveMember removes the member at slot.
func OpRemoveMember(slot Slot) Op { return Op{Kind: KindRemoveMember, Slot: slot} }

// OpSetState sets the state and weight of the member at slot.
func OpSetState(slot Slot, s MemberState, weight uint32) Op {
	return Op{Kind: KindSetState, Slot: slot, State: s, Weight: weight}
}

// OpSetCell moves the member at slot to cell.
func OpSetCell(slot Slot, cell uint32) Op { return Op{Kind: KindSetCell, Slot: slot, Cell: cell} }

// OpReshape replaces the grid. cells is indexed by slot. The slice is
// retained.
func OpReshape(s topology.Shape, cells []uint32) Op {
	return Op{Kind: KindReshape, Shape: s, Cells: cells}
}

// OpAssign moves parts[i] to owners[i]. The slices are retained.
func OpAssign(parts, owners []uint32) Op { return Op{Kind: KindAssign, Parts: parts, Owners: owners} }

// OpEpoch sets the epoch anchor.
func OpEpoch(epoch uint64, period time.Duration) Op {
	return Op{Kind: KindEpoch, Epoch: epoch, Period: period}
}

// OpInvalidate sets the generation of keyspace seed to gen.
func OpInvalidate(seed uint64, gen uint32) Op { return Op{Kind: KindInvalidate, Seed: seed, Gen: gen} }

// Delta changes a view from Base to Version. Digest is the digest of the
// resulting view, sha256(base digest || canonical encoding of the delta
// without its digest); fill it with Seal or NewDelta.
type Delta struct {
	Base, Version Version
	Digest        Digest
	Ops           []Op
}

// NextVersion returns the version of a delta on base: {Term, 1} if ops start
// with NewTerm, otherwise {base.Term, base.Seq + 1}.
func NextVersion(base Version, ops []Op) Version {
	if len(ops) > 0 && ops[0].Kind == KindNewTerm {
		return Version{Term: ops[0].Term, Seq: 1}
	}

	return Version{Term: base.Term, Seq: base.Seq + 1}
}

// NewDelta returns a sealed delta of ops on base with the next version.
// The ops slice is retained.
func NewDelta(base *View, ops ...Op) *Delta {
	d := &Delta{Base: base.Version(), Version: NextVersion(base.Version(), ops), Ops: ops}
	Seal(base, d)

	return d
}

// Seal sets d.Digest for applying d on base.
func Seal(base *View, d *Delta) { d.Digest = DeltaDigest(base.Digest(), d) }

// DeltaDigest returns the digest of the view that results from applying d on
// a view with digest base. It ignores d.Digest.
func DeltaDigest(base Digest, d *Delta) Digest {
	h := sha256.New()
	_, _ = h.Write(base[:])
	_, _ = h.Write(appendDeltaBody(make([]byte, 0, deltaSizeHint(d)), d))

	var out Digest

	h.Sum(out[:0])

	return out
}
