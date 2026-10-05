// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"encoding/binary"
	"io"
)

const (
	FrameSize          = 21
	CreditSize         = 12
	PageFrame     byte = 1
	CompleteFrame byte = 2
)

// Frame describes a page payload or the terminal page/byte totals. Payload is external.
type Frame struct {
	Kind           byte
	Number, Offset uint64
	Length         uint32
}

func DecodeFrame(b [FrameSize]byte) Frame {
	return Frame{Kind: b[0], Number: binary.BigEndian.Uint64(b[1:9]), Offset: binary.BigEndian.Uint64(b[9:17]), Length: binary.BigEndian.Uint32(b[17:])}
}

func (f Frame) Encode() [FrameSize]byte {
	var b [FrameSize]byte

	b[0] = f.Kind
	binary.BigEndian.PutUint64(b[1:9], f.Number)
	binary.BigEndian.PutUint64(b[9:17], f.Offset)
	binary.BigEndian.PutUint32(b[17:], f.Length)

	return b
}

// WriteFrame preserves the encoder's single-write contract; the caller owns flushing.
func WriteFrame(w io.Writer, f Frame) error {
	b := f.Encode()
	_, err := w.Write(b[:])

	return err
}

type Credit struct {
	Number uint64
	Length uint32
}

func DecodeCredit(b [CreditSize]byte) Credit {
	return Credit{Number: binary.BigEndian.Uint64(b[:8]), Length: binary.BigEndian.Uint32(b[8:])}
}

func (c Credit) Encode() [CreditSize]byte {
	var b [CreditSize]byte
	binary.BigEndian.PutUint64(b[:8], c.Number)
	binary.BigEndian.PutUint32(b[8:], c.Length)

	return b
}

type pageInterval struct{ first, end uint64 }

// Sequence validates frame headers without reading payload, so callers can splice it.
// Call Accept only in the single receive goroutine, and do not continue after an error.
// Counts advance on accepted headers; callers must consume each payload before the next.
type Sequence struct {
	first, end, pages, delivered, bytes uint64
	ordered, complete                   bool
	intervals                           []pageInterval
}

func NewSequence(first, end uint64, ordered bool) *Sequence {
	return &Sequence{first: first, end: end, pages: PageCount(first, end), ordered: ordered}
}

// Intervals reports bounded tracking storage for diagnostics after a receive operation.
func (s *Sequence) Intervals() int {
	if s == nil {
		return 0
	}

	return len(s.intervals)
}

func PageCount(first, end uint64) uint64 {
	if end <= first {
		return 0
	}

	return (end-1)/PageSize - first/PageSize + 1
}

// Accept checks page identity, shape, uniqueness, ordering, and terminal totals.
// The operation is supplied to preserve the two SDK consumers' historical diagnostics.
func (s *Sequence) Accept(f Frame, operation string) error {
	bad := failure(ErrorProtocol, operation, nil)
	if s.complete {
		return bad
	}

	if f.Kind == CompleteFrame {
		if f.Number != s.pages || s.delivered != s.pages || f.Offset != s.end-s.first || s.bytes != f.Offset || f.Length != 0 {
			return bad
		}

		s.complete = true

		return nil
	}

	if f.Kind != PageFrame || f.Length == 0 || f.Offset < s.first || f.Offset >= s.end || f.Number != f.Offset/PageSize {
		return bad
	}

	start := max(s.first, f.Number*PageSize)

	end := min(s.end, (f.Number+1)*PageSize)
	if f.Offset != start || uint64(f.Length) != end-start || s.ordered && f.Number != s.first/PageSize+s.delivered {
		return bad
	}

	if !s.record(f.Number) {
		return bad
	}

	s.delivered++
	s.bytes += uint64(f.Length)

	return nil
}

// Write validates the same sequence as the decoder before encoding a frame header.
// The caller streams exactly Length payload bytes after each page header.
func (s *Sequence) Write(w io.Writer, f Frame) error {
	if err := s.Accept(f, "subscription frame"); err != nil {
		return err
	}

	return WriteFrame(w, f)
}

// record merges adjacent intervals with a hard memory limit for fragmented streams.
func (s *Sequence) record(n uint64) bool {
	i := 0
	for i < len(s.intervals) && s.intervals[i].end <= n {
		i++
	}

	if i < len(s.intervals) && s.intervals[i].first <= n {
		return false
	}

	if i > 0 && s.intervals[i-1].end == n {
		s.intervals[i-1].end++
		if i < len(s.intervals) && s.intervals[i].first == n+1 {
			s.intervals[i-1].end = s.intervals[i].end
			s.intervals = append(s.intervals[:i], s.intervals[i+1:]...)
		}

		return true
	}

	if i < len(s.intervals) && s.intervals[i].first == n+1 {
		s.intervals[i].first = n
		return true
	}

	if len(s.intervals) == 4096 {
		return false
	}

	s.intervals = append(s.intervals, pageInterval{})
	copy(s.intervals[i+1:], s.intervals[i:])
	s.intervals[i] = pageInterval{n, n + 1}

	return true
}
