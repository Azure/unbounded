// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package racersdk

import (
	"context"
	"errors"
	"io"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func spliceBody(ctx context.Context, source *responseBody, sink *FDSink, length int64) (int64, bool, error) {
	src, ok := source.conn.Conn.(syscall.Conn)
	if !ok {
		return 0, false, nil
	}

	dst, ok := sink.connection.(syscall.Conn)
	if !ok {
		return 0, false, nil
	}

	input, err := src.SyscallConn()
	if err != nil {
		return 0, false, nil
	}

	output, err := dst.SyscallConn()
	if err != nil {
		return 0, false, nil
	}

	pipe := []int{-1, -1}
	if err := unix.Pipe2(pipe, unix.O_CLOEXEC|unix.O_NONBLOCK); err != nil {
		return 0, false, nil
	}

	// A UID under pipe pressure may receive an 8 KiB default. Request a bounded
	// full chunk; denied growth is harmless because splice handles short counts.
	prepareSplicePipe(pipe[1])

	defer func() {
		if err := unix.Close(pipe[0]); err != nil {
			return
		}
	}()
	defer func() {
		if err := unix.Close(pipe[1]); err != nil {
			return
		}
	}()

	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(stopped)

		if err := source.conn.SetReadDeadline(time.Now()); err != nil {
			closeBody(source)
		}

		if err := sink.connection.SetWriteDeadline(time.Now()); err != nil {
			closeBody(sink.connection)
		}
	})

	defer func() {
		if !stop() {
			<-stopped
		}
	}()

	var sent int64
	for sent < length {
		if err := ctx.Err(); err != nil {
			return sent, true, err
		}

		var (
			count   int64
			callErr error
		)

		err := input.Read(func(fd uintptr) bool {
			for {
				count, callErr = unix.Splice(int(fd), nil, pipe[1], nil, int(min(length-sent, 64*1024)), unix.SPLICE_F_NONBLOCK)
				if errors.Is(callErr, unix.EINTR) {
					continue
				}

				return !errors.Is(callErr, unix.EAGAIN)
			}
		})
		if err != nil {
			return sent, true, err
		}

		if callErr != nil {
			if sent == 0 && (errors.Is(callErr, unix.EINVAL) || errors.Is(callErr, unix.ENOSYS)) {
				return 0, false, nil
			}

			return sent, true, callErr
		}

		if count == 0 {
			return sent, true, io.ErrUnexpectedEOF
		}

		source.mu.Lock()
		source.remaining -= count
		source.mu.Unlock()
		source.client.stats.bytesRead.Add(uint64(count))

		for count > 0 {
			var n int64

			err = output.Write(func(fd uintptr) bool {
				for {
					n, callErr = unix.Splice(pipe[0], nil, int(fd), nil, int(count), unix.SPLICE_F_NONBLOCK)
					if errors.Is(callErr, unix.EINTR) {
						continue
					}

					return !errors.Is(callErr, unix.EAGAIN)
				}
			})
			if err != nil {
				return sent, true, err
			}

			if callErr != nil {
				return sent, true, callErr
			}

			if n == 0 {
				return sent, true, io.ErrNoProgress
			}

			count -= n
			sent += n
			sink.spliced += n
		}
	}

	return sent, true, nil
}

func prepareSplicePipe(fd int) {
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETPIPE_SZ, 64*1024); err != nil {
		return // Best effort; the existing pipe remains usable.
	}
}
