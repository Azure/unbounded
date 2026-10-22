// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

// deliveryMemory creates a memfd holding data and returns a read-only
// descriptor for it, as Racer passes to clients.
func deliveryMemory(t testing.TB, data []byte) int {
	t.Helper()

	fd, err := unix.MemfdCreate("racer-test", unix.MFD_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFDs([]int{fd})

	if _, err := unix.Pwrite(fd, data, 0); err != nil {
		t.Fatal(err)
	}

	ro, err := unix.Open("/proc/self/fd/"+strconv.Itoa(fd), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { closeFDs([]int{ro}) })

	return ro
}

// mappedHead is a valid mapped subscription response for [first, end) of an
// object with the given size and ETag "v".
func mappedHead(size, first, end uint64) string {
	pages := wire.PageCount(first, end)

	return mappedHeadLength(size, first, end, wire.MappedFrameSize*pages+wire.FrameSize)
}

func mappedHeadLength(size, first, end, length uint64) string {
	return fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nContent-Length: %d\r\nETag: \"v\"\r\n"+
		"Racer-Expires-At: 0\r\nRacer-Object-Length: %d\r\nRacer-Range-Start: %d\r\nRacer-Range-End: %d\r\n"+
		"Racer-Delivery: mapped\r\nConnection: close\r\n\r\n", length, size, first, end)
}

// sendHead writes head with fds attached to its first byte.
func sendHead(conn net.Conn, head string, fds ...int) error {
	var oob []byte
	if len(fds) != 0 {
		oob = unix.UnixRights(fds...)
	}

	n, _, err := conn.(*net.UnixConn).WriteMsgUnix([]byte(head), oob, nil) //nolint:forcetypeassert // rawServer is a Unix socket.
	if err == nil && n != len(head) {
		err = io.ErrShortWrite
	}

	return err
}

func writeMappedFrame(w io.Writer, number, offset uint64, length uint32, memory uint64) error {
	b := wire.Frame{Kind: wire.MappedFrame, Number: number, Offset: offset, Length: length, Memory: memory}.EncodeMapped()
	_, err := w.Write(b[:])

	return err
}

func requireMapped(t testing.TB, head []byte) bool {
	t.Helper()

	if !strings.Contains(string(head), "\r\nRacer-Delivery: mapped\r\n") {
		t.Errorf("subscription did not request mapped delivery: %q", head)
		return false
	}

	return true
}

func isProtocol(err error) bool {
	var e *sdkError
	return errors.As(err, &e) && e.kind == wire.ErrorProtocol
}

func consume(t *testing.T, o *Object, mode string) (string, error) {
	t.Helper()

	var b bytes.Buffer

	var err error
	if mode == "read" {
		_, err = io.Copy(writerOnly{&b}, readerOnly{o})
	} else {
		_, err = o.WriteTo(&b)
	}

	return b.String(), err
}

func TestMappedSinglePage(t *testing.T) {
	memory := []byte("xxxxxxxxxxhello, mapped worldyyyy")
	payload := "hello, mapped world"

	for _, mode := range []string{"read", "write"} {
		t.Run(mode, func(t *testing.T) {
			fd := deliveryMemory(t, memory)
			c := rawServer(t, func(conn net.Conn, r *bufio.Reader, head []byte) {
				if !requireMapped(t, head) {
					return
				}

				size := uint64(len(payload))
				if sendHead(conn, mappedHead(size, 0, size), fd) != nil {
					return
				}

				if writeMappedFrame(conn, 0, 0, uint32(size), 10) != nil {
					return
				}

				if err := readCredit(r, 0, uint32(size)); err != nil {
					t.Error(err)
					return
				}

				_ = writeFrame(conn, wire.CompleteFrame, 1, size, 0)
			})

			o, err := c.Get(t.Context(), Request{})
			if err != nil {
				t.Fatal(err)
			}
			defer closeQuietly(o)

			got, err := consume(t, o, mode)
			if err != nil || got != payload {
				t.Fatalf("received %q, %v; want %q", got, err, payload)
			}
		})
	}
}

