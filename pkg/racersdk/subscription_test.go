// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk/internal/fakeracer"
	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

// subscriptionHandler migrates HTTP payload fixtures to the duplex wire. The
// fixture still chooses metadata and payload/failure timing. No request is
// rewritten or retried, and credits gate every page frame on this one socket.
func subscriptionHandler(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			handler.ServeHTTP(w, r)
			return
		}

		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer closeBody(conn)

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		credits := fakeracer.NewCredits()
		go credits.Releases(rw.Reader, cancel)

		s, err := wire.ParseSubscriptionRequest(r)
		if err != nil {
			return
		}

		writer := &subscriptionFixture{header: make(http.Header), writer: rw, conn: conn, ctx: ctx, credits: credits, request: s}
		handler.ServeHTTP(writer, r.WithContext(ctx))
		writer.finish()
	})
}

type subscriptionFixture struct {
	header                              http.Header
	writer                              *bufio.ReadWriter
	conn                                net.Conn
	ctx                                 context.Context
	credits                             *fakeracer.Credits
	request                             wire.SubscriptionRequest
	started                             bool
	err                                 error
	first, end, offset, frameEnd, count uint64
}

func (w *subscriptionFixture) Header() http.Header { return w.header }
func (w *subscriptionFixture) SetWriteDeadline(deadline time.Time) error {
	return w.conn.SetWriteDeadline(deadline)
}

func fixtureRange(t *testing.T, r *http.Request, size int64) (ByteOffset, ByteOffset) {
	t.Helper()

	s, err := wire.ParseSubscriptionRequest(r)
	if err != nil {
		t.Error(err)
		return 0, 0
	}

	return ByteOffset(s.First), ByteOffset(min(s.End, uint64(size)) - 1)
}

func (w *subscriptionFixture) WriteHeader(status int) {
	if w.started {
		return
	}

	w.started = true
	if status == 200 || status == 206 {
		size, err := decimal(w.header.Get("Content-Length"))
		if cr := w.header.Get("Content-Range"); cr != "" {
			parts := strings.Split(cr, "/")
			if len(parts) == 2 {
				size, err = decimal(parts[1])
			}
		}

		if err != nil {
			w.err = err
			return
		}

		w.first, w.end = w.request.First, min(size, w.request.End)
		w.offset = w.first

		pages := uint64(0)
		if w.end > w.first {
			pages = (w.end-1)/uint64(PageSize) - w.first/uint64(PageSize) + 1
		}

		w.header.Del("Content-Range")
		w.header.Set("Content-Type", "application/octet-stream")
		w.header.Set("Racer-Object-Length", strconv.FormatUint(size, 10))
		w.header.Set("Racer-Range-Start", strconv.FormatUint(w.first, 10))
		w.header.Set("Racer-Range-End", strconv.FormatUint(w.end, 10))
		w.header.Set("Content-Length", strconv.FormatUint(w.end-w.first+21*(pages+1), 10))

		status = 200
	}

	w.err = wire.WriteSubscriptionHead(w.writer, status, w.header)
	if w.err == nil {
		w.err = w.writer.Flush()
	}
}

func (w *subscriptionFixture) Write(p []byte) (int, error) {
	if !w.started {
		w.WriteHeader(200)
	}

	n := 0

	for len(p) > 0 && w.err == nil {
		if w.offset == w.end {
			return n, io.ErrShortWrite
		}

		if w.offset == w.frameEnd || w.frameEnd == 0 {
			w.frameEnd = min(w.end, (w.offset/uint64(PageSize)+1)*uint64(PageSize))
			length := uint32(w.frameEnd - w.offset)

			w.err = w.credits.Reserve(w.ctx, w.request, w.offset/uint64(PageSize), length)
			if w.err == nil {
				w.err = fakeSubscriptionFrame(w.writer, 1, w.offset/uint64(PageSize), w.offset, length)
			}

			w.count++
		}

		if w.err != nil {
			break
		}

		part := p[:min(uint64(len(p)), w.frameEnd-w.offset)]

		var written int

		written, w.err = w.writer.Write(part)
		w.offset += uint64(written)
		n += written
		p = p[written:]

		if w.err == nil {
			w.err = w.writer.Flush()
		}
	}

	return n, w.err
}

func (w *subscriptionFixture) Flush() {
	if !w.started {
		w.WriteHeader(200)
	}

	if w.err == nil {
		w.err = w.writer.Flush()
	}
}

