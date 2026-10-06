// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"syscall"
	"testing"
)

type socketErrorResponse struct {
	*httptest.ResponseRecorder
	cause    error
	complete bool
	calls    int
}

func (w *socketErrorResponse) ReadFrom(r io.Reader) (int64, error) {
	w.calls++
	if w.complete {
		return io.Copy(w.ResponseRecorder, r)
	}

	n, err := io.CopyN(w.ResponseRecorder, r, 1)
	if err != nil {
		return n, err
	}

	return n, w.cause
}

func TestStreamingSocketErrorNormalization(t *testing.T) {
	reset := &net.OpError{Op: "read", Net: "unix", Err: syscall.ECONNRESET}
	destinationErr := errors.New("destination failure")

	for _, tt := range []struct {
		name     string
		cause    error
		kind     ErrorKind
		want     error
		complete bool
	}{
		{name: "reset", cause: reset, kind: ErrorIO, want: io.ErrUnexpectedEOF},
		{name: "wrapped reset", cause: fmt.Errorf("read: %w", reset), kind: ErrorIO, want: io.ErrUnexpectedEOF},
		{name: "unexpected EOF", cause: io.ErrUnexpectedEOF, kind: ErrorIO, want: io.ErrUnexpectedEOF},
		{name: "EOF", cause: io.EOF, kind: ErrorIO, want: io.ErrUnexpectedEOF},
		{name: "short nil", kind: ErrorIO, want: io.ErrUnexpectedEOF},
		{name: "canceled", cause: context.Canceled, kind: ErrorCanceled, want: context.Canceled},
		{name: "deadline", cause: context.DeadlineExceeded, kind: ErrorDeadline, want: context.DeadlineExceeded},
		{name: "socket timeout", cause: os.ErrDeadlineExceeded, kind: ErrorIO, want: os.ErrDeadlineExceeded},
		{name: "destination", cause: destinationErr, kind: ErrorIO, want: destinationErr},
		{name: "success", complete: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const size = 2 * copyBufferSize

			path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				streamResponse(w, 0, size, size, `"v"`)
			}))
			c := testClient(t, path, 1)

			v, err := c.GetStreaming(t.Context(), Request{})
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(v)

			w := &socketErrorResponse{ResponseRecorder: httptest.NewRecorder(), cause: tt.cause, complete: tt.complete}

			n, err := v.WriteToHTTP(w)
			if w.calls == 0 {
				t.Fatal("ReaderFrom was not called")
			}

			if n != int64(w.Body.Len()) {
				t.Fatalf("reported bytes=%d, delivered=%d", n, w.Body.Len())
			}

			if tt.complete {
				if err != nil || n != size {
					t.Fatalf("successful transfer: bytes=%d err=%v", n, err)
				}
			} else {
				assertKind(t, err, tt.kind)

				if !errors.Is(err, tt.want) {
					t.Fatalf("error %v does not wrap %v", err, tt.want)
				}

				if tt.cause != nil && tt.cause != io.EOF && !errors.Is(err, tt.cause) {
					t.Fatal("underlying error lost", err)
				}

				if errors.Is(tt.cause, syscall.ECONNRESET) {
					var got *net.OpError
					if !errors.As(err, &got) || got != reset {
						t.Fatal("underlying network error lost", err)
					}
				}

				if n <= 0 || n >= size {
					t.Fatal("failed transfer did not preserve partial count", n)
				}
			}

			if c.Stats().ActiveBulk != 0 || len(c.copySlots) != 0 {
				t.Fatal("transfer retained admission", c.Stats())
			}
		})
	}
}
