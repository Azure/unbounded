// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func assertPipeClosed(t *testing.T, fd [2]int) {
	t.Helper()

	for _, f := range fd {
		if _, err := unix.FcntlInt(uintptr(f), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
			t.Errorf("pipe FD %d still open: %v", f, err)
		}
	}
}

func TestSplicePipeSizing(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(map[bool]string{false: "host", true: "denied"}[denied], func(t *testing.T) {
			var commands []int

			p, err := newSplicePipeWithFcntl(func(fd uintptr, cmd, value int) (int, error) {
				commands = append(commands, cmd)
				if cmd == unix.F_SETPIPE_SZ {
					if value != splicePipeSize {
						t.Fatalf("requested capacity %d", value)
					}

					if denied {
						return 0, unix.EPERM
					}
				}

				return unix.FcntlInt(fd, cmd, value)
			})
			if err != nil {
				t.Fatal(err)
			}
			defer p.close()

			actual, err := unix.FcntlInt(uintptr(p.fd[0]), unix.F_GETPIPE_SZ, 0)
			if err != nil || actual <= 0 || p.capacity != actual {
				t.Fatal(p.capacity, actual, err)
			}

			if len(commands) != 2 || commands[0] != unix.F_SETPIPE_SZ || commands[1] != unix.F_GETPIPE_SZ {
				t.Fatal(commands)
			}

			for _, fd := range p.fd {
				flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
				if err != nil || flags&unix.O_NONBLOCK == 0 {
					t.Fatal("blocking pipe", flags, err)
				}

				flags, err = unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
				if err != nil || flags&unix.FD_CLOEXEC == 0 {
					t.Fatal("inheritable pipe", flags, err)
				}
			}
		})
	}
}

func TestSplicePipeCapacityFailureClosesFDs(t *testing.T) {
	for _, queryErr := range []error{unix.EIO, nil} {
		var fd [2]int

		p, err := newSplicePipeWithFcntl(func(f uintptr, cmd, _ int) (int, error) {
			// Pipe2 allocates the lowest two free descriptors in this serial test.
			// Discover the read end through the pipe's identity rather than relying
			// on descriptor adjacency.
			if cmd == unix.F_SETPIPE_SZ {
				fd[1] = int(f)

				var want unix.Stat_t
				if err := unix.Fstat(int(f), &want); err != nil {
					t.Fatal(err)
				}

				for i := 0; i < int(f); i++ {
					var stat unix.Stat_t
					if unix.Fstat(i, &stat) == nil && stat.Ino == want.Ino && stat.Dev == want.Dev {
						fd[0] = i
						break
					}
				}

				return 0, unix.EPERM
			}

			return 0, queryErr
		})
		if err == nil || p != nil || fd[0] == 0 {
			t.Fatal(p, err, fd)
		}

		assertPipeClosed(t, fd)
	}
}

func TestSplicePipePoolBoundsAndCleanup(t *testing.T) {
	for _, limit := range []int{0, 1, maxIdleSplicePipes + 1} {
		pool := &splicePipePool{}
		t.Cleanup(pool.closeIdle)

		var pipes []*splicePipe

		for range maxIdleSplicePipes + 2 {
			p, err := pool.get()
			if err != nil {
				t.Fatal(err)
			}

			pipes = append(pipes, p)
		}

		for i, p := range pipes {
			pool.put(p, true, limit)

			if i >= min(limit, maxIdleSplicePipes) {
				assertPipeClosed(t, p.fd)
			}
		}

		if len(pool.idle) != min(limit, maxIdleSplicePipes) {
			t.Fatal("unbounded cache", len(pool.idle))
		}

		pool.closeIdle()

		for _, p := range pipes {
			assertPipeClosed(t, p.fd)
		}
	}
}

