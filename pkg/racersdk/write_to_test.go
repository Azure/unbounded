// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type writeFunc func([]byte) (int, error)

func TestServeHTTPPreservesContentTypeAndRejectsPartialValue(t *testing.T) {
	for _, partial := range []bool{false, true} {
		v := copyTestValue(t, io.NopCloser(strings.NewReader("abc")), 3)
		v.metadata.Size = 3

		v.metadata.ContentType = "text/plain"
		if partial {
			v.end = 2
		}

		response := httptest.NewRecorder()
		v.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))

		if partial {
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("partial value status: %d", response.Code)
			}
		} else if response.Code != http.StatusOK || response.Body.String() != "abc" || response.Header().Get("Content-Type") != "text/plain" || response.Header().Get("Content-Length") != "3" {
			t.Fatalf("response: %d %v %q", response.Code, response.Header(), response.Body.String())
		}
	}
}

func (f writeFunc) Write(p []byte) (int, error) { return f(p) }

type copyDestination struct {
	bytes.Buffer
	readFrom bool
	maxWrite int
}

func (w *copyDestination) ReadFrom(io.Reader) (int64, error) {
	w.readFrom = true
	return 0, errors.New("unexpected ReaderFrom")
}

func (w *copyDestination) Write(p []byte) (int, error) {
	w.maxWrite = max(w.maxWrite, len(p))
	return w.Buffer.Write(p)
}

// Exercise copying through the supported subscription transport, including page
// verification, ordered delivery, and admission cleanup.
func copyTestValue(t *testing.T, source io.ReadCloser, length int64) *Value {
	t.Helper()
	t.Cleanup(func() { closeBody(source) })
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		streamResponseHead(w, 0, length, length, `"v"`)
		_, _ = io.Copy(w, source)
	}))
	c := testClient(t, path, 1)

	v, err := c.Get(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { closeBody(v) })

	return v
}

func TestWriteToUsesBoundedBufferWithoutReaderFrom(t *testing.T) {
	data := strings.Repeat("xyz", copyBufferSize+13)
	v := copyTestValue(t, io.NopCloser(strings.NewReader(data)), int64(len(data)))
	w := &copyDestination{}

	n, err := io.Copy(w, v)
	if err != nil || n != int64(len(data)) || w.String() != data || w.readFrom || w.maxWrite != copyBufferSize {
		t.Fatal("copy chunking or ReaderFrom dispatch", n, err, w.readFrom, w.maxWrite)
	}

	if n, err := v.WriteTo(w); n != 0 || err != nil {
		t.Fatal("repeated EOF", n, err)
	}

	if len(v.client.copySlots) != 0 || len(v.client.copyBuffers) != 1 {
		t.Fatal("copy buffer not returned")
	}
}

func TestWriteToWriterFailures(t *testing.T) {
	sentinel := errors.New("writer failed")
	for _, tt := range []struct {
		name    string
		n       int
		err     error
		wantN   int64
		wantErr error
	}{
		{"zero", 0, nil, 0, io.ErrShortWrite},
		{"short", 2, nil, 2, io.ErrShortWrite},
		{"error", 0, sentinel, 0, sentinel},
		{"partial error", 2, sentinel, 2, sentinel},
		{"full error", 4, sentinel, 4, sentinel},
		{"negative", -1, nil, 0, io.ErrShortWrite},
		{"excess", 5, nil, 0, io.ErrShortWrite},
		{"negative error", -1, sentinel, 0, sentinel},
		{"excess error", 5, sentinel, 0, sentinel},
	} {
		t.Run(tt.name, func(t *testing.T) {
			v := copyTestValue(t, io.NopCloser(strings.NewReader("data")), 4)
			calls := 0

			n, err := v.WriteTo(writeFunc(func(p []byte) (int, error) {
				calls++

				if string(p) != "data" {
					t.Fatal("unexpected writer data")
				}

				return tt.n, tt.err
			}))
			if n != tt.wantN || !errors.Is(err, tt.wantErr) || calls != 1 {
				t.Fatal(n, err, calls)
			}

			if len(v.client.copySlots) != 0 {
				t.Fatal("writer failure retained copy buffer")
			}

			closeBody(v)

			if len(v.client.slots) != 0 {
				t.Fatal("writer failure retained admission after Close")
			}
		})
	}
}

