// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

type transferResponse struct {
	http.ResponseWriter
	fast *atomic.Int64
}

func (w transferResponse) ReadFrom(r io.Reader) (int64, error) {
	if lr, ok := r.(*io.LimitedReader); ok {
		if _, ok := lr.R.(*net.UnixConn); ok {
			w.fast.Add(lr.N)
		}
	}

	return w.ResponseWriter.(io.ReaderFrom).ReadFrom(r)
}

func TestHTTPTransferKeepsConnectionsAndRanges(t *testing.T) {
	const size = 2*int64(PageSize) + 173

	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		selected, err := parseRange(r.Header.Get("Range"))
		if err != nil {
			t.Error(err)
			return
		}

		first, last, err := selected.resolve(ByteLength(size))
		if err != nil {
			t.Error(err)
			return
		}

		streamResponseHead(w, int64(first), int64(last-first)+1, size, `"v"`)
		_, _ = io.CopyN(w, &offsetStream{offset: int64(first)}, int64(last-first)+1)
	}))
	c := testClient(t, path, 2)

	var fast, connections atomic.Int64

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		options := ReadOptions{}

		if offset != 0 {
			metadata := Metadata{Size: ByteLength(size), ETag: ETag{value: `"v"`}, ExpiresAt: time.Unix(2000000000, 0)}
			options = ReadOptions{Offset: ByteOffset(offset), Pin: metadata.ETag, Metadata: &metadata}
		}

		v, err := c.Get(r.Context(), Request{}, options)
		if err != nil {
			t.Error(err)
			http.Error(w, "get failed", http.StatusBadGateway)

			return
		}
		defer closeBody(v)

		w.Header().Set("Content-Length", strconv.FormatInt(size-offset, 10))

		if offset != 0 {
			w.WriteHeader(http.StatusPartialContent)
		}

		if n, err := v.WriteToHTTP(transferResponse{w, &fast}); err != nil || n != size-offset {
			t.Errorf("transfer %d: %v", n, err)
		}
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}

	server.Start()
	defer server.Close()

	for _, offset := range []int64{0, int64(PageSize) + 7, 0} {
		response, err := server.Client().Get(server.URL + "?offset=" + strconv.FormatInt(offset, 10))
		if err != nil {
			t.Fatal(err)
		}

		n, err := io.Copy(&offsetSink{offset: offset}, response.Body)
		closeBody(response.Body)

		if err != nil || n != size-offset {
			t.Fatal(n, err)
		}
	}

	if connections.Load() != 1 || fast.Load() < size {
		t.Fatal("keep-alive/fast path", connections.Load(), fast.Load())
	}

	if c.Stats().ActiveBulk != 0 {
		t.Fatal("retained admission")
	}
}

func TestHTTPTransferFallbackAndTruncation(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		t.Run(strconv.FormatBool(truncated), func(t *testing.T) {
			path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				streamResponseHead(w, 0, 8192, 8192, `"v"`)

				length := int64(8192)
				if truncated {
					length--
				}

				_, _ = io.CopyN(w, &offsetStream{}, length)
			}))
			c := testClient(t, path, 1)

			v, err := c.Get(context.Background(), Request{})
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(v)

			n, err := v.WriteToHTTP(httptest.NewRecorder())
			if truncated != (err != nil) || !truncated && n != 8192 {
				t.Fatal(n, err)
			}
		})
	}
}