func TestSplicePipeDefaultIdleCapacity(t *testing.T) {
	const idle = 8

	c, err := NewClient("/unused", ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(c.CloseIdleConnections)

	var pipes []*splicePipe
	for range idle + 1 {
		p, err := c.streamPool.pipes.get()
		if err != nil {
			t.Fatal(err)
		}

		pipes = append(pipes, p)
	}

	for _, p := range pipes {
		c.streamPool.pipes.put(p, true, c.streamPool.limit)
	}

	if len(c.streamPool.pipes.idle) != min(idle, maxIdleSplicePipes) {
		t.Fatal("unexpected pipe cache capacity", len(c.streamPool.pipes.idle))
	}

	for _, p := range pipes[min(idle, maxIdleSplicePipes):] {
		assertPipeClosed(t, p.fd)
	}

	c.CloseIdleConnections()

	for _, p := range pipes {
		assertPipeClosed(t, p.fd)
	}
}

func TestSplicePipePoolRejectsNonemptyAndFailed(t *testing.T) {
	for _, nonempty := range []bool{false, true} {
		pool := &splicePipePool{}
		t.Cleanup(pool.closeIdle)

		p, err := pool.get()
		if err != nil {
			t.Fatal(err)
		}

		if nonempty {
			n, err := unix.Write(p.fd[1], []byte("unforwarded"))
			if err != nil {
				t.Fatal(err)
			}

			p.buffered = int64(n)
		}

		pool.put(p, nonempty, 1)

		if len(pool.idle) != 0 {
			t.Fatal("unusable pipe cached")
		}

		assertPipeClosed(t, p.fd)
	}
}

func TestSplicePipePoolSharedCleanup(t *testing.T) {
	c, err := NewClient("/unused", ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseIdleConnections()

	view, err := c.WithOriginData([]byte("request"))
	if err != nil {
		t.Fatal(err)
	}

	pool := &c.streamPool.pipes

	idle, err := pool.get()
	if err != nil {
		t.Fatal(err)
	}

	active, err := pool.get()
	if err != nil {
		idle.close()
		t.Fatal(err)
	}

	pool.put(idle, true, 1)
	view.CloseIdleConnections()
	assertPipeClosed(t, idle.fd)

	if _, err := unix.Write(active.fd[1], []byte("x")); err != nil {
		t.Fatal("cleanup interrupted active pipe", err)
	}

	var b [1]byte
	if n, err := unix.Read(active.fd[0], b[:]); err != nil || n != 1 || b[0] != 'x' {
		t.Fatal(n, err, b)
	}

	pool.put(active, true, 1)
	assertPipeClosed(t, active.fd)

	if len(pool.idle) != 0 {
		t.Fatal("old checkout repopulated cache")
	}

	p, err := pool.get()
	if err != nil {
		t.Fatal(err)
	}

	pool.put(p, true, 1)

	reused, err := pool.get()
	if err != nil || reused != p {
		t.Fatal("new generation did not reuse", err)
	}

	pool.put(reused, true, 1)
	c.CloseIdleConnections()
	assertPipeClosed(t, p.fd)
}

func TestSplicePipePoolConcurrentCleanup(t *testing.T) {
	pool := &splicePipePool{}
	defer pool.closeIdle()

	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 20 {
				p, err := pool.get()
				if err != nil {
					t.Error(err)
					return
				}

				if _, err := unix.Write(p.fd[1], []byte("x")); err != nil {
					t.Error(err)
				}

				var b [1]byte
				if n, err := unix.Read(p.fd[0], b[:]); err != nil || n != 1 || b[0] != 'x' {
					t.Error(n, err, b)
				}

				pool.put(p, true, 2)
			}
		})
	}

	wg.Go(func() {
		for range 20 {
			pool.closeIdle()
		}
	})
	wg.Wait()
}

func TestSplicePipeUnreachableCleanup(t *testing.T) {
	fd := func() [2]int {
		p, err := newSplicePipe()
		if err != nil {
			t.Fatal(err)
		}

		return p.fd
	}()
	deadline := time.Now().Add(5 * time.Second)

	for {
		runtime.GC()

		_, first := unix.FcntlInt(uintptr(fd[0]), unix.F_GETFD, 0)

		_, second := unix.FcntlInt(uintptr(fd[1]), unix.F_GETFD, 0)
		if errors.Is(first, unix.EBADF) && errors.Is(second, unix.EBADF) {
			return
		}

		if time.Now().After(deadline) {
			t.Fatal("unreachable pipe FDs not reclaimed", first, second)
		}

		time.Sleep(time.Millisecond)
	}
}

