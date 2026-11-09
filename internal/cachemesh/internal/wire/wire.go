// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package wire defines the cachemesh control-plane messages and their
// canonical binary encoding.
//
// Every message is a request or a response carried by one
// ControlTransport.Send round trip. A message is an envelope (format version,
// kind, sender, sender's view version, digest, epoch, and known leader slot)
// followed by a kind-specific body. Deltas and snapshots are opaque byte
// strings produced by the view package.
//
// Layout (all integers per package codec):
//
//	u8      format version (FormatVersion)
//	u8      kind
//	ref     From
//	uvarint Version.Term, uvarint Version.Seq
//	[32]    Digest
//	uvarint Epoch
//	u32     LeaderSlot
//	...     body
//
// where ref is: string ID, uvarint Incarnation, u32 Slot.
//
// Decode never allocates for Ping, Pong, Ack, Nack, Broadcast, Invalidate,
// and Leave when decoding into a reused Message: string and byte-string fields
// alias the input buffer. Call Message.Detach before the input is reused or
// modified if the message is retained.
package wire

import (
	"errors"
	"fmt"
	"strconv"
	"unsafe"

	"github.com/Azure/unbounded/internal/cachemesh/internal/codec"
	"github.com/Azure/unbounded/internal/cachemesh/internal/placement"
	"github.com/Azure/unbounded/internal/cachemesh/internal/topology"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
)

// FormatVersion is the only envelope format this package reads and writes.
const FormatVersion uint8 = 1

// Limits enforced by Decode.
const (
	// MaxIDLen bounds a member ID, as in a view.
	MaxIDLen = view.MaxIDLen
	// MaxAddrLen bounds a member address, as in a view.
	MaxAddrLen = view.MaxAddrLen
	// MaxZoneLen bounds a zone name, as in a view.
	MaxZoneLen = view.MaxZoneLen
	// MaxMessageSize bounds a whole encoded message, including snapshots.
	MaxMessageSize = 64 << 20
	// MaxDeltas bounds the deltas in one CatchUpResponse.
	MaxDeltas = 4096
	// MaxInnerSize bounds the inner message carried by Routed.
	MaxInnerSize = 64 << 10
	// MaxPartitionBits bounds Join.PartitionBits: no cluster can use more
	// bits than a placement table holds.
	MaxPartitionBits = placement.MaxBits
	// MaxDimensions is the number of grid dimensions a Broadcast level may
	// name (levels 0..MaxDimensions-1).
	MaxDimensions = topology.MaxDims
)

// Errors returned by Decode, in addition to wrapped codec errors.
var (
	// ErrUnknownKind means the kind byte is not a defined Kind.
	ErrUnknownKind = errors.New("wire: unknown message kind")
	// ErrVersion means the format version byte is not FormatVersion.
	ErrVersion = errors.New("wire: unsupported format version")
	// ErrTooLarge means the message exceeds MaxMessageSize.
	ErrTooLarge = errors.New("wire: message too large")
)

// Kind identifies a message type.
type Kind uint8

// Message kinds. The zero value is invalid.
const (
	KindPing            Kind = 1
	KindPong            Kind = 2
	KindAck             Kind = 3
	KindNack            Kind = 4
	KindBroadcast       Kind = 5
	KindCatchUpRequest  Kind = 6
	KindCatchUpResponse Kind = 7
	KindRouted          Kind = 8
	KindJoin            Kind = 9
	KindLeave           Kind = 10
	KindSuspect         Kind = 11
	KindInvalidate      Kind = 12

	kindMax = KindInvalidate
)

var kindNames = [...]string{
	KindPing: "Ping", KindPong: "Pong", KindAck: "Ack", KindNack: "Nack",
	KindBroadcast: "Broadcast", KindCatchUpRequest: "CatchUpRequest",
	KindCatchUpResponse: "CatchUpResponse", KindRouted: "Routed", KindJoin: "Join",
	KindLeave: "Leave", KindSuspect: "Suspect", KindInvalidate: "Invalidate",
}

// Valid reports whether k is a defined kind.
func (k Kind) Valid() bool { return k >= KindPing && k <= kindMax }

// String returns the kind name.
func (k Kind) String() string {
	if k.Valid() {
		return kindNames[k]
	}

	return "Kind(" + strconv.Itoa(int(k)) + ")"
}

// Broadcast levels other than dimension indexes.
const (
	// LevelLeader marks a broadcast sent by the leader.
	LevelLeader int8 = -1
	// LevelLocal marks a cell-mate fan-out.
	LevelLocal int8 = -2
)

// ValidLevel reports whether l is LevelLeader, LevelLocal, or a dimension
// index below MaxDimensions.
func ValidLevel(l int8) bool { return l >= LevelLocal && l < MaxDimensions }

// Reason explains a Nack.
type Reason uint8

// Nack reasons. The zero value is invalid.
const (
	ReasonTTLExhausted Reason = 1
	ReasonNoRoute      Reason = 2
	ReasonNotLeader    Reason = 3
	ReasonBusy         Reason = 4
	ReasonBadRequest   Reason = 5

	reasonMax = ReasonBadRequest
)

