// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type writeFunc func([]byte) (int, error)

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

// Deterministic source chunking tests the copy loop separately from Unix packet
// sizes. It still uses the production Value state machine and admission cleanup.
func copyTestValue(t *testing.T, source io.ReadCloser, length int64) *Value {
	t.Helper()
	c := testClient(t, "unused", 1)

	v, err := c.admit(context.Background(), &c.bulk, OriginRequest{})
	if err != nil {
		t.Fatal(err)
	}

	v.body, v.remaining, v.end = source, length, length

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

type copyFinalErrorReader struct{ err error }

func (r copyFinalErrorReader) Read(p []byte) (int, error) { return copy(p, "data"), r.err }

type noProgressReader struct{}

func (noProgressReader) Read([]byte) (int, error) { return 0, nil }

func TestWriteToPartialReadErrorsAndNoProgress(t *testing.T) {
	sentinel := errors.New("source failed")
	for _, sourceErr := range []error{sentinel, io.EOF} {
		v := copyTestValue(t, io.NopCloser(copyFinalErrorReader{err: sourceErr}), 5)

		var dst bytes.Buffer

		n, err := v.WriteTo(&dst)

		want := sourceErr
		if sourceErr == io.EOF {
			want = io.ErrUnexpectedEOF
		}

		if n != 4 || dst.String() != "data" || !errors.Is(err, want) {
			t.Fatal(n, err, dst.String())
		}

		if _, err := v.Read(nil); !errors.Is(err, want) {
			t.Fatal("source error not terminal", err)
		}
	}

	v := copyTestValue(t, io.NopCloser(noProgressReader{}), 1)
	if n, err := v.WriteTo(io.Discard); n != 0 || !errors.Is(err, io.ErrNoProgress) {
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