func TestSpliceLazyPipe(t *testing.T) {
	for _, size := range []int64{0, 137} {
		c := generatedStreamClient(t, size, nil)

		o, err := c.Open(t.Context(), "/blob")
		if err != nil {
			t.Fatal(err)
		}

		s, err := o.Stream(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()

		if err := s.Prepare(); err != nil {
			t.Fatal(err)
		}

		if size > 0 {
			if _, err := s.conn.reader.Peek(int(size)); err != nil {
				t.Fatal(err)
			}
		}

		dst, receiver := downstreamPair(t, "unix")
		n, err := s.WriteTo(dst)
		_ = dst.Close()

		body, readErr := io.ReadAll(receiver)
		if err != nil || readErr != nil || n != size || !bytes.Equal(body, make([]byte, size)) {
			t.Fatal(n, err, readErr, len(body))
		}

		if len(c.streamPool.pipes.idle) != 0 || s.Stats().SpliceCalls != 0 {
			t.Fatal("buffered or empty transfer acquired a pipe")
		}
	}
}

func TestSpliceFailureClosesCheckedOutPipe(t *testing.T) {
	for _, action := range []string{"cancel", "close", "deadline", "disconnect"} {
		t.Run(action, func(t *testing.T) {
			c := generatedStreamClient(t, 2*PageSize, nil)

			o, err := c.Open(t.Context(), "/blob")
			if err != nil {
				t.Fatal(err)
			}

			p, err := c.streamPool.pipes.get()
			if err != nil {
				t.Fatal(err)
			}

			c.streamPool.pipes.put(p, true, c.streamPool.limit)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			s, err := o.Stream(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()

			dst, receiver := downstreamPair(t, "unix")
			_ = dst.(*net.UnixConn).SetWriteBuffer(4096)
			_ = receiver.SetReadDeadline(time.Now().Add(5 * time.Second))

			type result struct {
				n   int64
				err error
			}

			done := make(chan result, 1)

			go func() { n, err := s.WriteTo(dst); done <- result{n, err} }()
			// More than header read-ahead guarantees that the seeded pipe is in use.
			prefix := make([]byte, 32<<10)
			if _, err := io.ReadFull(receiver, prefix); err != nil {
				t.Fatal(err)
			}

			switch action {
			case "cancel":
				cancel()
			case "close":
				_ = s.Close()
			case "deadline":
				_ = dst.SetWriteDeadline(time.Now())
			case "disconnect":
				_ = receiver.Close()
			}

			select {
			case got := <-done:
				if got.err == nil || got.n < int64(len(prefix)) || got.n >= 2*PageSize {
					t.Fatal(got)
				}

				if (action == "cancel" || action == "close") && !errors.Is(got.err, context.Canceled) {
					t.Fatal(got.err)
				}

				_ = dst.Close()

				if action != "disconnect" {
					rest, err := io.ReadAll(receiver)
					if err != nil || int64(len(prefix)+len(rest)) != got.n || !bytes.Equal(rest, make([]byte, len(rest))) {
						t.Fatal("incorrect partial bytes", got.n, len(rest), err)
					}
				}

				if !bytes.Equal(prefix, make([]byte, len(prefix))) {
					t.Fatal("corrupt prefix")
				}

				if len(c.streamPool.pipes.idle) != 0 {
					t.Fatal("failed pipe cached")
				}

				assertPipeClosed(t, p.fd)
			case <-time.After(5 * time.Second):
				t.Fatal("transfer did not stop")
			}
		})
	}
}

func TestSplicePartialRangeAndActiveCleanup(t *testing.T) {
	data := payload(2 << 20)

	const offset, length = 13, 1500000

	resume := make(chan struct{})

	var resumeOnce sync.Once

	unblock := func() { resumeOnce.Do(func() { close(resume) }) }
	defer unblock()

	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", checksumTag(data))
		w.Header().Set("Content-Type", "application/octet-stream")

		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
			return
		}

		if r.Header.Get("Range") != fmt.Sprintf("bytes=%d-%d", offset, offset+length-1) {
			t.Error("incorrect range", r.Header.Get("Range"))
		}

		w.Header().Set("Content-Length", fmt.Sprint(length))
		w.Header().Set("Content-Range", contentRange(offset, offset+length-1, int64(len(data))))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[offset : offset+(32<<10)])
		w.(http.Flusher).Flush()

		select {
		case <-resume:
		case <-r.Context().Done():
			return
		}

		_, _ = w.Write(data[offset+(32<<10) : offset+length])
	}), ClientOptions{})

	view, err := c.WithOriginData([]byte("scoped"))
	if err != nil {
		t.Fatal(err)
	}

	o, err := view.Open(t.Context(), "/blob")
	if err != nil {
		t.Fatal(err)
	}

	p, err := c.streamPool.pipes.get()
	if err != nil {
		t.Fatal(err)
	}

	c.streamPool.pipes.put(p, true, c.streamPool.limit)

	s, err := o.ReadRange(t.Context(), offset, length)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	dst, receiver := downstreamPair(t, "unix")
	_ = receiver.SetReadDeadline(time.Now().Add(5 * time.Second))

	type result struct {
		n   int64
		err error
	}

	done := make(chan result, 1)

	go func() { n, err := s.WriteTo(dst); done <- result{n, err} }()

	body := make([]byte, length)
	if _, err := io.ReadFull(receiver, body[:32<<10]); err != nil {
		t.Fatal(err)
	}

	view.CloseIdleConnections()
	unblock()

	if _, err := io.ReadFull(receiver, body[32<<10:]); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-done:
		if got.err != nil || got.n != length || !bytes.Equal(body, data[offset:offset+length]) {
			t.Fatal("partial range changed during cleanup", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup blocked active transfer")
	}

	if len(c.streamPool.pipes.idle) != 0 {
		t.Fatal("active pipe repopulated closed cache generation")
	}

	assertPipeClosed(t, p.fd)
}

