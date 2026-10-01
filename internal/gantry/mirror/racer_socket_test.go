// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

type racerSocketResponse struct {
	http.ResponseWriter
	bytes *atomic.Int64
}

func (w racerSocketResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type racerSocketFallback struct{ http.ResponseWriter }

func (w racerSocketFallback) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w racerSocketResponse) ReadFrom(r io.Reader) (int64, error) {
	source, ok := r.(*io.LimitedReader)
	if !ok || source.N <= 0 || source.N > racerSocketChunk {
		return 0, errors.New("socket transfer is not bounded")
	}

	if _, unix := source.R.(*net.UnixConn); !unix {
		return 0, errors.New("socket hidden by wrapper or nested limiter")
	}

	fast, ok := w.ResponseWriter.(io.ReaderFrom)
	if !ok {
		return 0, errors.New("net/http ReaderFrom unavailable")
	}

	n, err := fast.ReadFrom(source)
	w.bytes.Add(n)

	return n, err
}

func TestRacerSocketHTTPReuse(t *testing.T) {
	for _, mode := range []string{"plaintext", "TLS", "fallback"} {
		t.Run(mode, func(t *testing.T) { testRacerSocketHTTPReuse(t, mode) })
	}
}

func testRacerSocketHTTPReuse(t *testing.T, mode string) {
	t.Helper()
	// Exercise the exact bounded raw source shape supplied by GetStreaming.
	// The SDK's public fake uses TCP, so socket forwarding is tested separately
	// from the SDK-to-mirror integration without modifying canonical /run paths.
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(t.TempDir(), "s"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { listener.Close() })

	data := bytes.Repeat([]byte("0123456789abcdef"), racerSocketChunk/4+1)

	var fastBytes, connections atomic.Int64

	observed := make(chan RacerHTTPObservation, 1)
	handler := RacerHTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset, err := strconv.Atoi(r.URL.Query().Get("offset"))
		if err != nil {
			t.Error(err)
			return
		}

		conn, err := net.DialUnix("unix", nil, listener.Addr().(*net.UnixAddr))
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()

		peer, err := listener.AcceptUnix()
		if err != nil {
			t.Error(err)
			return
		}

		done := make(chan error, 1)

		go func() {
			defer peer.Close()

			if err := peer.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
				done <- err
				return
			}

			_, err := peer.Write(data[offset:])
			done <- err
		}()

		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)-offset))

		if offset != 0 {
			w.WriteHeader(http.StatusPartialContent)
		}

		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Error(err)
		}

		if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Error(err)
		}

		n, err := w.(io.ReaderFrom).ReadFrom(&io.LimitedReader{R: conn, N: int64(len(data) - offset)})
		if err != nil || n != int64(len(data)-offset) {
			t.Errorf("socket transfer: bytes=%d err=%v", n, err)
		}

		if err := <-done; err != nil {
			t.Error(err)
		}
	}), time.Second, func(o RacerHTTPObservation) { observed <- o })
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w = racerSocketResponse{w, &fastBytes}
		if mode == "fallback" {
			w = racerSocketFallback{w}
		}

		handler.ServeHTTP(w, r)
	}))

	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	if mode == "TLS" {
		server.StartTLS()
	} else {
		server.Start()
	}

	t.Cleanup(server.Close)
	server.Client().Timeout = 10 * time.Second

	var total int64

	for _, offset := range []int{0, 17, len(data) - 1} {
		resp, err := server.Client().Get(server.URL + "?offset=" + strconv.Itoa(offset))
		if err != nil {
			t.Fatal(err)
		}

		got, err := io.ReadAll(resp.Body)
		resp.Body.Close()

		status := http.StatusOK
		if offset != 0 {
			status = http.StatusPartialContent
		}

		if err != nil || !bytes.Equal(got, data[offset:]) || resp.StatusCode != status || resp.ProtoMajor != 1 || resp.Close {
			t.Fatalf("response: status=%d bytes=%d err=%v", resp.StatusCode, len(got), err)
		}

		select {
		case o := <-observed:
			if o.Aborted || o.Bytes != int64(len(got)) || o.Status != status {
				t.Fatalf("observation=%+v", o)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("missing observation")
		}

		total += int64(len(got))
	}

	if mode == "fallback" {
		total = 0
	}

	if fastBytes.Load() != total || connections.Load() != 1 {
		t.Fatalf("bounded fast bytes=%d want=%d connections=%d", fastBytes.Load(), total, connections.Load())
	}
}