func (w *subscriptionFixture) finish() {
	w.Flush()

	if w.err == nil && w.offset == w.end {
		w.err = fakeSubscriptionFrame(w.writer, 2, w.count, w.end-w.first, 0)
		if w.err == nil {
			w.err = w.writer.Flush()
		}
	}
}

func subscriptionHead(size, first, end uint64) string {
	pages := uint64(0)
	if end > first {
		pages = (end-1)/uint64(PageSize) - first/uint64(PageSize) + 1
	}

	return fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nContent-Length: %d\r\nETag: \"v\"\r\nRacer-Expires-At: 0\r\nRacer-Object-Length: %d\r\nRacer-Range-Start: %d\r\nRacer-Range-End: %d\r\nConnection: close\r\n\r\n", end-first+21*(pages+1), size, first, end)
}

// fakeSubscriptionFrame encodes intentionally valid or malformed test frames
// with the shared codec; sequence validation belongs to the client under test.
func fakeSubscriptionFrame(w io.Writer, kind byte, page, offset uint64, length uint32) error {
	return wire.WriteFrame(w, wire.Frame{Kind: kind, Number: page, Offset: offset, Length: length})
}

func rawSubscriptionClient(t *testing.T, serve func(net.Conn, *bufio.Reader, []byte)) *Client {
	t.Helper()
	c := testClient(t, "unused", 1)
	c.config.BodyReadTimeout = time.Second
	poolConfig := c.bulk.Config()
	poolConfig.Dial = func(context.Context, string, string) (net.Conn, error) {
		client, peer := net.Pipe()

		t.Cleanup(func() { closeBody(peer) })

		go func() {
			defer closeBody(peer)

			r := bufio.NewReader(peer)

			head, err := readRawHead(r, false)
			if err == nil {
				serve(peer, r, head)
			}
		}()

		return client, nil
	}
	c.configurePools(poolConfig)

	return c
}

