// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"errors"
	"io"
	"math"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

// HostReadOptions selects the part of an object a [Client.HostRead] reads
// and where in the file it lands. Object offset o is written at file offset
// DestinationOffset + (o - Offset).
type HostReadOptions struct {
	// Offset is the first object byte to read.
	Offset int64
	// Length is the number of bytes to read. Zero reads to the end of the
	// object.
	Length int64
	// ETag pins the read to one version, as in [ReadOptions].
	ETag string
	// DestinationOffset is the file offset of the first byte written.
	DestinationOffset int64
	// DestinationLength bounds the file window Racer may write, starting at
	// DestinationOffset. It must be at least the range length. Zero means
	// the file size minus DestinationOffset.
	DestinationLength int64
	// OnPage, if set, is called after Racer reports that a page is in the
	// file, with the object offset and length of the bytes written. It runs
	// on the calling goroutine and should be quick.
	OnPage func(offset, length int64)
}

// HostReadResult reports a completed [Client.HostRead].
type HostReadResult struct {
	// Metadata describes the version that was read.
	Metadata Metadata
	// Offset and Length are the object range that was written.
	Offset, Length int64
}

// HostRead asks Racer to write an object range straight into file, normally
// a memfd, instead of streaming it over the socket. The file must be a
// regular file opened for writing without O_APPEND. Racer writes with
// positioned writes, except when file is a memfd opened read-write, sealed
// with F_SEAL_SHRINK, not sealed with F_SEAL_WRITE or F_SEAL_FUTURE_WRITE,
// and at least as long as the destination window: Racer then maps the file
// and decrypts straight into it, skipping a copy. Create such a memfd with
// MFD_ALLOW_SEALING and add F_SEAL_SHRINK after sizing it. The descriptor is passed to Racer with SCM_RIGHTS on the
// client socket, so the caller and Racer must be on the same host.
//
// HostRead returns once Racer confirms every byte is in the file. On error
// the file may hold partial data. Racers that predate host-read answer with
// [ErrNotFound] or [ErrInvalidRequest]; callers that must work with them can
// fall back to [Client.Get].
func (c *Client) HostRead(ctx context.Context, request Request, file *os.File, options HostReadOptions) (HostReadResult, error) {
	const op = "host-read"

	o := options

	if file == nil {
		return HostReadResult{}, invalid(op, errors.New("nil file"))
	}

	if o.Offset < 0 || o.Length < 0 || o.Length > math.MaxInt64-o.Offset ||
		o.DestinationOffset < 0 || o.DestinationLength < 0 {
		return HostReadResult{}, invalid(op, errors.New("invalid range"))
	}

	if o.ETag != "" {
		if err := wire.ValidateETag(o.ETag); err != nil {
			return HostReadResult{}, invalid(op, err)
		}
	}

	if o.DestinationLength == 0 {
		info, err := file.Stat()
		if err != nil {
			return HostReadResult{}, invalid(op, err)
		}

		o.DestinationLength = info.Size() - o.DestinationOffset
		if o.DestinationLength <= 0 {
			return HostReadResult{}, invalid(op, errors.New("file has no room past the destination offset"))
		}
	}

	if o.DestinationLength > math.MaxInt64-o.DestinationOffset {
		return HostReadResult{}, invalid(op, errors.New("invalid destination window"))
	}

	if o.Length > o.DestinationLength {
		return HostReadResult{}, invalid(op, errors.New("destination is smaller than the range"))
	}

	r, err := request.wire(wire.OperationHead)
	if err != nil {
		return HostReadResult{}, err
	}

	r.Pin = o.ETag
	wo := wire.HostReadOptions{
		Offset:            uint64(o.Offset),
		Length:            uint64(o.Length),
		Pin:               o.ETag,
		DestinationOffset: uint64(o.DestinationOffset),
		DestinationLength: uint64(o.DestinationLength),
	}

	head, err := wire.HostReadHead(r, wo)
	if err != nil {
		return HostReadResult{}, invalid(op, err)
	}

	release, err := c.admit(ctx, &c.bulk, op)
	if err != nil {
		return HostReadResult{}, err
	}
	defer release()

	ctx, done := c.bind(ctx)
	defer done()

	conn, _, err := c.conn(ctx, op, true)
	if err != nil {
		return HostReadResult{}, err
	}
	defer closeQuietly(conn)

	stop := context.AfterFunc(ctx, func() { closeQuietly(conn) })
	defer stop()

	result, err := c.hostRead(conn, file, head, wo, o.OnPage)
	if err != nil {
		if ctx.Err() != nil {
			return HostReadResult{}, contextError(op, ctx)
		}

		return HostReadResult{}, ioFailure(op, truncation(err))
	}

	return result, nil
}

func (c *Client) hostRead(conn *clientConn, file *os.File, head []byte, o wire.HostReadOptions, onPage func(int64, int64)) (HostReadResult, error) {
	if err := conn.SetDeadline(time.Now().Add(c.limits.headerTimeout)); err != nil {
		return HostReadResult{}, err
	}

	if err := sendWithFile(conn.Conn, head, file); err != nil {
		return HostReadResult{}, err
	}

	raw, err := wire.ReadRawHead(conn.r, true)
	if err != nil {
		return HostReadResult{}, err
	}

	response, err := wire.ParseHostReadResponse(raw, o)
	if err != nil {
		return HostReadResult{}, err
	}

	seq := wire.NewSequence(response.First, response.End)

	var frame [wire.FrameSize]byte

	for {
		// Racer writes each page before its frame, so a frame may take as
		// long as a page fetch; bound each one like a Get body read.
		if err := conn.SetDeadline(time.Now().Add(c.limits.bodyTimeout)); err != nil {
			return HostReadResult{}, err
		}

		if _, err := io.ReadFull(conn.r, frame[:]); err != nil {
			return HostReadResult{}, err
		}

		f := wire.DecodeFrame(frame)
		if err := seq.Accept(f, "host-read"); err != nil {
			return HostReadResult{}, err
		}

		if f.Kind == wire.CompleteFrame {
			break
		}

		if onPage != nil {
			onPage(int64(f.Offset), int64(f.Length))
		}
	}

	return HostReadResult{
		Metadata: fromWireMetadata(response.Metadata),
		Offset:   int64(response.First),
		Length:   int64(response.End - response.First),
	}, nil
}

// sendWithFile writes head with file's descriptor attached as SCM_RIGHTS on
// the first sendmsg, then writes any remainder plainly.
func sendWithFile(conn net.Conn, head []byte, file *os.File) error {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return errors.New("descriptor passing needs a Unix socket connection")
	}

	raw, err := file.SyscallConn()
	if err != nil {
		return err
	}

	var (
		n       int
		sendErr error
	)

	// Send inside Control so the descriptor stays open during sendmsg.
	if err := raw.Control(func(fd uintptr) {
		n, _, sendErr = unixConn.WriteMsgUnix(head, syscall.UnixRights(int(fd)), nil)
	}); err != nil {
		return err
	}

	if sendErr != nil {
		return sendErr
	}

	if n < len(head) {
		_, err = unixConn.Write(head[n:])
	}

	return err
}
