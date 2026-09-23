// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Adapted from the removed racer-object adapter. RawConn integrates EAGAIN with
// Go's poller, so socket deadlines and cancellation interrupt blocked splices.
type splicePipe struct{ fd [2]int }

func newSplicePipe() (*splicePipe, error) {
	p := &splicePipe{}
	if err := unix.Pipe2(p.fd[:], unix.O_CLOEXEC|unix.O_NONBLOCK); err != nil {
		return nil, err
	}

	return p, nil
}

func (p *splicePipe) close() {
	_ = unix.Close(p.fd[0]) //nolint:errcheck // Cleanup must preserve the transfer error.
	_ = unix.Close(p.fd[1]) //nolint:errcheck // Cleanup must preserve the transfer error.
}

func spliceReady(socket syscall.RawConn, write bool, pipe, count int, stats *TransferStats) (int64, error) {
	var (
		n     int64
		opErr error
	)

	callback := func(fd uintptr) bool {
		for {
			stats.SpliceCalls++

			if write {
				n, opErr = unix.Splice(pipe, nil, int(fd), nil, count, unix.SPLICE_F_NONBLOCK)
			} else {
				n, opErr = unix.Splice(int(fd), nil, pipe, nil, count, unix.SPLICE_F_NONBLOCK)
			}

			if errors.Is(opErr, unix.EINTR) {
				continue
			}

			return !errors.Is(opErr, unix.EAGAIN)
		}
	}

	var err error
	if write {
		err = socket.Write(callback)
	} else {
		err = socket.Read(callback)
	}

	if err != nil {
		return max(n, 0), err
	}

	return max(n, 0), opErr
}

func (s *Stream) spliceTo(dst io.Writer) (int64, error, bool) {
	var (
		conn net.Conn
		raw  syscall.RawConn
		err  error
	)

	switch c := dst.(type) {
	case *net.TCPConn:
		conn = c
		raw, err = c.SyscallConn()
	case *net.UnixConn:
		conn = c
		raw, err = c.SyscallConn()
	default:
		return 0, nil, false
	}

	if err != nil {
		return 0, s.fail(err), true
	}
	// Only cancellation changes the caller's write deadline. A canceled
	// downstream must be closed by its owner, not reused for another HTTP response.
	done := make(chan struct{})
	stop := context.AfterFunc(s.ctx, func() {
		if err := conn.SetWriteDeadline(time.Now()); err != nil {
			_ = conn.Close() //nolint:errcheck // Cancellation must interrupt the downstream write.
		}

		close(done)
	})

	defer func() {
		if !stop() {
			<-done
		}
	}()

	p, err := newSplicePipe()
	if err != nil {
		return 0, s.fail(err), true
	}
	defer p.close()

	var tee *splicePipe
	if s.digest != nil {
		tee, err = newSplicePipe()
		if err != nil {
			return 0, s.fail(err), true
		}
		defer tee.close()
	}

	buf := copyBuffers.Get().(*[]byte) //nolint:errcheck // The private pool only contains *[]byte.
	defer copyBuffers.Put(buf)

	var total int64

	for {
		if s.closed {
			return total, net.ErrClosed, true
		}

		if s.err != nil {
			return total, s.err, true
		}

		if err := s.ctx.Err(); err != nil {
			return total, s.fail(err), true
		}

		if s.offset == s.end {
			err := s.finish()
			if err == io.EOF {
				err = nil
			}

			return total, err, true
		}

		if err := s.nextPage(); err != nil {
			return total, s.fail(err), true
		}

		remaining := s.pageEnd - s.offset
		if s.digest != nil {
			remaining = min(remaining, s.end-s.offset-1)
		}
		// Drain only bytes the HTTP header reader already consumed. Do not
		// read another buffer from the socket before switching to splice.
		buffered := min(int64(s.conn.reader.Buffered()), remaining)
		if buffered > 0 || remaining == 0 {
			length := min(buffered, int64(len(*buf)))
			if remaining == 0 {
				length = 1
			}

			n, err := s.read((*buf)[:length])
			if err != nil && err != io.EOF {
				return total, err, true
			}

			written, writeErr := conn.Write((*buf)[:n])

			total += int64(written)
			if writeErr == nil && written != n {
				writeErr = io.ErrShortWrite
			}

			if writeErr != nil {
				return total, s.fail(writeErr), true
			}

			continue
		}

		source, ok := s.conn.Conn.(*net.UnixConn)
		if !ok {
			return total, s.fail(ErrProtocol), true
		}

		r, err := source.SyscallConn()
		if err != nil {
			return total, s.fail(err), true
		}

		n, err := spliceReady(r, false, p.fd[1], int(min(remaining, 1<<20)), &s.stats)
		if err != nil {
			return total, s.fail(err), true
		}

		if n == 0 {
			return total, s.fail(io.ErrUnexpectedEOF), true
		}

		for n > 0 {
			ready := n
			if tee != nil {
				ready, err = s.teeHash(p, tee, int(n), *buf)
				if err != nil {
					return total, s.fail(err), true
				}
			}
			// Consume exactly the prefix duplicated by tee before teeing again.
			// Repeating tee without this drain would hash the same bytes twice.
			for ready > 0 {
				written, err := spliceReady(raw, true, p.fd[0], int(ready), &s.stats)
				total += written
				s.offset += written
				s.stats.SpliceBytes += written
				n -= written
				ready -= written

				if err != nil {
					return total, s.fail(err), true
				}

				if written == 0 {
					return total, s.fail(io.ErrNoProgress), true
				}
			}
		}
	}
}

func (s *Stream) teeHash(src, dst *splicePipe, count int, buf []byte) (int64, error) {
	var (
		n   int64
		err error
	)

	for {
		s.stats.TeeCalls++

		n, err = unix.Tee(src.fd[0], dst.fd[1], count, unix.SPLICE_F_NONBLOCK)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}

	if err != nil {
		return 0, err
	}

	if n == 0 {
		return 0, io.ErrNoProgress
	}

	s.stats.TeeBytes += n
	for left := n; left > 0; {
		read, err := unix.Read(dst.fd[0], buf[:min(left, int64(len(buf)))])
		if errors.Is(err, unix.EINTR) {
			continue
		}

		if err != nil {
			return 0, err
		}

		if read == 0 {
			return 0, io.ErrUnexpectedEOF
		}

		_, _ = s.digest.Write(buf[:read])
		left -= int64(read)
	}

	return n, nil
}