func TestSpliceTruncatedSourceClosesPipe(t *testing.T) {
	data := payload(100000)
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", checksumTag(data))
		w.Header().Set("Content-Length", fmt.Sprint(len(data)+1))

		if r.Method == "HEAD" {
			return
		}

		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()

		_, _ = fmt.Fprintf(conn, "HTTP/1.1 206 Partial Content\r\nETag: %s\r\nContent-Length: %d\r\nContent-Range: bytes 0-%d/%d\r\n\r\n", checksumTag(data), len(data)+1, len(data), len(data)+1)
		_, _ = conn.Write(data)
	}), ClientOptions{})

	o, err := c.Open(t.Context(), "/blob")
	if err != nil {
		t.Fatal(err)
	}

	p, err := c.streamPool.pipes.get()
	if err != nil {
		t.Fatal(err)
	}

	c.streamPool.pipes.put(p, true, c.streamPool.limit)

	s, err := o.Stream(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	dst, receiver := downstreamPair(t, "unix")
	result := make(chan []byte, 1)

	go func() { body, _ := io.ReadAll(receiver); result <- body }()

	n, err := s.WriteTo(dst)
	_ = dst.Close()

	if !errors.Is(err, io.ErrUnexpectedEOF) || n != int64(len(data)) || !bytes.Equal(<-result, data) {
		t.Fatal("incorrect truncated transfer", n, err)
	}

	if s.Stats().SpliceBytes == 0 || len(c.streamPool.pipes.idle) != 0 {
		t.Fatal("truncated source pipe reused", s.Stats())
	}

	assertPipeClosed(t, p.fd)
}
