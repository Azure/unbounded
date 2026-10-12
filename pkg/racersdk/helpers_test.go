// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk/internal/fakeracer"
	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

const (
	maxHeadBytes = wire.MaxHeadBytes
	objectPrefix = wire.ObjectPrefix
)

// socketDir keeps socket paths inside this worktree and short enough for
// sun_path, including under -race.
func socketDir(t testing.TB) string {
	t.Helper()

	if err := os.MkdirAll("../../tmp", 0o700); err != nil {
		t.Fatal(err)
	}

	dir, err := os.MkdirTemp("../../tmp", "sdk-")
	if err != nil {
		t.Fatal(err)
	}

	path, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := os.RemoveAll(path); err != nil {
			t.Error(err)
		}
	})

	return path
}

func testClient(t testing.TB, path string, maxConnections int) *Client {
	t.Helper()

	c, err := newClient(ClientConfig{Cache: "test", MaxConnections: maxConnections}, path)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})

	return c
}

// rawClientPeer serves handler on a client socket and returns its path.
func rawClientPeer(t testing.TB, handler http.Handler) string {
	t.Helper()
	path := filepath.Join(socketDir(t), "socket")

	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}

	s := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	done := make(chan struct{})

	go func() { defer close(done); _ = s.Serve(l) }()

	t.Cleanup(func() { _ = s.Close(); <-done })

	return path
}

func unixTransport(path string) *http.Transport {
	return &http.Transport{
		DisableCompression: true, MaxConnsPerHost: 16, MaxIdleConnsPerHost: 16,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}
}

// originClient connects a real Client to the origin socket at path through
// the fake Racer. It establishes SDK integration, not Racer compatibility.
func originClient(t testing.TB, path string, maxConnections int) *Client {
	t.Helper()

	transport := unixTransport(path)
	t.Cleanup(transport.CloseIdleConnections)

	return testClient(t, rawClientPeer(t, fakeracer.NewHandler(transport)), maxConnections)
}

// fakeClient serves origin and returns a Client that reads through it.
func fakeClient(t *testing.T, origin Origin) *Client {
	t.Helper()

	path, cancel, done := startOrigin(t, nil, origin)
	c := originClient(t, path, 0)

	t.Cleanup(func() { closeQuietly(c); cancel(); <-done })

	return c
}

// rawServer accepts client connections on a socket and hands each one, with
// its parsed request head, to serve. The connection closes when serve returns.
func rawServer(t testing.TB, serve func(conn net.Conn, r *bufio.Reader, head []byte)) *Client {
	t.Helper()
	path := filepath.Join(socketDir(t), "socket")

	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}

			go func() {
				defer closeQuietly(conn)

				r := bufio.NewReader(conn)
				if head, err := wire.ReadRawHead(r, false); err == nil {
					serve(conn, r, head)
				}
			}()
		}
	}()

	t.Cleanup(func() { closeQuietly(l); <-done })

	c := testClient(t, path, 1)
	c.limits.bodyTimeout = 2 * time.Second

	return c
}

// subscriptionHead is a valid subscription response for [first, end) of an
// object with the given size and ETag "v".
func subscriptionHead(size, first, end uint64) string {
	pages := wire.PageCount(first, end)

	return fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nContent-Length: %d\r\nETag: \"v\"\r\nRacer-Expires-At: 0\r\nRacer-Object-Length: %d\r\nRacer-Range-Start: %d\r\nRacer-Range-End: %d\r\nConnection: close\r\n\r\n", end-first+wire.FrameSize*(pages+1), size, first, end)
}

func writeFrame(w io.Writer, kind byte, page, offset uint64, length uint32) error {
	return wire.WriteFrame(w, wire.Frame{Kind: kind, Number: page, Offset: offset, Length: length})
}

// readCredit reads one credit frame and reports whether it matches.
func readCredit(r io.Reader, number uint64, length uint32) error {
	var frame [wire.CreditSize]byte
	if _, err := io.ReadFull(r, frame[:]); err != nil {
		return err
	}

	if frame != (wire.Credit{Number: number, Length: length}).Encode() {
		return fmt.Errorf("credit %x, want page %d length %d", frame, number, length)
	}

	return nil
}

type repeatedByte byte

func (b repeatedByte) Read(p []byte) (int, error) {
	if len(p) > 0 {
		p[0] = byte(b)
		for filled := 1; filled < len(p); {
			filled += copy(p[filled:], p[:filled])
		}
	}

	return len(p), nil
}

// offsetStream makes wrong page offsets observable without allocating an object.
type offsetStream struct{ offset int64 }

func (r *offsetStream) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte((r.offset + int64(i)) % 251)
	}

	r.offset += int64(len(p))

	return len(p), nil
}

type offsetSink struct{ offset int64 }

func (w *offsetSink) Write(p []byte) (int, error) {
	for i, b := range p {
		if b != byte((w.offset+int64(i))%251) {
			return i, fmt.Errorf("wrong byte at offset %d", w.offset+int64(i))
		}
	}

	w.offset += int64(len(p))

	return len(p), nil
}

// offsetOrigin serves an object of the given size whose bytes encode their
// offset.
func offsetOrigin(size int64) Origin {
	return func(_ context.Context, r OriginRequest) (Metadata, io.ReadCloser, error) {
		m := originMeta(size)
		if r.Head {
			return m, nil, nil
		}

		start := min(r.Offset, size)

		length := min(r.Length, size-start)
		if length == 0 {
			return m, nil, nil
		}

		return m, io.NopCloser(io.LimitReader(&offsetStream{offset: start}, length)), nil
	}
}

// Raw fixtures deliberately bypass validation.
func rawRequest(method, fields string) []byte {
	return []byte(method + " " + objectPrefix + (Key{}).String() + " HTTP/1.1\r\nHost: racer\r\n" + fields + "\r\n")
}

func rawResponse(status int, fields string) []byte {
	return []byte("HTTP/1.1 " + strconv.Itoa(status) + " " + http.StatusText(status) + "\r\n" + fields + "\r\n")
}

type finalErrorReader struct{ err error }

func (r finalErrorReader) Read(p []byte) (int, error) { return copy(p, "abc"), r.err }

func assertIs(t testing.TB, err, target error) {
	t.Helper()

	if !errors.Is(err, target) {
		t.Fatalf("error = %v; want %v", err, target)
	}
}
