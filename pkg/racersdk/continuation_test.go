// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientContinuationAbsoluteDeadline(t *testing.T) {
	const (
		budget    = 2 * time.Second
		pageDelay = 1100 * time.Millisecond
		size      = 3*int64(PageSize) + 13
	)

	var calls atomic.Int32

	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)

		controller := http.NewResponseController(w)
		// Model the listener's absolute per-operation budget. Progress does not
		// renew it, including when an old SDK asks for several pages at once.
		if err := controller.SetWriteDeadline(time.Now().Add(budget)); err != nil {
			t.Error(err)
			return
		}

		defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()

		requested, err := parseRange(r.Header.Get("Range"))
		if err != nil {
			t.Error(err)
			return
		}

		first, last, err := requested.Resolve(ByteLength(size))
		if err != nil {
			t.Error(err)
			return
		}

		streamResponseHead(w, int64(first), int64(last-first)+1, size, `"v"`)

		if err := controller.Flush(); err != nil {
			return
		}

		for start := int64(first); start <= int64(last); start += int64(PageSize) {
			timer := time.NewTimer(pageDelay)
			select {
			case <-timer.C:
			case <-r.Context().Done():
				timer.Stop()
				return
			}

			length := min(int64(PageSize), int64(last)-start+1)
			if _, err := io.CopyN(w, repeatedByte('x'), length); err != nil {
				return
			}

			if err := controller.Flush(); err != nil {
				return
			}
		}
	}))
	c := testClient(t, path, 1)

	v, err := c.Get(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := io.CopyN(io.Discard, v, int64(PageSize)); err != nil {
		t.Fatal(err)
	}

	started := time.Now()

	n, err := io.Copy(io.Discard, v)
	if err != nil || n != size-int64(PageSize) {
		t.Fatalf("continuations exceeded a single request budget: %d %v", n, err)
	}

	if time.Since(started) <= budget || calls.Load() != 4 {
		t.Fatal("test did not exercise multiple requests beyond the absolute budget")
	}
}

func TestClientLaterContinuationCancellation(t *testing.T) {
	for _, bodyStarted := range []bool{false, true} {
		t.Run(strconv.FormatBool(bodyStarted), func(t *testing.T) {
			for _, action := range []string{"context", "value", "client"} {
				t.Run(action, func(t *testing.T) {
					const size = 2*int64(PageSize) + 2

					var calls atomic.Int32

					entered, stopped := make(chan struct{}), make(chan struct{})
					path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						call := calls.Add(1)
						if call <= 2 {
							streamResponse(w, int64(call-1)*int64(PageSize), int64(PageSize), size, `"v"`)
							return
						}

						if bodyStarted {
							streamResponseHead(w, 2*int64(PageSize), 2, size, `"v"`)

							if err := http.NewResponseController(w).Flush(); err != nil {
								t.Error(err)
							}
						}

						close(entered)
						<-r.Context().Done()
						close(stopped)
					}))
					c := testClient(t, path, 1)

					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()

					v, err := c.Get(ctx, Request{})
					if err != nil {
						t.Fatal(err)
					}

					if n, err := io.CopyN(io.Discard, v, 2*int64(PageSize)); err != nil || n != 2*int64(PageSize) {
						t.Fatal(n, err)
					}

					if n, err := v.Read(nil); n != 0 || err != nil || calls.Load() != 2 || len(c.slots) != 1 {
						t.Fatal("page boundary eagerly continued or released capacity", n, err)
					}

					waitCtx, stopWait := context.WithTimeout(context.Background(), 20*time.Millisecond)
					defer stopWait()

					if _, err := c.Get(waitCtx, Request{}); !errors.Is(err, context.DeadlineExceeded) {
						t.Fatal("live Value did not retain its slot", err)
					}

					done := make(chan error, 1)

					go func() { _, err := io.Copy(io.Discard, v); done <- err }()

					select {
					case <-entered:
					case <-time.After(3 * time.Second):
						t.Fatal("later continuation not opened")
					}

					switch action {
					case "context":
						cancel()
					case "value":
						closeBody(v)
					case "client":
						closeBody(c)
					}

					select {
					case err := <-done:
						if action == "context" {
							if !errors.Is(err, context.Canceled) {
								t.Fatal(err)
							}
						} else {
							assertKind(t, err, ErrorClosed)
						}
					case <-time.After(3 * time.Second):
						t.Fatal("later continuation retained after cancellation")
					}

					select {
					case <-stopped:
					case <-time.After(3 * time.Second):
						t.Fatal("continuation connection retained")
					}

					closeBody(v)

					if len(c.slots) != 0 || calls.Load() != 3 {
						t.Fatal("cancellation retained capacity or retried")
					}
				})
			}
		})
	}
}
