// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/netutil"

	"github.com/Azure/unbounded/pkg/racersdk"
	"github.com/Azure/unbounded/pkg/racersdk/racersdktest"
)

type sidecarObservedListener struct {
	net.Listener
	bytes       atomic.Int64
	connections atomic.Int64
}

func (l *sidecarObservedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}

	l.connections.Add(1)

	return &sidecarObservedConn{TCPConn: c.(*net.TCPConn), bytes: &l.bytes}, nil
}

type sidecarObservedConn struct {
	*net.TCPConn
	bytes *atomic.Int64
}

func (c *sidecarObservedConn) ReadFrom(r io.Reader) (int64, error) {
	lr, ok := r.(*io.LimitedReader)
	if !ok || lr.N <= 0 || lr.N > 256<<10 {
		return 0, errors.New("unbounded source")
	}

	if _, ok := lr.R.(*net.UnixConn); !ok {
		return 0, errors.New("wrapped Unix source")
	}

	n, err := c.TCPConn.ReadFrom(r)
	c.bytes.Add(n)

	return n, err
}

// Exercise the real SDK, Unix sockets, serveSidecar listener, net/http response,
// and TCPConn.ReadFrom. Counting only ResponseWriter.ReadFrom misses the bug.
func sidecarStreamFixture(tb testing.TB, size int, hideReaderFrom bool) (*http.Client, string, *sidecarObservedListener, []byte) {
	tb.Helper()

	data := bytes.Repeat([]byte("sidecar!"), size/8)

	tag, err := racersdk.ParseETag(`"version"`)
	if err != nil {
		tb.Fatal(err)
	}

	metadata := racersdk.Metadata{Size: racersdk.ByteLength(size), ETag: tag, ExpiresAt: time.Now().Add(time.Hour).Truncate(time.Millisecond)}

	sdk, cleanup, err := racersdktest.NewClient(func(_ context.Context, request racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		if request.Operation() == racersdk.OperationHead {
			return metadata, nil, nil
		}

		return metadata, io.NopCloser(bytes.NewReader(data)), nil
	})
	if err != nil {
		tb.Fatal(err)
	}

	tb.Cleanup(cleanup)

	l, err := listenSidecar(tb.Context(), "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}

	observed := &sidecarObservedListener{Listener: l}

	var listener net.Listener = observed
	if hideReaderFrom {
		listener = netutil.LimitListener(listener, 128)
	}

	ctx, cancel := context.WithCancel(tb.Context())
	done := make(chan error, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, err := sdk.GetStreaming(r.Context(), racersdk.Request{})
		if err != nil {
			tb.Error(err)
			panic(http.ErrAbortHandler)
		}
		defer v.Close()

		w.Header().Set("Content-Length", strconv.Itoa(size))
		w.Header().Set("Content-Type", "application/octet-stream")

		if n, err := v.WriteToHTTP(w); err != nil || n != int64(size) {
			tb.Errorf("transfer: %d, %v", n, err)
			panic(http.ErrAbortHandler)
		}
	})

	go func() { done <- serveSidecar(ctx, listener, handler, 10*time.Second) }()

	tb.Cleanup(func() {
		cancel()

		select {
		case err := <-done:
			if err != nil {
				tb.Error(err)
			}
		case <-time.After(7 * time.Second):
			tb.Error("sidecar did not stop")
		}
	})

	transport := &http.Transport{}
	tb.Cleanup(transport.CloseIdleConnections)

	return &http.Client{Transport: transport, Timeout: 10 * time.Second}, "http://" + l.Addr().String(), observed, data
}

func TestSidecarHTTPReaderFrom(t *testing.T) {
	client, address, observed, data := sidecarStreamFixture(t, 1<<20, false)
	for range 2 {
		response, err := client.Get(address)
		if err != nil {
			t.Fatal(err)
		}

		got, err := io.ReadAll(response.Body)
		response.Body.Close()

		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("integrity: %d, %v", len(got), err)
		}
	}

	if got := observed.bytes.Load(); got < int64(len(data)) {
		t.Fatalf("underlying TCP ReaderFrom bytes = %d, want at least %d", got, len(data))
	}

	if got := observed.connections.Load(); got != 1 {
		t.Fatalf("keepalive connections = %d", got)
	}
}