// TestMappedMultiPage reads a full object of two pages, each from its own
// region of the delivery memory.
func TestMappedMultiPage(t *testing.T) {
	const size = PageSize + 10

	memory := make([]byte, 2*PageSize)
	for i := range memory {
		memory[i] = byte(i * 7)
	}

	want := append(append([]byte(nil), memory[:PageSize]...), memory[PageSize:PageSize+10]...)

	for _, mode := range []string{"read", "write"} {
		t.Run(mode, func(t *testing.T) {
			fd := deliveryMemory(t, memory)
			c := rawServer(t, func(conn net.Conn, r *bufio.Reader, head []byte) {
				if !requireMapped(t, head) || sendHead(conn, mappedHead(size, 0, size), fd) != nil {
					return
				}

				if writeMappedFrame(conn, 0, 0, PageSize, 0) != nil || writeMappedFrame(conn, 1, PageSize, 10, PageSize) != nil {
					return
				}

				for number, length := range []uint32{PageSize, 10} {
					if err := readCredit(r, uint64(number), length); err != nil {
						t.Error(err)
						return
					}
				}

				_ = writeFrame(conn, wire.CompleteFrame, 2, size, 0)
			})

			o, err := c.Get(t.Context(), Request{})
			if err != nil {
				t.Fatal(err)
			}
			defer closeQuietly(o)

			got, err := consume(t, o, mode)
			if err != nil || got != string(want) {
				t.Fatalf("received %d bytes, %v; want %d matching bytes", len(got), err, len(want))
			}
		})
	}
}

