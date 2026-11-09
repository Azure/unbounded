// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package view

import (
	"encoding/hex"
	"math"
	"strconv"
)

// Slot is a member's index in a view. Slots are dense, reused only after
// Config.SlotReuseEpochs epochs, and stable for the lifetime of a member.
type Slot = uint32

// NoSlot means no member.
const NoSlot Slot = math.MaxUint32

// Version orders views. Versions compare by Term, then Seq. The zero Version
// is the genesis view, which has no members.
type Version struct {
	Term uint64
	Seq  uint64
}

// Less reports whether v orders before o.
func (v Version) Less(o Version) bool {
	if v.Term != o.Term {
		return v.Term < o.Term
	}

	return v.Seq < o.Seq
}

// IsZero reports whether v is the genesis version.
func (v Version) IsZero() bool { return v == Version{} }

// String formats v as "term.seq".
func (v Version) String() string {
	return strconv.FormatUint(v.Term, 10) + "." + strconv.FormatUint(v.Seq, 10)
}

// DigestSize is the size of a view digest in bytes.
const DigestSize = 32

// Digest identifies the content of a view. Two views with the same Version
// and a different Digest are a safety violation.
type Digest [DigestSize]byte

// String returns the first 8 bytes of d in hex, enough for logs.
func (d Digest) String() string { return hex.EncodeToString(d[:8]) }

// MemberState is a member's lifecycle state in a view.
type MemberState uint8

// Member states. The zero value is invalid so that a missing state is caught
// by Validate.
const (
	// Joining members are admitted but own no partitions (weight 0) until
	// they leave quarantine.
	Joining MemberState = 1
	// Active members own partitions according to their weight.
	Active MemberState = 2
	// Draining members are leaving: weight 0, removed after the drain period.
	Draining MemberState = 3
)

// Valid reports whether s is a defined state.
func (s MemberState) Valid() bool { return s >= Joining && s <= Draining }

// String returns the state name.
func (s MemberState) String() string {
	switch s {
	case Joining:
		return "Joining"
	case Active:
		return "Active"
	case Draining:
		return "Draining"
	default:
		return "MemberState(" + strconv.Itoa(int(s)) + ")"
	}
}

// Limits on view contents. Apply rejects values beyond them and the decoders
// use them to bound allocation on hostile input.
const (
	// MaxIDLen bounds Member.ID (a DNS subdomain name fits).
	MaxIDLen = 253
	// MaxAddrLen bounds Member.Addr.
	MaxAddrLen = 1024
	// MaxZoneLen bounds Member.Zone.
	MaxZoneLen = 128
	// MaxSlots bounds SlotCap and any slot number in a view.
	MaxSlots = 1 << 22
	// MaxCells bounds Shape().Cells() of a view. It equals MaxSlots (a view
	// never needs more cells than members) and is stricter than
	// MaxCells, which only bounds Shape arithmetic.
	MaxCells = MaxSlots
	// MaxDeltaOps bounds the number of ops in one delta.
	MaxDeltaOps = 1 << 16
	// MaxGens bounds the number of keyspace generations in a view.
	MaxGens = 1 << 16
)

// Member is one member record of a view. Members returned by a View are
// shared and must not be modified.
type Member struct {
	// ID is the application-supplied member name, unique in a view.
	ID string
	// Incarnation distinguishes restarts of the same ID.
	Incarnation uint64
	// Key is MemberKey(ID, Incarnation), the rendezvous key.
	Key uint64
	// Addr is the control and data endpoint chosen by the application.
	Addr string
	// Zone is an application-defined failure domain label.
	Zone string
	// State is the lifecycle state.
	State MemberState
	// Weight is 0..MaxWeight and 0 unless State is Active.
	Weight uint32
	// Cell is the member's grid cell, below Shape().Cells().
	Cell uint32
	// Joined is the view epoch at which the member was admitted.
	Joined uint64
	// Since is the view epoch of the member's last state change.
	Since uint64
}

// Config holds the cluster constants, set once by the Config op of the
// first delta of a cluster.
type Config struct {
	// PartitionBits is log2 of the partition count.
	PartitionBits uint8
	// InitialOwner owns every partition of the initial table, or NoSlot.
	InitialOwner Slot
	// SlotReuseEpochs is how many epochs a freed slot stays unused.
	SlotReuseEpochs uint32
	// HandoffEpochs is how long a moved partition reads through from Prev.
	HandoffEpochs uint32
}

// FreeSlot is a slot below SlotCap without a member, reusable once the view
// epoch reaches ReusableAt.
type FreeSlot struct {
	Slot       Slot
	ReusableAt uint64
}

// KeyspaceGen is the generation of the keyspace with the given seed.
// Keyspaces not listed have generation 0.
type KeyspaceGen struct {
	Seed uint64
	Gen  uint32
}