func TestPageStreamUnorderedReleaseAndCompletion(t *testing.T) {
	const (
		first = uint64(PageSize) - 2
		end   = uint64(PageSize) + 3
	)

	var calls atomic.Int32

	c := rawSubscriptionClient(t, func(conn net.Conn, reader *bufio.Reader, head []byte) {
		calls.Add(1)

		if !strings.HasPrefix(string(head), "POST /v2/objects/") || headHeaders(head).Get("Racer-Ordered") != "0" {
			t.Error("not unordered v2")
		}

		_, _ = io.WriteString(conn, subscriptionHead(end, first, end))
		_ = fakeSubscriptionFrame(conn, 1, 1, uint64(PageSize), 3)
		_, _ = conn.Write([]byte("xyz"))

		var release [12]byte
		if _, err := io.ReadFull(reader, release[:]); err != nil {
			return
		}

		if binary.BigEndian.Uint64(release[:8]) != 1 || binary.BigEndian.Uint32(release[8:]) != 3 {
			t.Error("incorrect release")
		}

		_ = fakeSubscriptionFrame(conn, 1, 0, first, 2)
		_, _ = conn.Write([]byte("ab"))
		_ = fakeSubscriptionFrame(conn, 2, 2, 5, 0)
	})

	s, err := c.OpenPages(t.Context(), Request{}, ReadOptions{Offset: ByteOffset(first), Length: 5, PageCredits: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(s)

	p, err := s.Next()
	if err != nil || p.Number != 1 || p.Offset != ByteOffset(PageSize) || string(p.Data) != "xyz" {
		t.Fatal(p, err)
	}

	result := make(chan *PageLease, 1)
	errors := make(chan error, 1)

	go func() { next, err := s.Next(); result <- next; errors <- err }()

	select {
	case <-result:
		t.Fatal("Next bypassed held credit")
	case <-time.After(20 * time.Millisecond):
	}

	if err := p.Release(); err != nil {
		t.Fatal(err)
	}

	if err := p.Release(); err != nil || p.Data != nil {
		t.Fatal("release not idempotent", err)
	}

	next := <-result
	if err := <-errors; err != nil || next.Number != 0 || next.Offset != ByteOffset(first) || string(next.Data) != "ab" {
		t.Fatal(next, err)
	}

	if err := next.Release(); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Next(); err != io.EOF {
		t.Fatal(err)
	}

	if calls.Load() != 1 || c.Stats().Dials != 1 || c.Stats().ActiveBulk != 0 {
		t.Fatal(c.Stats())
	}
}

func TestPageStreamMalformedFrames(t *testing.T) {
	for _, mode := range []string{"kind", "number", "offset", "length", "short", "missing complete", "complete count", "complete bytes", "complete length"} {
		t.Run(mode, func(t *testing.T) {
			c := rawSubscriptionClient(t, func(conn net.Conn, _ *bufio.Reader, _ []byte) {
				_, _ = io.WriteString(conn, subscriptionHead(3, 0, 3))
				kind, number, offset, length := byte(1), uint64(0), uint64(0), uint32(3)

				switch mode {
				case "kind":
					kind = 7
				case "number":
					number = 1
				case "offset":
					offset = 1
				case "length":
					length = 4
				}

				_ = fakeSubscriptionFrame(conn, kind, number, offset, length)
				if mode == "short" {
					_, _ = conn.Write([]byte("ab"))
					return
				}

				_, _ = conn.Write([]byte("abc"))

				if mode == "missing complete" {
					return
				}

				number, offset, length = 1, 3, 0

				switch mode {
				case "complete count":
					number++
				case "complete bytes":
					offset++
				case "complete length":
					length++
				}

				_ = fakeSubscriptionFrame(conn, 2, number, offset, length)
			})

			s, err := c.OpenPages(t.Context(), Request{})
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(s)

			p, err := s.Next()
			if p != nil || err == nil || err == io.EOF {
				t.Fatal("invalid frame exposed", p, err)
			}

			if mode == "short" || mode == "missing complete" {
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatal(err)
				}
			} else {
				assertKind(t, err, ErrorProtocol)
			}

			if _, again := s.Next(); again != err {
				t.Fatal("nonterminal failure", again)
			}

			if c.Stats().ActiveBulk != 0 {
				t.Fatal("admission leaked")
			}
		})
	}
}

func TestPageStreamPartialAndEmptyRanges(t *testing.T) {
	c, cleanup, err := newFakeClient(t, func(_ context.Context, r OriginRequest) (Metadata, io.ReadCloser, error) {
		if r.Key()[0] == 1 {
			return originMeta(0), nil, nil
		}

		if r.Operation() == OperationHead {
			return originMeta(9), nil, nil
		}

		return originMeta(9), io.NopCloser(strings.NewReader("012345678")), nil
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(cleanup)

	for _, offset := range []ByteOffset{3, 9} {
		o := ReadOptions{Offset: offset}
		request := Request{}

		if offset == 3 {
			o.Length = 3
		} else {
			o.Offset = 0
			request.Key[0] = 1
		}

		s, err := c.OpenPages(t.Context(), request, o)
		if err != nil {
			t.Fatal(err)
		}

		p, err := s.Next()
		if offset == 3 {
			if err != nil || p.Offset != 3 || string(p.Data) != "345" {
				t.Fatal(p, err)
			}

			if err := p.Release(); err != nil || p.Data != nil {
				t.Fatal("release retained payload", err)
			}

			p, err = s.Next()
		}

		if p != nil || err != io.EOF {
			t.Fatal("missing Complete", p, err)
		}

		closeBody(s)

		if c.Stats().ActiveBulk != 0 {
			t.Fatal("range retained admission")
		}
	}
}

func TestPageStreamCallerFailureReleasesOwnership(t *testing.T) {
	c := rawSubscriptionClient(t, func(conn net.Conn, reader *bufio.Reader, _ []byte) {
		first, end := uint64(PageSize)-1, uint64(PageSize)+1
		_, _ = io.WriteString(conn, subscriptionHead(end, first, end))
		_ = fakeSubscriptionFrame(conn, 1, 0, first, 1)
		_, _ = io.WriteString(conn, "x")
		_, _ = io.Copy(io.Discard, reader)
	})

	s, err := c.OpenPages(t.Context(), Request{}, ReadOptions{Offset: ByteOffset(PageSize - 1), PageCredits: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(s)

	p, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}

	failed := errors.New("destination failed")

	_, err = (writeFunc(func([]byte) (int, error) { return 0, failed })).Write(p.Data)
	if !errors.Is(err, failed) {
		t.Fatal(err)
	}
	// The caller owns cleanup after its destination fails. Close must not
	// invalidate a held public lease, and Release must work after Close.
	closeBody(s)

	if string(p.Data) != "x" || c.Stats().ActiveBulk != 0 {
		t.Fatal("close invalidated lease or retained admission")
	}

	assertKind(t, p.Release(), ErrorClosed)
	assertKind(t, p.Release(), ErrorClosed)

	if p.Data != nil || s.bytesHeld != 0 || len(s.outstanding) != 0 {
		t.Fatal("release retained ownership")
	}
}

func TestPageStreamByteCreditWaitAndCancellation(t *testing.T) {
	for _, action := range []string{"release", "context", "stream", "client"} {
		t.Run(action, func(t *testing.T) {
			const (
				first = uint64(PageSize) - 1
				end   = 2*uint64(PageSize) + 1
			)

			c := rawSubscriptionClient(t, func(conn net.Conn, reader *bufio.Reader, _ []byte) {
				_, _ = io.WriteString(conn, subscriptionHead(end, first, end))
				_ = fakeSubscriptionFrame(conn, 1, 0, first, 1)
				_, _ = io.WriteString(conn, "x")

				var release [12]byte
				if _, err := io.ReadFull(reader, release[:]); err != nil {
					return
				}

				if action != "release" {
					t.Error("close emitted a release")
				}

				_ = fakeSubscriptionFrame(conn, 1, 1, uint64(PageSize), uint32(PageSize))
				_, _ = io.CopyN(conn, repeatedByte('y'), int64(PageSize))
				_, _ = io.Copy(io.Discard, reader)
			})

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			s, err := c.OpenPages(ctx, Request{}, ReadOptions{Offset: ByteOffset(first), PageCredits: 2, ByteCredits: PageSize})
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(s)

			p, err := s.Next()
			if err != nil {
				t.Fatal(err)
			}

			result := make(chan error, 1)

			go func() {
				next, err := s.Next()
				if next != nil {
					if next.Number != 1 || len(next.Data) != int(PageSize) {
						t.Error("wrong resumed page")
					}

					closeBody(s)

					_ = next.Release()
				}

				result <- err
			}()

			select {
			case err := <-result:
				t.Fatal("Next did not wait for byte credit", err)
			case <-time.After(20 * time.Millisecond):
			}

			switch action {
			case "release":
				if err := p.Release(); err != nil {
					t.Fatal(err)
				}
			case "context":
				cancel()
			case "stream":
				closeBody(s)
			case "client":
				closeBody(c)
			}

			select {
			case err := <-result:
				switch action {
				case "release":
					if err != nil {
						t.Fatal(err)
					}
				case "context":
					if !errors.Is(err, context.Canceled) {
						t.Fatal(err)
					}
				default:
					assertKind(t, err, ErrorClosed)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("credit waiter did not unblock")
			}

			_ = p.Release()

			if c.Stats().ActiveBulk != 0 {
				t.Fatal("retained admission")
			}
		})
	}
}

func TestPageStreamDuplicateAndOrderedValidation(t *testing.T) {
	for _, ordered := range []bool{false, true} {
		t.Run(strconv.FormatBool(ordered), func(t *testing.T) {
			const (
				first = uint64(PageSize) - 1
				end   = uint64(PageSize) + 1
			)

			c := rawSubscriptionClient(t, func(conn net.Conn, reader *bufio.Reader, _ []byte) {
				_, _ = io.WriteString(conn, subscriptionHead(end, first, end))
				_ = fakeSubscriptionFrame(conn, 1, 1, uint64(PageSize), 1)
				_, _ = io.WriteString(conn, "x")

				if ordered {
					return
				}

				var release [12]byte
				if _, err := io.ReadFull(reader, release[:]); err != nil {
					return
				}

				_ = fakeSubscriptionFrame(conn, 1, 1, uint64(PageSize), 1)
			})

			s, err := c.OpenPages(t.Context(), Request{}, ReadOptions{Offset: ByteOffset(first), Ordered: ordered})
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(s)

			p, err := s.Next()
			if !ordered {
				if err != nil {
					t.Fatal(err)
				}

				if err := p.Release(); err != nil {
					t.Fatal(err)
				}

				p, err = s.Next()
			}

			if p != nil {
				t.Fatal("invalid page exposed")
			}

			assertKind(t, err, ErrorProtocol)
		})
	}
}

func TestPageStreamHeadAndOptionsValidation(t *testing.T) {
	base := subscriptionHead(3, 0, 3)
	for _, head := range []string{
		strings.Replace(base, "Content-Length: 45", "Content-Length: 44", 1),
		strings.Replace(base, "Racer-Object-Length: 3", "Racer-Object-Length: 03", 1),
		strings.Replace(base, "Racer-Range-End: 3", "Racer-Range-End: 4", 1),
		strings.Replace(base, "Racer-Range-Start: 0", "Racer-Range-Start: 1", 1),
		strings.Replace(base, "Racer-Expires-At: 0", "Racer-Expires-At: 9223372036854775808", 1),
		strings.Replace(base, "Connection: close", "Connection: keep-alive", 1),
		strings.Replace(base, "ETag: \"v\"", "ETag: W/\"v\"", 1),
		strings.Replace(base, "Racer-Range-End: 3", "Racer-Range-End: 3\r\nracer-range-end: 3", 1),
		strings.Replace(base, "Content-Type: application/octet-stream", "Content-Type: text/plain", 1),
		"HTTP/1.1 416 Range Not Satisfiable\r\nContent-Length: 0\r\n\r\n",
	} {
		c := rawSubscriptionClient(t, func(conn net.Conn, _ *bufio.Reader, _ []byte) { _, _ = io.WriteString(conn, head) })
		_, err := c.OpenPages(t.Context(), Request{})
		assertKind(t, err, ErrorProtocol)

		if c.Stats().ActiveBulk != 0 {
			t.Fatal("bad head retained admission")
		}
	}

	c := testClient(t, "unused", 1)
	for _, o := range []ReadOptions{{PageCredits: -1}, {PageCredits: 65}, {ByteCredits: PageSize - 1}, {ByteCredits: 64*PageSize + 1}} {
		_, err := c.OpenPages(t.Context(), Request{}, o)
		assertKind(t, err, ErrorInvalidArgument)
	}

	if c.Stats().Dials != 0 {
		t.Fatal("invalid credits dialed")
	}
}

func TestPageStreamReleaseAfterTerminalAndIntervalBound(t *testing.T) {
	c := rawSubscriptionClient(t, func(conn net.Conn, _ *bufio.Reader, _ []byte) {
		_, _ = io.WriteString(conn, subscriptionHead(1, 0, 1))
		_ = fakeSubscriptionFrame(conn, 1, 0, 0, 1)
		_, _ = io.WriteString(conn, "x")
		_ = fakeSubscriptionFrame(conn, 2, 1, 1, 0)
	})

	s, err := c.OpenPages(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(s)

	p, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.Next(); err != io.EOF {
		t.Fatal(err)
	}

	if string(p.Data) != "x" {
		t.Fatal("EOF invalidated lease")
	}

	if err := p.Release(); err != nil {
		t.Fatal(err)
	}

	if err := p.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestPageStreamReleaseConcurrentWithFinalRead(t *testing.T) {
	const (
		first = uint64(PageSize) - 1
		end   = uint64(PageSize) + 1
	)

	for range 20 {
		c := rawSubscriptionClient(t, func(conn net.Conn, _ *bufio.Reader, _ []byte) {
			_, _ = io.WriteString(conn, subscriptionHead(end, first, end))
			_ = fakeSubscriptionFrame(conn, 1, 0, first, 1)
			_, _ = io.WriteString(conn, "a")
			_ = fakeSubscriptionFrame(conn, 1, 1, uint64(PageSize), 1)
			_, _ = io.WriteString(conn, "b")
			_ = fakeSubscriptionFrame(conn, 2, 2, 2, 0)
		})

		s, err := c.OpenPages(t.Context(), Request{}, ReadOptions{Offset: ByteOffset(first)})
		if err != nil {
			t.Fatal(err)
		}

		p, err := s.Next()
		if err != nil {
			t.Fatal(err)
		}

		released := make(chan error, 1)

		go func() { released <- p.Release() }()

		next, err := s.Next()
		if err != nil || string(next.Data) != "b" {
			t.Fatal(next, err)
		}

		if _, err := s.Next(); err != io.EOF {
			t.Fatal(err)
		}

		if err := <-released; err != nil {
			t.Fatal("final close raced release", err)
		}

		if err := next.Release(); err != nil {
			t.Fatal(err)
		}

		closeBody(s)
	}
}