// Valid reports whether r is a defined reason.
func (r Reason) Valid() bool { return r >= ReasonTTLExhausted && r <= reasonMax }

// MemberRef names one incarnation of a member. Slot is view.NoSlot when the
// member is not (yet) in the sender's view.
type MemberRef struct {
	ID          string
	Incarnation uint64
	Slot        view.Slot
}

// Message is any control-plane message. Kind selects which body fields are
// encoded; the others are ignored by Append and zeroed by Decode.
type Message struct {
	// Envelope, present on every kind.
	Kind       Kind
	From       MemberRef
	Version    view.Version
	Digest     view.Digest
	Epoch      uint64
	LeaderSlot view.Slot // sender's known leader, or view.NoSlot

	// Pong: optional leader address hint.
	LeaderAddr string

	// Nack.
	Reason Reason

	// Broadcast.
	Level int8
	Delta []byte

	// CatchUpRequest.
	Have       view.Version
	HaveDigest view.Digest
	MaxDeltas  uint32

	// CatchUpResponse: at most one of Deltas and Snapshot is non-empty; both
	// empty means the responder has nothing newer. Decode reuses the
	// capacity of Deltas.
	Deltas   [][]byte
	Snapshot []byte

	// Routed: Origin is the original sender. Target is used only when
	// ToLeader is false. Inner is a complete encoded message.
	Origin   MemberRef
	ToLeader bool
	Target   MemberRef
	TTL      uint8
	Hops     uint8
	Inner    []byte

	// Join uses Member (Slot ignored, encoded as given), Addr, Zone, and
	// PartitionBits. Leave uses Member. Suspect uses Target and Observer.
	Member        MemberRef
	Addr          string
	Zone          string
	PartitionBits uint8
	Observer      MemberRef

	// Invalidate.
	Seed uint64
}

// Reset zeroes m, keeping the capacity of Deltas.
func (m *Message) Reset() {
	d := m.Deltas[:0]
	clear(m.Deltas)
	*m = Message{Deltas: d}
}

// Detach copies every string and byte-string field so m no longer aliases
// the buffer it was decoded from.
func (m *Message) Detach() {
	cs := func(s string) string {
		if s == "" {
			return ""
		}

		return string([]byte(s))
	}
	cb := func(p []byte) []byte {
		if p == nil {
			return nil
		}

		return append([]byte{}, p...)
	}

	for _, r := range []*MemberRef{&m.From, &m.Origin, &m.Target, &m.Member, &m.Observer} {
		r.ID = cs(r.ID)
	}

	m.LeaderAddr, m.Addr, m.Zone = cs(m.LeaderAddr), cs(m.Addr), cs(m.Zone)
	m.Delta, m.Snapshot, m.Inner = cb(m.Delta), cb(m.Snapshot), cb(m.Inner)

	if m.Deltas != nil {
		ds := make([][]byte, len(m.Deltas))
		for i, d := range m.Deltas {
			ds[i] = cb(d)
		}

		m.Deltas = ds
	}
}

// Validate reports whether Append would produce bytes that Decode accepts.
func (m *Message) Validate() error {
	b := Append(nil, m)

	var t Message

	return Decode(b, &t)
}

// PeekKind returns the kind of an encoded message after checking its format
// version, without decoding the rest.
func PeekKind(b []byte) (Kind, error) {
	if len(b) < 2 {
		return 0, fmt.Errorf("wire: %w", codec.ErrShort)
	}

	if b[0] != FormatVersion {
		return 0, ErrVersion
	}

	k := Kind(b[1])
	if !k.Valid() {
		return 0, ErrUnknownKind
	}

	return k, nil
}

func appendRef(b []byte, r *MemberRef) []byte {
	b = codec.AppendString(b, r.ID)
	b = codec.AppendUvarint(b, r.Incarnation)

	return codec.AppendU32(b, r.Slot)
}