// TestMappedRangeAndCredits reads a range starting mid-page and checks that
// a page's credit is sent only after all of its bytes were consumed.
func TestMappedRangeAndCredits(t *testing.T) {
	const (
		first = PageSize - 3
		end   = PageSize + 3
	)

	fd := deliveryMemory(t, []byte("..abc..def"))
	partial := make(chan struct{})
	checked := make(chan struct{})

	c := rawServer(t, func(conn net.Conn, r *bufio.Reader, head []byte) {
		defer close(checked)

		if !requireMapped(t, head) || !strings.Contains(string(head), fmt.Sprintf("Range: bytes=%d-%d\r\n", first, end-1)) {
			t.Errorf("unexpected subscription %q", head)
			return
		}

		if sendHead(conn, mappedHead(2*PageSize, first, end), fd) != nil {
			return
		}

		if writeMappedFrame(conn, 0, first, 3, 2) != nil || writeMappedFrame(conn, 1, PageSize, 3, 7) != nil {
			return
		}

		<-partial

		// Two of page 0's three bytes are consumed; no credit may arrive.
		_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))

		if _, err := r.Peek(1); !errors.Is(err, unix.ETIMEDOUT) && !isTimeout(err) {
			t.Errorf("credit before page consumed: %v", err)
			return
		}

		_ = conn.SetReadDeadline(time.Time{})

		checked <- struct{}{}

		for number, length := range []uint32{3, 3} {
			if err := readCredit(r, uint64(number), length); err != nil {
				t.Error(err)
				return
			}
		}

		_ = writeFrame(conn, wire.CompleteFrame, 2, end-first, 0)
	})

	o, err := c.Get(t.Context(), Request{}, ReadOptions{Offset: first, Length: end - first})
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(o)

	b := make([]byte, 2)
	if n, err := io.ReadFull(o, b); n != 2 || err != nil || string(b) != "ab" {
		t.Fatalf("Read = %d %q, %v; want ab", n, b, err)
	}

	close(partial)

	if _, ok := <-checked; !ok {
		t.Fatal("server failed")
	}

	rest, err := io.ReadAll(o)
	if err != nil || string(rest) != "cdef" {
		t.Fatalf("ReadAll = %q, %v; want cdef", rest, err)
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// serveMapped answers each subscription for a 3-byte object from the delivery
// memory returned by fd, with payload at memory offset 0.
func serveMapped(t *testing.T, fd func() int) *Client {
	t.Helper()

	c := rawServer(t, func(conn net.Conn, r *bufio.Reader, _ []byte) {
		if sendHead(conn, mappedHead(3, 0, 3), fd()) != nil || writeMappedFrame(conn, 0, 0, 3, 0) != nil {
			return
		}

		if readCredit(r, 0, 3) == nil {
			_ = writeFrame(conn, wire.CompleteFrame, 1, 3, 0)
		}
	})
	c.bulk = newLane(4, bulkQueue)

	return c
}

func readObject(t *testing.T, o *Object, want string) {
	t.Helper()

	got, err := io.ReadAll(o)
	if err != nil || string(got) != want {
		t.Fatalf("ReadAll = %q, %v; want %q", got, err, want)
	}
}

func TestMappedReuse(t *testing.T) {
	fd := deliveryMemory(t, []byte("abc"))
	c := serveMapped(t, func() int { return fd })

	for range 2 {
		o, err := c.Get(t.Context(), Request{})
		if err != nil {
			t.Fatal(err)
		}

		readObject(t, o, "abc")
		closeQuietly(o)
	}

	if maps, unmaps := c.maps.counts(); maps != 1 || unmaps != 0 {
		t.Fatalf("maps, unmaps = %d, %d; want 1, 0", maps, unmaps)
	}

	closeQuietly(c)

	if maps, unmaps := c.maps.counts(); maps != 1 || unmaps != 1 {
		t.Fatalf("after Close maps, unmaps = %d, %d; want 1, 1", maps, unmaps)
	}
}

func TestMappedReplacement(t *testing.T) {
	fds := []int{deliveryMemory(t, []byte("abc")), deliveryMemory(t, []byte("xyz"))}

	var served atomic.Int32

	c := serveMapped(t, func() int { return fds[served.Add(1)-1] })

	a, err := c.Get(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(a)

	b, err := c.Get(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(b)

	if maps, unmaps := c.maps.counts(); maps != 2 || unmaps != 0 {
		t.Fatalf("maps, unmaps = %d, %d; want 2, 0", maps, unmaps)
	}

	// The replaced mapping stays valid while a still references it.
	readObject(t, a, "abc")
	closeQuietly(a)

	if maps, unmaps := c.maps.counts(); maps != 2 || unmaps != 1 {
		t.Fatalf("after releasing old maps, unmaps = %d, %d; want 2, 1", maps, unmaps)
	}

	readObject(t, b, "xyz")
	closeQuietly(b)

	// The current mapping is cached until the client closes.
	if _, unmaps := c.maps.counts(); unmaps != 1 {
		t.Fatalf("current mapping unmapped early: %d", unmaps)
	}

	closeQuietly(c)

	if maps, unmaps := c.maps.counts(); maps != 2 || unmaps != 2 {
		t.Fatalf("after Close maps, unmaps = %d, %d; want 2, 2", maps, unmaps)
	}
}

// TestMappedCloseWhileReferenced keeps the mapping alive past Client.Close
// until its object ends.
func TestMappedCloseWhileReferenced(t *testing.T) {
	fd := deliveryMemory(t, []byte("abc"))
	c := serveMapped(t, func() int { return fd })

	o, err := c.Get(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}

	closeQuietly(c)

	if _, unmaps := c.maps.counts(); unmaps != 0 {
		t.Fatal("mapping unmapped while referenced")
	}

	closeQuietly(o)

	if maps, unmaps := c.maps.counts(); maps != 1 || unmaps != 1 {
		t.Fatalf("maps, unmaps = %d, %d; want 1, 1", maps, unmaps)
	}
}

func TestMappedProtocolErrors(t *testing.T) {
	type server func(t *testing.T, conn net.Conn, fd int)

	page := func(memory uint64) server {
		return func(_ *testing.T, conn net.Conn, fd int) {
			if sendHead(conn, mappedHead(3, 0, 3), fd) == nil {
				_ = writeMappedFrame(conn, 0, 0, 3, memory)
			}
		}
	}

	for _, tc := range []struct {
		name   string
		serve  server
		atRead bool // the error surfaces from Read rather than Get
	}{
		{"memory offset out of bounds", page(2), true},
		{"memory offset overflow", page(^uint64(0) - 1), true},
		{"memory offset past end", page(1 << 20), true},
		{"no descriptor", func(_ *testing.T, conn net.Conn, _ int) {
			if sendHead(conn, mappedHead(3, 0, 3)) == nil {
				_ = writeMappedFrame(conn, 0, 0, 3, 0)
			}
		}, true},
		{"two descriptors", func(_ *testing.T, conn net.Conn, fd int) {
			if sendHead(conn, mappedHead(3, 0, 3), fd, fd) == nil {
				_ = writeMappedFrame(conn, 0, 0, 3, 0)
			}
		}, false},
		{"descriptor after mapping", func(_ *testing.T, conn net.Conn, fd int) {
			if sendHead(conn, mappedHead(3, 0, 3), fd) == nil {
				frame := wire.Frame{Kind: wire.MappedFrame, Length: 3}.EncodeMapped()
				_, _, _ = conn.(*net.UnixConn).WriteMsgUnix(frame[:], unix.UnixRights(fd), nil) //nolint:forcetypeassert // Unix socket.
			}
		}, true},
		{"wrong content length", func(_ *testing.T, conn net.Conn, fd int) {
			_ = sendHead(conn, mappedHeadLength(3, 0, 3, 3+2*wire.FrameSize), fd)
		}, false},
		{"inline frame", func(_ *testing.T, conn net.Conn, fd int) {
			if sendHead(conn, mappedHead(3, 0, 3), fd) == nil && writeFrame(conn, wire.PageFrame, 0, 0, 3) == nil {
				_, _ = io.WriteString(conn, "abc")
			}
		}, true},
		{"bad delivery value", func(_ *testing.T, conn net.Conn, fd int) {
			head := strings.Replace(mappedHead(3, 0, 3), "Racer-Delivery: mapped", "Racer-Delivery: inline", 1)
			_ = sendHead(conn, head, fd)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fd := deliveryMemory(t, []byte("abc"))
			c := rawServer(t, func(conn net.Conn, r *bufio.Reader, _ []byte) {
				tc.serve(t, conn, fd)
				// Hold the connection until the client gives up.
				_, _ = io.Copy(io.Discard, r)
			})

			o, err := c.Get(t.Context(), Request{})
			if tc.atRead {
				if err != nil {
					t.Fatalf("Get: %v", err)
				}

				_, err = io.ReadAll(o)
				closeQuietly(o)
			} else if err == nil {
				closeQuietly(o)
				t.Fatal("Get succeeded")
			}

			if !isProtocol(err) {
				t.Fatalf("error %v; want protocol error", err)
			}
		})
	}
}

// TestMappedNotRequested rejects a mapped response to an inline request.
func TestMappedNotRequested(t *testing.T) {
	fd := deliveryMemory(t, []byte("abc"))
	c := rawServer(t, func(conn net.Conn, r *bufio.Reader, head []byte) {
		if strings.Contains(string(head), "Racer-Delivery") {
			t.Errorf("inline client requested mapped delivery: %q", head)
		}

		_ = sendHead(conn, mappedHead(3, 0, 3), fd)
		_, _ = io.Copy(io.Discard, r)
	})
	c.inline = true

	if o, err := c.Get(t.Context(), Request{}); !isProtocol(err) {
		closeQuietly(o)
		t.Fatalf("Get error %v; want protocol error", err)
	}
}

// TestInlineDelivery covers an inline client, and a server that ignores the
// mapped request and answers inline, possibly with a stray descriptor.
func TestInlineDelivery(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inline bool
		stray  bool
	}{
		{"client inline", true, false},
		{"server declines", false, false},
		{"server declines with descriptor", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fd := deliveryMemory(t, []byte("zzz"))
			c := rawServer(t, func(conn net.Conn, r *bufio.Reader, head []byte) {
				if got := strings.Contains(string(head), "Racer-Delivery: mapped\r\n"); got == tc.inline {
					t.Errorf("mapped requested = %v, want %v: %q", got, !tc.inline, head)
					return
				}

				var fds []int
				if tc.stray {
					fds = []int{fd}
				}

				if sendHead(conn, subscriptionHead(3, 0, 3), fds...) != nil {
					return
				}

				if writeFrame(conn, wire.PageFrame, 0, 0, 3) != nil {
					return
				}

				if _, err := io.WriteString(conn, "abc"); err != nil {
					return
				}

				if readCredit(r, 0, 3) == nil {
					_ = writeFrame(conn, wire.CompleteFrame, 1, 3, 0)
				}
			})
			c.inline = tc.inline

			for _, mode := range []string{"read", "write"} {
				o, err := c.Get(t.Context(), Request{})
				if err != nil {
					t.Fatal(err)
				}

				got, err := consume(t, o, mode)
				closeQuietly(o)

				if err != nil || got != "abc" {
					t.Fatalf("%s: received %q, %v; want abc", mode, got, err)
				}
			}

			if maps, _ := c.maps.counts(); maps != 0 {
				t.Fatalf("inline response mapped memory %d times", maps)
			}
		})
	}
}

// TestMappedEmptyRange reads a range with no pages: only completion follows.
func TestMappedEmptyRange(t *testing.T) {
	fd := deliveryMemory(t, []byte("abc"))
	c := rawServer(t, func(conn net.Conn, _ *bufio.Reader, _ []byte) {
		if sendHead(conn, mappedHead(0, 0, 0), fd) == nil {
			_ = writeFrame(conn, wire.CompleteFrame, 0, 0, 0)
		}
	})

	o, err := c.Get(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(o)

	readObject(t, o, "")
}
