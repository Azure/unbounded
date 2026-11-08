// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package route

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
)

// HeaderSize is the size of the encoded Header in bytes.
const HeaderSize = 32

// HeaderFormat is the only header format version this package produces and
// accepts.
const HeaderFormat uint8 = 1

// Header flags. FlagHedge is set by the caller on a hedged copy of a request;
// the others describe the hop the header is sent on and are recomputed by
// every member that routes it.
const (
	// FlagHedge marks a hedged request. It is preserved across hops.
	FlagHedge uint8 = 1 << iota
	// FlagDirect marks a hop to the owner outside the neighbor budget.
	FlagDirect
	// FlagGateway marks a hop from a non-member to a gateway.
	FlagGateway
	// FlagBackup marks a hop to a backup of an unusable owner. The receiver
	// serves the request itself if it is in the owner's cell.
	FlagBackup

	flagsKnown = FlagHedge | FlagDirect | FlagGateway | FlagBackup
	flagsHop   = FlagDirect | FlagGateway | FlagBackup
)

// Errors returned by ParseHeader and State.Forward.
var (
	// ErrBadHeader reports a malformed header or one that does not fit the
	// receiver's view.
	ErrBadHeader = errors.New("route: bad header")
	// ErrTTLExhausted reports a request that would exceed its hop budget.
	ErrTTLExhausted = errors.New("route: ttl exhausted")
)

// Header travels with every data-plane request. Its wire form is HeaderSize
// bytes, big-endian:
//
//	0      Format
//	1      Flags
//	2      Hops
//	3      TTL
//	4..8   Partition
//	8..12  Sender
//	12..16 Term
//	16..24 Seq
//	24..32 Epoch
//
// Term is the low 32 bits of the sender's view term; members compare it with
// the low 32 bits of their own term, which is exact as long as the cluster
// sees fewer than 2^32 leader terms.
type Header struct {
	// Format is HeaderFormat.
	Format uint8
	// Flags is a set of Flag* bits.
	Flags uint8
	// Hops counts the hops taken so far; the origin sends 0.
	Hops uint8
	// TTL is the hop budget set at the origin: grid dimensions plus two.
	TTL uint8
	// Partition is the partition of the key.
	Partition uint32
	// Sender is the slot of the member that sent this hop, or view.NoSlot.
	Sender view.Slot
	// Term is the sender's view term, truncated to 32 bits.
	Term uint32
	// Seq is the sender's view seq.
	Seq uint64
	// Epoch is the sender's current epoch.
	Epoch uint64
}

// Put encodes h into b.
func (h *Header) Put(b *[HeaderSize]byte) {
	b[0] = h.Format
	b[1] = h.Flags
	b[2] = h.Hops
	b[3] = h.TTL
	binary.BigEndian.PutUint32(b[4:], h.Partition)
	binary.BigEndian.PutUint32(b[8:], h.Sender)
	binary.BigEndian.PutUint32(b[12:], h.Term)
	binary.BigEndian.PutUint64(b[16:], h.Seq)
	binary.BigEndian.PutUint64(b[24:], h.Epoch)
}

// Append appends the encoded header to b.
func (h Header) Append(b []byte) []byte {
	var buf [HeaderSize]byte

	h.Put(&buf)

	return append(b, buf[:]...)
}

// ParseHeader decodes the first HeaderSize bytes of b. It rejects a short
// buffer, an unknown format, unknown flags, and Hops above TTL. Trailing
// bytes are ignored.
func ParseHeader(b []byte) (Header, error) {
	if len(b) < HeaderSize {
		return Header{}, fmt.Errorf("%w: %d bytes", ErrBadHeader, len(b))
	}

	h := Header{
		Format:    b[0],
		Flags:     b[1],
		Hops:      b[2],
		TTL:       b[3],
		Partition: binary.BigEndian.Uint32(b[4:]),
		Sender:    binary.BigEndian.Uint32(b[8:]),
		Term:      binary.BigEndian.Uint32(b[12:]),
		Seq:       binary.BigEndian.Uint64(b[16:]),
		Epoch:     binary.BigEndian.Uint64(b[24:]),
	}

	if err := h.check(); err != nil {
		return Header{}, err
	}

	return h, nil
}

func (h *Header) check() error {
	switch {
	case h.Format != HeaderFormat:
		return fmt.Errorf("%w: format %d", ErrBadHeader, h.Format)
	case h.Flags&^flagsKnown != 0:
		return fmt.Errorf("%w: flags %#x", ErrBadHeader, h.Flags)
	case h.Hops > h.TTL:
		return fmt.Errorf("%w: hops %d above ttl %d", ErrBadHeader, h.Hops, h.TTL)
	}

	return nil
}