// Append appends the canonical encoding of m to b. Append does not validate
// m; an invalid m (for example an undefined Kind, or both Deltas and
// Snapshot set) yields bytes that Decode rejects. Use Validate to check.
// Fields that do not belong to m.Kind are not encoded.
func Append(b []byte, m *Message) []byte {
	b = append(b, FormatVersion, byte(m.Kind))
	b = appendRef(b, &m.From)
	b = view.AppendVersion(b, m.Version)
	b = append(b, m.Digest[:]...)
	b = codec.AppendUvarint(b, m.Epoch)
	b = codec.AppendU32(b, m.LeaderSlot)

	switch m.Kind {
	case KindPong:
		b = codec.AppendString(b, m.LeaderAddr)
	case KindNack:
		b = codec.AppendU8(b, uint8(m.Reason))
	case KindBroadcast:
		b = codec.AppendU8(b, uint8(m.Level))
		b = codec.AppendBytes(b, m.Delta)
	case KindCatchUpRequest:
		b = view.AppendVersion(b, m.Have)
		b = append(b, m.HaveDigest[:]...)
		b = codec.AppendUvarint(b, uint64(m.MaxDeltas))
	case KindCatchUpResponse:
		switch {
		case len(m.Deltas) > 0 && len(m.Snapshot) > 0:
			b = codec.AppendU8(b, 3) // rejected by Decode
		case len(m.Deltas) > 0:
			b = codec.AppendU8(b, 1)

			b = codec.AppendUvarint(b, uint64(len(m.Deltas)))
			for _, d := range m.Deltas {
				b = codec.AppendBytes(b, d)
			}
		case len(m.Snapshot) > 0:
			b = codec.AppendU8(b, 2)
			b = codec.AppendBytes(b, m.Snapshot)
		default:
			b = codec.AppendU8(b, 0)
		}
	case KindRouted:
		b = appendRef(b, &m.Origin)

		b = codec.AppendBool(b, m.ToLeader)
		if !m.ToLeader {
			b = appendRef(b, &m.Target)
		}

		b = append(b, m.TTL, m.Hops)
		b = codec.AppendBytes(b, m.Inner)
	case KindJoin:
		b = appendRef(b, &m.Member)
		b = codec.AppendString(b, m.Addr)
		b = codec.AppendString(b, m.Zone)
		b = codec.AppendU8(b, m.PartitionBits)
	case KindLeave:
		b = appendRef(b, &m.Member)
	case KindSuspect:
		b = appendRef(b, &m.Target)
		b = appendRef(b, &m.Observer)
	case KindInvalidate:
		b = codec.AppendU64(b, m.Seed)
	case KindPing, KindAck:
	}

	return b
}

// aliasString returns a string sharing p's memory.
func aliasString(p []byte) string {
	if len(p) == 0 {
		return ""
	}

	return unsafe.String(unsafe.SliceData(p), len(p))
}

func readRef(r *codec.Reader, ref *MemberRef) {
	ref.ID = aliasString(r.Bytes(MaxIDLen))
	ref.Incarnation = r.Uvarint()
	ref.Slot = r.U32()
}

// Decode decodes b into m, replacing its contents (the capacity of m.Deltas
// is reused). String and byte-string fields alias b; call m.Detach before
// reusing b if m is retained. On error m holds unspecified partial content.
func Decode(b []byte, m *Message) error {
	if len(b) > MaxMessageSize {
		return ErrTooLarge
	}

	k, err := PeekKind(b)
	if err != nil {
		return err
	}

	m.Reset()
	m.Kind = k
	r := codec.NewReader(b[2:])
	readRef(&r, &m.From)
	m.Version = view.ReadVersion(&r)
	r.Fixed(m.Digest[:])
	m.Epoch = r.Uvarint()
	m.LeaderSlot = r.U32()

	switch k {
	case KindPong:
		m.LeaderAddr = aliasString(r.Bytes(MaxAddrLen))
	case KindNack:
		m.Reason = Reason(r.U8())
		if r.Err() == nil && !m.Reason.Valid() {
			r.Fail(codec.ErrInvalid)
		}
	case KindBroadcast:
		m.Level = int8(r.U8()) //nolint:gosec // two's complement reinterpretation.
		if r.Err() == nil && !ValidLevel(m.Level) {
			r.Fail(codec.ErrInvalid)
		}

		m.Delta = r.Bytes(MaxMessageSize)
	case KindCatchUpRequest:
		m.Have = view.ReadVersion(&r)
		r.Fixed(m.HaveDigest[:])
		m.MaxDeltas = r.Uvarint32()
	case KindCatchUpResponse:
		decodeCatchUp(&r, m)
	case KindRouted:
		readRef(&r, &m.Origin)

		m.ToLeader = r.Bool()
		if !m.ToLeader {
			readRef(&r, &m.Target)
		}

		m.TTL = r.U8()
		m.Hops = r.U8()
		m.Inner = r.Bytes(MaxInnerSize)
	case KindJoin:
		readRef(&r, &m.Member)
		m.Addr = aliasString(r.Bytes(MaxAddrLen))
		m.Zone = aliasString(r.Bytes(MaxZoneLen))

		m.PartitionBits = r.U8()
		if m.PartitionBits > MaxPartitionBits {
			r.Fail(codec.ErrInvalid)
		}
	case KindLeave:
		readRef(&r, &m.Member)
	case KindSuspect:
		readRef(&r, &m.Target)
		readRef(&r, &m.Observer)
	case KindInvalidate:
		m.Seed = r.U64()
	case KindPing, KindAck:
	}

	if err := r.Done(); err != nil {
		return fmt.Errorf("wire: decode %v: %w", k, err)
	}

	return nil
}

func decodeCatchUp(r *codec.Reader, m *Message) {
	switch r.U8() {
	case 0:
	case 1:
		n := r.Len(MaxDeltas, 1)
		if r.Err() == nil && n == 0 {
			r.Fail(codec.ErrInvalid)
		}

		for range n {
			m.Deltas = append(m.Deltas, r.Bytes(MaxMessageSize))
		}
	case 2:
		m.Snapshot = r.Bytes(MaxMessageSize)
		if r.Err() == nil && len(m.Snapshot) == 0 {
			r.Fail(codec.ErrInvalid)
		}
	default:
		r.Fail(codec.ErrInvalid)
	}
}
