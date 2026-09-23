// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// A pipe belongs to one connection. Successful transfers empty it before reuse;
// failure closes both sockets and the pipe. No userspace payload fallback exists.
type splicePipe struct{ fd [2]int }

func newSplicePipe() (*splicePipe, error) {
	p := &splicePipe{}
	if err := unix.Pipe2(p.fd[:], unix.O_CLOEXEC|unix.O_NONBLOCK); err != nil {
		return nil, err
	}
	// Capacity tuning is optional; unprivileged kernels may retain a smaller pipe.
	if _, err := unix.FcntlInt(uintptr(p.fd[0]), unix.F_SETPIPE_SZ, 1<<20); err != nil {
		slog.Debug("using default pipe capacity", "error", err)
	}

	return p, nil
}

func (p *splicePipe) close() {
	for _, fd := range p.fd {
		if err := unix.Close(fd); err != nil {
			slog.Debug("close pipe", "error", err)
		}
	}
}

func (p *splicePipe) transfer(dst *net.TCPConn, src *net.UnixConn, length int64) (int64, error) {
	r, err := src.SyscallConn()
	if err != nil {
		return 0, err
	}

	w, err := dst.SyscallConn()
	if err != nil {
		return 0, err
	}

	var total int64
	for total < length {
		n, err := spliceReady(r, false, p.fd[1], int(min(length-total, 1<<20)))
		if err != nil {
			return total, err
		}

		if n == 0 {
			return total, io.ErrUnexpectedEOF
		}

		for n > 0 {
			written, err := spliceReady(w, true, p.fd[0], int(n))
			total += written
			n -= written

			if err != nil {
				return total, err
			}

			if written == 0 {
				return total, io.ErrNoProgress
			}
		}
	}

	return total, nil
}

func spliceReady(socket syscall.RawConn, write bool, pipe int, count int) (int64, error) {
	var (
		n     int64
		opErr error
	)

	callback := func(fd uintptr) bool {
		for {
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
