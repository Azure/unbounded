// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type dispatchListener struct {
	net.Listener
	kernelBytes atomic.Int64
}

func (l *dispatchListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}

	return &dispatchConn{TCPConn: c.(*net.TCPConn), listener: l}, nil
}

type (
	dispatchConn struct {
		*net.TCPConn
		listener *dispatchListener
	}
	dispatchReader struct {
		io.Reader
		syscall.Conn
		read int64
	}
)

func (r *dispatchReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.read += int64(n)

	return n, err
}

func (c *dispatchConn) ReadFrom(src io.Reader) (int64, error) {
	lr, ok := src.(*io.LimitedReader)
	if !ok {
		return c.TCPConn.ReadFrom(src)
	}

	file, ok := lr.R.(syscall.Conn)
	if !ok {
		return c.TCPConn.ReadFrom(src)
	}

	probe := &dispatchReader{Reader: lr.R, Conn: file}
	lr.R = probe
	n, err := c.TCPConn.ReadFrom(lr)
	c.listener.kernelBytes.Add(n - probe.read)

	return n, err
}

func TestPinnedOriginActualSendfileDispatch(t *testing.T) {
	data := payload(2 << 20)
	store := pinnedFixture(t, data)
	origin, _ := NewOrigin(store)
	server := httptest.NewUnstartedServer(origin)
	listener := &dispatchListener{Listener: server.Listener}
	server.Listener = listener

	server.Start()
	defer server.Close()

	r, _ := http.NewRequestWithContext(t.Context(), "GET", server.URL+"/file", nil)
	r.Header.Set("Range", "bytes=17-1048592")

	resp, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}

	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	server.Close()

	if err != nil || resp.StatusCode != 206 || !bytes.Equal(body, data[17:1048593]) || len(resp.TransferEncoding) != 0 {
		t.Fatal("framing/content", len(body), err)
	}

	if listener.kernelBytes.Load() < 1<<19 {
		t.Fatal("no kernel transfer through Go dispatch", listener.kernelBytes.Load())
	}

	if store.source.closes != 1 {
		t.Fatal("source leaked")
	}
}

func TestPinnedOriginCancelsBlockedNetworkWrite(t *testing.T) {
	store := pinnedFixture(t, nil)
	store.meta.Size = 64 << 20

	store.source.length = store.meta.Size
	if err := store.source.Truncate(6 + store.meta.Size); err != nil {
		t.Fatal(err)
	}

	origin, _ := NewOrigin(store)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)

		origin.ServeHTTP(w, r.WithContext(ctx))
	}))
	defer server.Close()

	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	_ = conn.(*net.TCPConn).SetReadBuffer(4096)
	if _, err := io.WriteString(conn, "GET /file HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	// Read only the first byte to prove the handler started sending, then leave
	// the rest backpressured in a small receive window.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	var first [1]byte
	if _, err := io.ReadFull(conn, first[:]); err != nil {
		t.Fatal(err)
	}

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("blocked origin write ignored cancellation")
	}

	if store.source.closes != 1 {
		t.Fatal("source not closed after cancellation")
	}
}
