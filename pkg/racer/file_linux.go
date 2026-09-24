// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"errors"
	"io"
	"net"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

type fileSpliceFunc func(int, *int64, int, *int64, int, int) (int64, error)

func unsupportedFileSplice(err error) bool {
	return errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EXDEV)
}

func (s *Stream) spliceToFile(dst *os.File, offset int64) (int64, error) {
	return s.spliceToFileWith(dst, offset, unix.Splice)
}

// The syscall parameter is per transfer, so fault-injection tests never replace
// a process-global hook while other transfers are active.
func (s *Stream) spliceToFileWith(dst *os.File, offset int64, splice fileSpliceFunc) (int64, error) {
	raw, err := dst.SyscallConn()
	if err != nil {
		return 0, s.fail(err)
	}

	var (
		flags int
		opErr error
	)

	if err := raw.Control(func(fd uintptr) { flags, opErr = unix.FcntlInt(fd, unix.F_GETFL, 0) }); err != nil {
		return 0, s.fail(err)
	}

	if opErr != nil {
		return 0, s.fail(opErr)
	}

	if flags&unix.O_APPEND != 0 {
		return 0, s.fail(errors.New("racer: file destination uses O_APPEND"))
	}

	pool := s.object.client.streamPool

	var p *splicePipe

	reusable := false

	defer func() {
		if p != nil {
			pool.pipes.put(p, reusable && s.ctx.Err() == nil, pool.limit)
		}
	}()

	buf := copyBuffers.Get().(*[]byte) //nolint:errcheck // The private pool only contains *[]byte.
	defer copyBuffers.Put(buf)

	var total int64

	fallback := func() (int64, error) {
		// Socket bytes already in the pipe must precede any further socket read.
		for p != nil && p.buffered > 0 {
			if err := s.ctx.Err(); err != nil {
				return total, s.fail(err)
			}

			n, err := unix.Read(p.fd[0], (*buf)[:min(p.buffered, int64(len(*buf)))])
			if errors.Is(err, unix.EINTR) {
				continue
			}

			if err != nil {
				return total, s.fail(err)
			}

			if n == 0 {
				return total, s.fail(io.ErrNoProgress)
			}

			p.buffered -= int64(n)
			s.stats.BufferedBytes += int64(n)
			written, err := dst.WriteAt((*buf)[:n], offset+total)
			total += int64(written)

			s.offset += int64(written)
			if err == nil && written != n {
				err = io.ErrShortWrite
			}

			if err != nil {
				return total, s.fail(err)
			}
		}

		n, err := s.copyToFileBuffer(dst, offset+total, *buf)
		reusable = err == nil

		return total + n, err
	}

	for {
		if s.closed {
			return total, net.ErrClosed
		}

		if s.err != nil {
			return total, s.err
		}

		if err := s.ctx.Err(); err != nil {
			return total, s.fail(err)
		}

		if s.offset == s.end {
			if err := s.finish(); err != io.EOF {
				return total, err
			}

			reusable = true

			return total, nil
		}

		if err := s.nextPage(); err != nil {
			return total, s.fail(err)
		}

		remaining := s.pageEnd - s.offset
		if buffered := min(int64(s.conn.reader.Buffered()), remaining); buffered > 0 {
			n, err := s.conn.reader.Read((*buf)[:min(buffered, int64(len(*buf)))])
			if err != nil {
				return total, s.fail(err)
			}

			s.stats.BufferedBytes += int64(n)
			written, err := dst.WriteAt((*buf)[:n], offset+total)
			total += int64(written)

			s.offset += int64(written)
			if err == nil && written != n {
				err = io.ErrShortWrite
			}

			if err != nil {
				return total, s.fail(err)
			}

			continue
		}

		source, ok := s.conn.Conn.(syscall.Conn)
		if !ok {
			return fallback()
		}

		socket, err := source.SyscallConn()
		if err != nil {
			return total, s.fail(err)
		}

		if p == nil {
			p, err = pool.pipes.get()
			if unsupportedFileSplice(err) {
				return fallback()
			}

			if err != nil {
				return total, s.fail(err)
			}
		}

		var n int64

		err = socket.Read(func(fd uintptr) bool {
			for {
				s.stats.SpliceCalls++

				n, opErr = splice(int(fd), nil, p.fd[1], nil, int(min(remaining, int64(p.capacity))), unix.SPLICE_F_NONBLOCK)
				if !errors.Is(opErr, unix.EINTR) {
					return !errors.Is(opErr, unix.EAGAIN)
				}
			}
		})
		p.buffered += max(n, 0)

		if err == nil {
			err = opErr
		}

		if unsupportedFileSplice(err) {
			return fallback()
		}

		if err != nil {
			return total, s.fail(err)
		}

		if n == 0 {
			return total, s.fail(io.ErrUnexpectedEOF)
		}

		for p.buffered > 0 {
			if err := s.ctx.Err(); err != nil {
				return total, s.fail(err)
			}

			position := offset + total
			n = 0
			err = raw.Control(func(fd uintptr) {
				for {
					s.stats.SpliceCalls++

					n, opErr = splice(p.fd[0], nil, int(fd), &position, int(p.buffered), unix.SPLICE_F_NONBLOCK)
					if !errors.Is(opErr, unix.EINTR) {
						break
					}
				}
			})
			n = max(n, 0)
			total += n
			s.offset += n
			p.buffered -= n
			s.stats.SpliceBytes += n

			if err == nil {
				err = opErr
			}

			if unsupportedFileSplice(err) {
				return fallback()
			}

			if err != nil {
				return total, s.fail(err)
			}

			if n == 0 {
				return total, s.fail(io.ErrNoProgress)
			}
		}
	}
}
