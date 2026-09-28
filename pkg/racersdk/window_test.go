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

		if r.Header.Get("Racer-Ordered") != "1" || r.Header.Get("Racer-Page-Credits") != "3" {
			t.Error("ordered credits lost")
		}

		streamResponseHead(w, 0, size, size, `"v"`)

		_, _ = io.CopyN(w, &offsetStream{}, int64(PageSize))
		entered <- uint64(PageSize)

		select {
		case <-release:
		case <-r.Context().Done():
			return
		}

		_, _ = io.CopyN(w, &offsetStream{offset: int64(PageSize)}, size-int64(PageSize))
	}))
	c := testClient(t, path, 3)
	c.config.PageWindow = 3

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

	for range 1 {
		select {
		case first := <-entered:
			seen[first] = true
		case <-time.After(3 * time.Second):
			t.Fatal("pages did not open concurrently")
		}
	}

	if len(seen) != 1 || !seen[uint64(PageSize)] {
		t.Fatal("unexpected page window", seen)
	}

	if calls.Load() != 1 || len(c.slots) != 1 {
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

	if calls.Load() != 1 || len(c.slots) != 0 {
		t.Fatal("wrong requests or retained permits")
	}
}

func TestBootstrapPrefetchUsesOnlySpareAdmission(t *testing.T) {
	entered := make(chan struct{}, 2)
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamResponseHead(w, 0, 4*int64(PageSize), 4*int64(PageSize), `"v"`)
		w.(http.Flusher).Flush()

		entered <- struct{}{}

		<-r.Context().Done()
	}))
	c := testClient(t, path, 3)
	c.config.PageWindow = 3
	c.config.PrefetchBootstrap = true

	v, err := c.Get(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(v)

	for range 1 {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("prefetch waited for bootstrap body")
		}
	}

	if len(c.slots) != 1 || c.Stats().Dials != 1 {
		t.Fatal("prefetch admission incorrect")
	}

	closeBody(v)

	if len(c.slots) != 0 {
		t.Fatal("prefetch retained admission")
	}
}

func TestValueWindowCloseCancelsEveryWorker(t *testing.T) {
	entered := make(chan struct{}, 3)
	stopped := make(chan struct{}, 3)
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamResponseHead(w, 0, 8*int64(PageSize), 8*int64(PageSize), `"v"`)
		_, _ = io.CopyN(w, repeatedByte('x'), int64(PageSize))

		entered <- struct{}{}

		<-r.Context().Done()

		stopped <- struct{}{}
	}))
	c := testClient(t, path, 3)
	c.config.PageWindow = 3

	v, err := c.Get(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := io.CopyN(io.Discard, v, int64(PageSize)); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() { _, err := io.Copy(io.Discard, v); done <- err }()

	for range 1 {
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

	for range 1 {
		select {
		case <-stopped:
		case <-time.After(3 * time.Second):
			t.Fatal("worker not canceled")
		}
	}

	if len(c.slots) != 0 {
		t.Fatal("Close retained permits")
	}
}

func TestValueWindowRefillsBeforeLaterPagesFinish(t *testing.T) {
	const size = 5 * int64(PageSize)

	entered := make(chan int64, 8)
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamResponseHead(w, 0, size, size, `"v"`)

		for page := range int64(4) {
			entered <- page * int64(PageSize)

			if _, err := io.CopyN(w, &offsetStream{offset: page * int64(PageSize)}, int64(PageSize)); err != nil {
				return
			}
		}

		entered <- 4 * int64(PageSize)

		<-r.Context().Done()
	}))
	c := testClient(t, path, 3)
	c.config.PageWindow = 3

	v, err := c.Get(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(v)

	if _, err := io.CopyN(io.Discard, v, 2*int64(PageSize)); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() { _, err := io.Copy(io.Discard, v); done <- err }()

	seen := make(map[int64]bool)
	for !seen[4*int64(PageSize)] {
		select {
		case offset := <-entered:
			seen[offset] = true
		case <-time.After(3 * time.Second):
			t.Fatal("window did not refill", seen)
		}
	}

	closeBody(v)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("reader did not stop")
	}

	if len(c.slots) != 0 {
		t.Fatal("retained permits")
	}
}
