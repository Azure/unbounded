// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSidecarHeaderLimit(t *testing.T) {
	var calls atomic.Int32

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})

	address, cancel, done := startTestSidecar(t, handler, time.Second)
	defer func() {
		cancel()
		waitSidecar(t, done)
	}()

	for _, shape := range []struct {
		name, uri, host, headers string
	}{
		{"single", "/bucket/key", "localhost", ""},
		{"repeated", "/bucket/key", "localhost", strings.Repeat("X-Part: "+strings.Repeat("a", 100)+"\r\n", 100)},
		{"empty", "/bucket/key", "localhost", strings.Repeat("X-Empty: \r\n", 100)},
		{"long-name", "/bucket/key", "localhost", strings.Repeat("K", 1024) + ": value\r\n"},
		{"uri", "/bucket/" + strings.Repeat("k", 1024), "localhost", ""},
		{"host", "/bucket/key", strings.Repeat("h", 1024), ""},
	} {
		for _, size := range []int{16 << 10, (32 << 10) - 1, 32 << 10, (32 << 10) + 1, 33 << 10, 40 << 10} {
			t.Run(fmt.Sprintf("%s/%d", shape.name, size), func(t *testing.T) {
				padding := size - len(shape.uri) - len(shape.host) - len(shape.headers) - len("X-Pad: \r\n")
				request := "GET " + shape.uri + " HTTP/1.1\r\nHost: " + shape.host + "\r\n" +
					shape.headers + "X-Pad: " + strings.Repeat("p", padding) + "\r\n\r\n"

				conn, err := net.DialTimeout("tcp", strings.TrimPrefix(address, "http://"), time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()

				if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
					t.Fatal(err)
				}

				before := calls.Load()

				if _, err := io.WriteString(conn, request); err != nil {
					t.Fatal(err)
				}

				response, err := http.ReadResponse(bufio.NewReader(conn), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()

				wantStatus, wantCalls := http.StatusNoContent, int32(1)
				if size > 32<<10 {
					wantStatus, wantCalls = http.StatusRequestHeaderFieldsTooLarge, 0
				}

				if response.StatusCode != wantStatus {
					t.Errorf("status = %d, want %d", response.StatusCode, wantStatus)
				}

				if got := calls.Load() - before; got != wantCalls {
					t.Errorf("sidecar handler calls = %d, want %d", got, wantCalls)
				}
			})
		}
	}
}
