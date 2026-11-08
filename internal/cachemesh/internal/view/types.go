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