// This local benchmark includes fake origin/protocol work and HTTP draining,
// not a production dataplane. The hidden variant reproduces the old limiter.
func BenchmarkSidecarHTTPReaderFrom(b *testing.B) {
	for _, hidden := range []bool{true, false} {
		b.Run(map[bool]string{true: "hidden", false: "preserved"}[hidden], func(b *testing.B) {
			client, address, observed, _ := sidecarStreamFixture(b, 16<<20, hidden)
			consume := func() {
				response, err := client.Get(address)
				if err != nil {
					b.Fatal(err)
				}

				n, err := io.Copy(io.Discard, response.Body)
				response.Body.Close()

				if err != nil || n != 16<<20 {
					b.Fatalf("transfer: %d, %v", n, err)
				}
			}
			consume()
			b.ReportAllocs()
			b.SetBytes(16 << 20)
			b.ResetTimer()

			for range b.N {
				consume()
			}

			b.StopTimer()

			if !hidden && observed.bytes.Load() == 0 {
				b.Fatal("TCP ReaderFrom was not reached")
			}
		})
	}
}

type (
	sidecarAcceptResult struct {
		conn net.Conn
		err  error
	}
	sidecarScriptListener struct {
		results chan sidecarAcceptResult
		done    chan struct{}
		once    sync.Once
	}
)

func (l *sidecarScriptListener) Accept() (net.Conn, error) {
	select {
	case r := <-l.results:
		return r.conn, r.err
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *sidecarScriptListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *sidecarScriptListener) Addr() net.Addr { return &net.TCPAddr{} }

func TestSidecarLimitedListenerLifecycle(t *testing.T) {
	base := &sidecarScriptListener{results: make(chan sidecarAcceptResult, 1), done: make(chan struct{})}
	l := limitSidecarListener(base, 1).(*sidecarLimitedListener)

	t.Cleanup(func() { _ = l.Close() })

	sentinel := errors.New("accept failed")
	base.results <- sidecarAcceptResult{err: sentinel}

	if _, err := l.Accept(); !errors.Is(err, sentinel) || len(l.slots) != 0 {
		t.Fatalf("accept failure: %v", err)
	}

	left, right := net.Pipe()
	defer right.Close()

	base.results <- sidecarAcceptResult{conn: left}

	c, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, ok := c.(io.ReaderFrom); ok {
		t.Fatal("invented ReaderFrom")
	}

	if err := c.SetDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("read deadline lost")
	}

	if _, err := c.Write([]byte{1}); err == nil {
		t.Fatal("write deadline lost")
	}

	accepted := make(chan error, 1)

	base.results <- sidecarAcceptResult{err: sentinel}

	go func() { _, err := l.Accept(); accepted <- err }()

	select {
	case <-accepted:
		t.Fatal("limit bypassed")
	case <-time.After(20 * time.Millisecond):
	}

	var closes sync.WaitGroup
	for range 2 {
		closes.Go(func() { _ = c.Close() })
	}

	closes.Wait()

	select {
	case err := <-accepted:
		if !errors.Is(err, sentinel) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("slot not released")
	}

	if len(l.slots) != 0 {
		t.Fatal("slot leaked")
	}

	for _, full := range []bool{false, true} {
		t.Run(strconv.FormatBool(full), func(t *testing.T) {
			base := &sidecarScriptListener{results: make(chan sidecarAcceptResult), done: make(chan struct{})}

			l := limitSidecarListener(base, 1).(*sidecarLimitedListener)
			if full {
				l.slots <- struct{}{}
			}

			done := make(chan error, 1)

			go func() { _, err := l.Accept(); done <- err }()

			_ = l.Close()

			select {
			case err := <-done:
				if !errors.Is(err, net.ErrClosed) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("close did not unblock Accept")
			}
		})
	}
}
