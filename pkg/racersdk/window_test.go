// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestValueWindowOrderedAndBounded(t *testing.T) {
	const size = 5*int64(PageSize) + 17

	entered := make(chan uint64, 8)
	release := make(chan struct{})

	var calls atomic.Int32

	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)

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

		if first != 0 {
			if r.Header.Get("If-Match") != `"v"` {
				t.Error("pin lost")
			}

			entered <- uint64(first)

			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}

		streamResponseHead(w, int64(first), int64(last-first)+1, size, `"v"`)
		_, _ = io.CopyN(w, &offsetStream{offset: int64(first)}, int64(last-first)+1)
	}))
	c := testClient(t, path, 3)

	v, err := c.Get(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(v)

	if _, err := io.CopyN(io.Discard, v, int64(PageSize)); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() {
		_, err := io.Copy(&offsetSink{offset: int64(PageSize)}, v)
		done <- err
	}()

	seen := make(map[uint64]bool)

	for range 3 {
		select {
		case first := <-entered:
			seen[first] = true
		case <-time.After(3 * time.Second):
			t.Fatal("pages did not open concurrently")
		}
	}

	if len(seen) != 3 || !seen[uint64(PageSize)] || !seen[2*uint64(PageSize)] || !seen[3*uint64(PageSize)] {
		t.Fatal("unexpected page window", seen)
	}

	if calls.Load() != 4 || len(c.pages) != 3 {
		t.Fatal("window exceeded pool bounds")
	}

	close(release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ordered stream stalled")
	}

	if calls.Load() != 6 || len(c.pages) != 0 {
		t.Fatal("wrong requests or retained permits")
	}
}

func TestValueWindowCloseCancelsEveryWorker(t *testing.T) {
	entered := make(chan struct{}, 3)
	stopped := make(chan struct{}, 3)
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-Match") == "" {
			streamResponse(w, 0, int64(PageSize), 8*int64(PageSize), `"v"`)
			return
		}

		entered <- struct{}{}

		<-r.Context().Done()

		stopped <- struct{}{}
	}))
	c := testClient(t, path, 3)

	v, err := c.Get(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := io.CopyN(io.Discard, v, int64(PageSize)); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() { _, err := io.Copy(io.Discard, v); done <- err }()

	for range 3 {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("worker not started")
		}
	}

	closeBody(v)

	select {
	case err := <-done:
		assertKind(t, err, ErrorClosed)
	case <-time.After(3 * time.Second):
		t.Fatal("consumer not canceled")
	}

	for range 3 {
		select {
		case <-stopped:
		case <-time.After(3 * time.Second):
			t.Fatal("worker not canceled")
		}
	}

	if len(c.pages) != 0 || len(c.slots) != 0 {
		t.Fatal("Close retained permits")
	}
}
