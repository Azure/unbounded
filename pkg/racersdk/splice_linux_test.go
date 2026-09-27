// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package racersdk

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestValueWriteToSplicesOrderedHTTPBodies(t *testing.T) {
	const size = 2*int64(PageSize) + 173

	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeValue, err := parseRange(r.Header.Get("Range"))
		if err != nil {
			t.Error(err)
			return
		}

		first, last, err := rangeValue.Resolve(ByteLength(size))
		if err != nil {
			t.Error(err)
			return
		}

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

	sink, err := NewFDSink(connection)
	if err != nil {
		t.Fatal(err)
	}

	n, err := v.WriteTo(sink)
	if err != nil || n != size {
		t.Fatal(n, err)
	}

	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if sink.SplicedBytes() < size/2 {
		t.Fatal("no substantial kernel splice", sink.SplicedBytes())
	}

	if len(c.pages) != 0 {
		t.Fatal("body permits leaked")
	}
	// A later response still goes through Transport parsing and body lifecycle.
	other, err := c.Get(ctx, Request{})
	if err != nil {
		t.Fatal(err)
	}

	closeBody(other)
}

func TestValueWriteToSpliceCancellation(t *testing.T) {
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamResponse(w, 0, int64(PageSize), int64(PageSize), `"v"`)
	}))
	c := testClient(t, path, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	v, err := c.Get(ctx, Request{})
	if err != nil {
		t.Fatal(err)
	}

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

	sink, err := NewFDSink(connection)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() { _, err := v.WriteTo(sink); done <- err }()
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

	if len(c.pages) != 0 {
		t.Fatal("canceled splice retained page")
	}
}

func TestValueServeHTTPLifecycle(t *testing.T) {
	const size = int64(PageSize) + 173

	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested, _ := parseRange(r.Header.Get("Range"))

		first, last, err := requested.Resolve(ByteLength(size))
		if err != nil {
			t.Error(err)
			return
		}

		streamResponseHead(w, int64(first), int64(last-first)+1, size, `"v"`)
		_, _ = io.CopyN(w, &offsetStream{offset: int64(first)}, int64(last-first)+1)
	}))
	c := testClient(t, path, 2)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, err := c.Get(r.Context(), Request{})
		if err != nil {
			t.Error(err)
			return
		}

		v.ServeHTTP(w, r)
	}))
	defer server.Close()

	client := &http.Client{Timeout: 10 * time.Second}
	for range 2 {
		response, err := client.Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}

		if response.ContentLength != size || response.Header.Get("ETag") != `"v"` {
			t.Fatal("HTTP metadata")
		}

		n, err := io.Copy(&offsetSink{}, response.Body)
		closeBody(response.Body)

		if err != nil || n != size {
			t.Fatal(n, err)
		}
	}
}
