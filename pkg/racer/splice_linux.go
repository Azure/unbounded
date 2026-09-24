// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	splicePipeSize     = 1 << 20
	maxIdleSplicePipes = 8
)

type splicePipe struct {
	fd         [2]int
	capacity   int
	buffered   int64
	generation uint64
	cleanup    runtime.Cleanup
}

func newSplicePipe() (*splicePipe, error) {
	return newSplicePipeWithFcntl(unix.FcntlInt)
}

func newSplicePipeWithFcntl(fcntl func(uintptr, int, int) (int, error)) (*splicePipe, error) {
	p := &splicePipe{}
	if err := unix.Pipe2(p.fd[:], unix.O_CLOEXEC|unix.O_NONBLOCK); err != nil {
		return nil, err
	}

	// Enlargement is only an optimization: quotas and unprivileged limits may
	// deny it. Always query the actual capacity, even when enlargement succeeds.
	_, _ = fcntl(uintptr(p.fd[1]), unix.F_SETPIPE_SZ, splicePipeSize) //nolint:errcheck // Enlargement is best-effort; query the actual capacity below.

	capacity, err := fcntl(uintptr(p.fd[1]), unix.F_GETPIPE_SZ, 0)
	if err != nil || capacity <= 0 {
		closeSpliceFDs(p.fd)

		if err == nil {
			err = unix.EINVAL
		}

		return nil, err
	}

	p.capacity = capacity
	// Explicit release handles normal lifetimes. Cleanup also owns FDs if an
	// entire client and its idle cache become unreachable without a close call.
	p.cleanup = runtime.AddCleanup(p, closeSpliceFDs, p.fd)

	return p, nil
}

func (p *splicePipe) close() {
	p.cleanup.Stop()
	closeSpliceFDs(p.fd)
	runtime.KeepAlive(p)
}

func closeSpliceFDs(fd [2]int) {
	_ = unix.Close(fd[0]) //nolint:errcheck // Cleanup must preserve the transfer error.
	_ = unix.Close(fd[1]) //nolint:errcheck // Cleanup must preserve the transfer error.
}

// Only exclusively owned, successfully drained pipes enter this bounded cache.
// A generation change prevents active transfers from undoing closeIdle.
type splicePipePool struct {
	mu         sync.Mutex
	idle       []*splicePipe
	generation uint64
}

func (pool *splicePipePool) get() (*splicePipe, error) {
	pool.mu.Lock()
	if n := len(pool.idle); n > 0 {
		p := pool.idle[n-1]
		pool.idle[n-1] = nil
		pool.idle = pool.idle[:n-1]
		pool.mu.Unlock()

		return p, nil
	}

	generation := pool.generation
	pool.mu.Unlock()

	p, err := newSplicePipe()
	if err == nil {
		p.generation = generation
	}

	return p, err
}

func (pool *splicePipePool) put(p *splicePipe, reusable bool, limit int) {
	pool.mu.Lock()

	if !reusable || p.buffered != 0 || p.generation != pool.generation || len(pool.idle) >= min(limit, maxIdleSplicePipes) {
		pool.mu.Unlock()
		p.close()

		return
	}

	pool.idle = append(pool.idle, p)
	pool.mu.Unlock()
}

func (pool *splicePipePool) closeIdle() {
	pool.mu.Lock()

	pool.generation++
	idle := pool.idle
	pool.idle = nil
	pool.mu.Unlock()

	for _, p := range idle {
		p.close()
	}
}

// RawConn integrates EAGAIN with Go's poller, so socket deadlines and
// cancellation interrupt blocked splices.
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

	var p *splicePipe

	reusable := false
	pool := s.object.client.streamPool

	defer func() {
		if p != nil {
			pool.pipes.put(p, reusable && s.ctx.Err() == nil, pool.limit)
		}
	}()

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

			reusable = err == nil

			return total, err, true
		}

		if err := s.nextPage(); err != nil {
			return total, s.fail(err), true
		}

		remaining := s.pageEnd - s.offset
		// Drain only bytes the HTTP header reader already consumed. Do not
		// read another buffer from the socket before switching to splice.
		buffered := min(int64(s.conn.reader.Buffered()), remaining)
		if buffered > 0 {
			length := min(buffered, int64(len(*buf)))

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

		if p == nil {
			p, err = pool.pipes.get()
			if err != nil {
				return total, s.fail(err), true
			}
		}

		n, err := spliceReady(r, false, p.fd[1], int(min(remaining, int64(p.capacity), splicePipeSize)), &s.stats)
		p.buffered += n

		if err != nil {
			return total, s.fail(err), true
		}

		if n == 0 {
			return total, s.fail(io.ErrUnexpectedEOF), true
		}

		for p.buffered > 0 {
			written, err := spliceReady(raw, true, p.fd[0], int(p.buffered), &s.stats)
			total += written
			s.offset += written
			s.stats.SpliceBytes += written
			p.buffered -= written

			if err != nil {
				return total, s.fail(err), true
			}

			if written == 0 {
				return total, s.fail(io.ErrNoProgress), true
			}
		}
	}
}
