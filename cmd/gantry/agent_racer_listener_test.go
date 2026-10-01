// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/mirror"
)

type racerObservedListener struct {
	net.Listener
	bytes atomic.Int64
}

type racerAcceptResult struct {
	conn net.Conn
	err  error
}

type racerScriptListener struct {
	results chan racerAcceptResult
	done    chan struct{}
	once    sync.Once
}

func (l *racerScriptListener) Accept() (net.Conn, error) {
	select {
	case result := <-l.results:
		return result.conn, result.err
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *racerScriptListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *racerScriptListener) Addr() net.Addr { return &net.TCPAddr{} }

func TestRacerLimitedListenerLifecycle(t *testing.T) {
	base := &racerScriptListener{results: make(chan racerAcceptResult, 1), done: make(chan struct{})}
	l := limitRacerListener(base, 1).(*racerLimitedListener)

	t.Cleanup(func() { _ = l.Close() })

	sentinel := errors.New("accept failed")
	base.results <- racerAcceptResult{err: sentinel}

	if _, err := l.Accept(); !errors.Is(err, sentinel) || len(l.slots) != 0 {
		t.Fatalf("accept failure leaked slot: %v", err)
	}

	left, right := net.Pipe()

	t.Cleanup(func() { _ = right.Close() })

	base.results <- racerAcceptResult{conn: left}

	c, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = c.Close() })

	if _, ok := c.(io.ReaderFrom); ok {
		t.Fatal("invented ReaderFrom for unsupported connection")
	}

	if err := c.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("read deadline not forwarded")
	}

	if err := c.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Write([]byte{1}); err == nil {
		t.Fatal("write deadline not forwarded")
	}

	accepted := make(chan racerAcceptResult, 1)

	base.results <- racerAcceptResult{err: sentinel}

	go func() { conn, acceptErr := l.Accept(); accepted <- racerAcceptResult{conn, acceptErr} }()

	select {
	case <-accepted:
		t.Fatal("connection limit bypassed")
	case <-time.After(20 * time.Millisecond):
	}

	var closes sync.WaitGroup
	for range 2 {
		closes.Go(func() { _ = c.Close() })
	}

	closes.Wait()

	select {
	case result := <-accepted:
		if !errors.Is(result.err, sentinel) {
			t.Fatal(result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not release slot")
	}

	if len(l.slots) != 0 {
		t.Fatal("slot leaked")
	}
	// Closing the listener must unblock Accept both at the semaphore and inside
	// the underlying listener, without waiting for existing connections to close.
	for _, full := range []bool{false, true} {
		t.Run(strconv.FormatBool(full), func(t *testing.T) {
			b := &racerScriptListener{results: make(chan racerAcceptResult), done: make(chan struct{})}

			limited := limitRacerListener(b, 1).(*racerLimitedListener)
			if full {
				limited.slots <- struct{}{}
			}

			done := make(chan error, 1)

			go func() { _, acceptErr := limited.Accept(); done <- acceptErr }()

			_ = limited.Close()

			select {
			case acceptErr := <-done:
				if !errors.Is(acceptErr, net.ErrClosed) {
					t.Fatal(acceptErr)
				}
			case <-time.After(time.Second):
				t.Fatal("listener close did not unblock accept")
			}
		})
	}
}

func (l *racerObservedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}

	return &racerObservedConn{TCPConn: c.(*net.TCPConn), bytes: &l.bytes}, nil
}

type racerObservedConn struct {
	*net.TCPConn
	bytes *atomic.Int64
}

func (c *racerObservedConn) ReadFrom(r io.Reader) (int64, error) {
	n, err := c.TCPConn.ReadFrom(r)
	c.bytes.Add(n)

	return n, err
}

func TestRacerLimitedListenerHTTPReaderFrom(t *testing.T) {
	unix, err := net.Listen("unix", filepath.Join(t.TempDir(), "source.sock"))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = unix.Close() })

	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	observed := &racerObservedListener{Listener: tcp}
	listener := limitRacerListener(observed, 1)
	data := bytes.Repeat([]byte("verified Unix to TCP payload"), 32768)
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: mirror.RacerHTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		source, dialErr := net.DialTimeout("unix", unix.Addr().String(), time.Second)
		if dialErr != nil {
			t.Error(dialErr)
			return
		}
		defer source.Close()

		peer, acceptErr := unix.Accept()
		if acceptErr != nil {
			t.Error(acceptErr)
			return
		}

		done := make(chan error, 1)

		go func() {
			defer peer.Close()

			_ = peer.SetWriteDeadline(time.Now().Add(3 * time.Second))

			_, writeErr := peer.Write(data)
			done <- writeErr
		}()

		_ = source.SetReadDeadline(time.Now().Add(3 * time.Second))

		w.Header().Set("Content-Length", strconv.Itoa(len(data)))

		if flushErr := http.NewResponseController(w).Flush(); flushErr != nil {
			t.Error(flushErr)
			return
		}

		n, copyErr := w.(io.ReaderFrom).ReadFrom(&io.LimitedReader{R: source, N: int64(len(data))})
		if copyErr != nil || n != int64(len(data)) {
			t.Errorf("transfer: %d, %v", n, copyErr)
		}

		if writeErr := <-done; writeErr != nil {
			t.Error(writeErr)
		}
	}), time.Second, nil)}
	served := make(chan error, 1)

	go func() { served <- server.Serve(listener) }()

	t.Cleanup(func() {
		_ = server.Close()

		if serveErr := <-served; !errors.Is(serveErr, http.ErrServerClosed) {
			t.Error(serveErr)
		}
	})

	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)

	client := &http.Client{Transport: transport, Timeout: 4 * time.Second}
	for range 2 {
		response, getErr := client.Get("http://" + tcp.Addr().String())
		if getErr != nil {
			t.Fatal(getErr)
		}

		got, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()

		if readErr != nil || !bytes.Equal(got, data) {
			t.Fatalf("integrity: %d bytes, %v", len(got), readErr)
		}
	}

	if got := observed.bytes.Load(); got != 2*int64(len(data)) {
		t.Fatalf("underlying TCP ReaderFrom bytes = %d, want %d", got, 2*len(data))
	}
}
