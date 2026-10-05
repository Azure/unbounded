// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestValueWriteToSplicesOrderedHTTPBodies(t *testing.T) {
	const size = 2*int64(PageSize) + 173

	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first, last := fixtureRange(t, r, size)

		streamResponseHead(w, int64(first), int64(last-first)+1, size, `"v"`)
		_, _ = io.CopyN(w, &offsetStream{offset: int64(first)}, int64(last-first)+1)
	}))
	c := testClient(t, path, 3)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	v, err := c.Get(ctx, Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(v)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(listener)

	peer, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(peer)

	connection, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(connection)

	if err := peer.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() {
		_, err := io.CopyN(&offsetSink{}, peer, size)
		done <- err
	}()

	sink := &tcpTransferWriter{TCPConn: connection.(*net.TCPConn)}

	n, err := v.WriteTo(sink)
	if err != nil || n != size {
		t.Fatal(n, err)
	}

	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if sink.readFrom != 0 {
		t.Fatal("buffered subscription bypassed page validation", sink.readFrom)
	}

	if len(c.slots) != 0 {
		t.Fatal("body permits leaked")
	}
	// A later response still goes through Transport parsing and body lifecycle.
	other, err := c.Get(ctx, Request{})
	if err != nil {
		t.Fatal(err)
	}

	closeBody(other)

	streaming, err := c.GetStreaming(ctx, Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(streaming)

	go func() {
		_, err := io.CopyN(&offsetSink{}, peer, size)
		done <- err
	}()

	if n, err := streaming.WriteToHTTP(sink); err != nil || n != size {
		t.Fatal(n, err)
	}

	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if sink.readFrom == 0 || c.Stats().ActiveBulk != 0 {
		t.Fatal("streaming transfer did not dispatch ReaderFrom or release admission")
	}
}

func TestValueWriteToSpliceCancellation(t *testing.T) {
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamResponse(w, 0, int64(PageSize), int64(PageSize), `"v"`)
	}))
	c := testClient(t, path, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	v, err := c.GetStreaming(ctx, Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(v)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(listener)

	peer, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(peer)

	connection, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(connection)

	if err := connection.(*net.TCPConn).SetWriteBuffer(4096); err != nil {
		t.Fatal(err)
	}

	if err := peer.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}

	sink := &tcpTransferWriter{TCPConn: connection.(*net.TCPConn)}

	done := make(chan error, 1)

	go func() { _, err := v.WriteToHTTP(sink); done <- err }()
	// Consume a prefix, then leave the destination blocked and cancel the Value.
	if _, err := io.CopyN(io.Discard, peer, 64*1024); err != nil {
		t.Fatal(err)
	}

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("splice did not cancel")
	}

	if len(c.slots) != 0 {
		t.Fatal("canceled splice retained page")
	}
}

// tcpTransferWriter exposes the real TCP ReaderFrom and deadline implementations
// without HTTP buffering, so transfers exercise Unix-to-TCP dispatch directly.
type tcpTransferWriter struct {
	*net.TCPConn
	readFrom int
}

func (w *tcpTransferWriter) Header() http.Header { return make(http.Header) }
func (w *tcpTransferWriter) WriteHeader(int)     {}
func (w *tcpTransferWriter) ReadFrom(r io.Reader) (int64, error) {
	w.readFrom++
	return w.TCPConn.ReadFrom(r)
}