func TestWriteToPartialReadErrorsAndNoProgress(t *testing.T) {
	// Incomplete pages and a missing Complete frame never expose unverified
	// bytes. Source errors cross the wire as truncation, not Go error identities.
	for _, payload := range []string{"dat", "data"} {
		c := rawSubscriptionClient(t, func(conn net.Conn, _ *bufio.Reader, _ []byte) {
			_, _ = io.WriteString(conn, subscriptionHead(4, 0, 4))
			_ = fakeSubscriptionFrame(conn, 1, 0, 0, 4)
			_, _ = io.WriteString(conn, payload)
		})

		v, err := c.Get(t.Context(), Request{})
		if err != nil {
			t.Fatal(err)
		}

		var dst bytes.Buffer

		n, err := v.WriteTo(&dst)
		if n != 0 || dst.Len() != 0 || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatal(n, err, dst.String())
		}

		if _, err := v.Read(nil); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatal("source error not terminal", err)
		}
	}

	// No progress on a subscription is bounded by its context, rather than a
	// synthetic reader repeatedly returning (0, nil).
	c := rawSubscriptionClient(t, func(conn net.Conn, reader *bufio.Reader, _ []byte) {
		_, _ = io.WriteString(conn, subscriptionHead(1, 0, 1))
		_, _ = reader.ReadByte()
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	v, err := c.Get(ctx, Request{})
	if err != nil {
		t.Fatal(err)
	}

	cancel()

	if n, err := v.WriteTo(io.Discard); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatal(n, err)
	}
}

func TestWriteToCancellationFromWriter(t *testing.T) {
	for _, action := range []string{"context", "value", "client"} {
		t.Run(action, func(t *testing.T) {
			v := copyTestValue(t, io.NopCloser(io.LimitReader(repeatedByte('x'), 2*copyBufferSize)), 2*copyBufferSize)

			n, err := v.WriteTo(writeFunc(func(p []byte) (int, error) {
				switch action {
				case "context":
					v.cancel()
				case "value":
					closeBody(v)
				case "client":
					closeBody(v.client)
				}

				return len(p), nil
			}))
			if n != copyBufferSize {
				t.Fatal("read after cancellation", n)
			}

			if action == "context" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			} else {
				assertKind(t, err, ErrorClosed)
			}
		})
	}
}

func TestWriteToScratchBoundWithCanceledBlockedWriter(t *testing.T) {
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { streamResponse(w, 0, 1, 1, `"v"`) }))
	c := testClient(t, path, 1)

	v, err := c.Get(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}

	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)

	done := make(chan error, 1)

	go func() {
		_, err := v.WriteTo(writeFunc(func(p []byte) (int, error) { close(entered); <-release; return len(p), nil }))
		done <- err
	}()

	<-entered
	closeBody(v)

	if len(c.slots) != 0 || len(c.copySlots) != 1 {
		t.Fatal("cancellation did not separate value and scratch lifetimes")
	}

	next, err := c.Get(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(next)

	if n, err := next.WriteTo(io.Discard); n != 0 {
		t.Fatal(n, err)
	} else {
		assertKind(t, err, ErrorUnavailable)
	}
	// Ordinary Read remains usable without SDK copy scratch.
	if data, err := io.ReadAll(next); err != nil || string(data) != "x" {
		t.Fatal(string(data), err)
	}

	closeBody(c)

	if len(c.copyBuffers) != 0 {
		t.Fatal("closed client retained idle scratch")
	}
	// Unblock without closing twice in deferred cleanup.
	release <- struct{}{}

	select {
	case err := <-done:
		assertKind(t, err, ErrorClosed)
	case <-time.After(time.Second):
		t.Fatal("copy did not return after writer unblocked")
	}

	if len(c.copySlots) != 0 || len(c.copyBuffers) != 0 {
		t.Fatal("copy returned scratch to closed client")
	}
}
