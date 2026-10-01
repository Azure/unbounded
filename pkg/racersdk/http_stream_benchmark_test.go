// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// BenchmarkHTTPStream compares the buffered and streaming SDK paths over real
// Unix sockets and plaintext loopback HTTP/1.1. The existing protocol fixture
// generates payloads in bounded scratch, not from the Rust dataplane or disk.
// Requests are sequential with one page credit and a warmed HTTP connection.
// Timing includes generation, UDS dialing/framing, HTTP delivery, and draining.
// Allocations cover the whole process, not just the SDK, and exclude warmup;
// pooled page memory retained by Get is not a per-operation allocation metric.
func BenchmarkHTTPStream(b *testing.B) {
	for _, size := range []int64{16 << 20, 32 << 20} {
		b.Run(fmt.Sprintf("%dMiB", size>>20), func(b *testing.B) {
			path := benchmarkPeer(b, size, false)

			for _, streaming := range []bool{false, true} {
				name := "Get"
				if streaming {
					name = "GetStreaming"
				}

				b.Run(name, func(b *testing.B) {
					benchmarkHTTPStream(b, path, size, streaming)
				})
			}
		})
	}
}

func benchmarkHTTPStream(b *testing.B, path string, size int64, streaming bool) {
	b.Helper()

	c, err := newClient(ClientConfig{Cache: CacheName{value: "bench"}}, path)
	if err != nil {
		b.Fatal(err)
	}
	defer closeBody(c)

	get := c.Get
	if streaming {
		get = c.GetStreaming
	}

	// Join handler cleanup before starting the next operation, including when
	// the client observes the final Content-Length byte before the handler exits.
	finished := make(chan error, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var transferErr error

		defer func() { finished <- transferErr }()

		v, err := get(r.Context(), Request{}, ReadOptions{PageCredits: 1, ByteCredits: PageSize})
		if err != nil {
			transferErr = err

			http.Error(w, "get failed", http.StatusBadGateway)

			return
		}
		defer closeBody(v)

		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))

		n, err := v.WriteToHTTP(w)
		if err != nil || n != size {
			transferErr = fmt.Errorf("HTTP transfer: bytes=%d, want=%d, error=%v", n, size, err)

			panic(http.ErrAbortHandler)
		}
	}))
	defer server.Close()

	httpClient := server.Client()
	httpClient.Timeout = 15 * time.Second

	consume := func() {
		b.Helper()

		req, err := http.NewRequestWithContext(b.Context(), http.MethodGet, server.URL, nil)
		if err != nil {
			b.Fatal(err)
		}

		res, err := httpClient.Do(req)
		if err != nil {
			b.Fatal(err)
		}

		n, err := io.Copy(io.Discard, res.Body)
		closeBody(res.Body)

		if err != nil || n != size || res.StatusCode != http.StatusOK || res.ProtoMajor != 1 {
			b.Fatalf("response: bytes=%d, want=%d, status=%s, protocol=%s, error=%v", n, size, res.Status, res.Proto, err)
		}

		if err := <-finished; err != nil {
			b.Fatal(err)
		}
	}
	consume()

	b.ReportAllocs()
	b.SetBytes(size)
	b.ResetTimer()

	for range b.N {
		consume()
	}

	b.StopTimer()
}
