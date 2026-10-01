// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIndependentQueuesAndSmallObjectAdmission(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})

	var heads atomic.Int32

	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			if heads.Add(1) == 1 {
				close(entered)

				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}

			w.Header().Set("Content-Length", "1")
			w.Header().Set("ETag", `"v"`)
			w.Header().Set("Racer-Expires-At", "0")

			return
		}

		streamResponse(w, 0, 1, 1, `"v"`)
	}))

	c, err := newClient(ClientConfig{Cache: CacheName{value: "test"}, MaxConnections: 1, MaxQueuedRequests: 1, MetadataConnections: 1, MetadataQueuedRequests: 1, SmallObjectConnections: 1, SmallObjectQueuedRequests: 1}, path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(c)

	v, err := c.Get(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(v)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bulkDone := make(chan error, 1)

	go func() { _, err := c.Get(ctx, Request{}); bulkDone <- err }()

	waitDepth := func(want int) {
		t.Helper()

		until := time.Now().Add(time.Second)
		for c.Stats().QueueDepth != want && time.Now().Before(until) {
			time.Sleep(time.Millisecond)
		}

		if c.Stats().QueueDepth != want {
			t.Fatal("queue depth", c.Stats())
		}
	}
	waitDepth(1)

	headDone := make(chan error, 2)

	go func() { _, err := c.Stat(ctx, Request{}); headDone <- err }()

	<-entered

	go func() { _, err := c.Stat(ctx, Request{}); headDone <- err }()

	waitDepth(2)

	small, err := c.Get(ctx, Request{}, ReadOptions{SmallObject: true})
	if err != nil {
		t.Fatal("bulk/HEAD saturation blocked small GET", err)
	}
	defer closeBody(small)

	smallDone := make(chan error, 1)

	go func() { _, err := c.Get(ctx, Request{}, ReadOptions{SmallObject: true}); smallDone <- err }()

	waitDepth(3)

	s := c.Stats()
	if s.BulkQueueDepth != 1 || s.MetadataQueueDepth != 1 || s.SmallObjectQueueDepth != 1 || s.ActiveSmallObjects != 1 || s.Connections != 3 {
		t.Fatal(s)
	}

	_, err = c.Get(ctx, Request{}, ReadOptions{SmallObject: true})
	assertKind(t, err, ErrorUnavailable)
	close(release)

	for range 2 {
		if err := <-headDone; err != nil {
			t.Fatal("reserved metadata queue rejected HEAD", err)
		}
	}

	cancel()

	for _, done := range []chan error{bulkDone, smallDone} {
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}

	closeBody(small)
	closeBody(v)

	if s := c.Stats(); s.QueueDepth != 0 || s.ActiveBulk != 0 || s.ActiveMetadata != 0 || s.ActiveSmallObjects != 0 {
		t.Fatal(s)
	}
}

func TestSmallObjectSizeAndBootstrap(t *testing.T) {
	for _, size := range []ByteLength{0, 3, PageSize, PageSize + 1} {
		t.Run(strconv.FormatUint(uint64(size), 10), func(t *testing.T) {
			var calls atomic.Int32

			path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)

				if r.Method != "POST" || r.Header.Get("If-Match") != "" || r.Header.Get("Range") != "" {
					t.Error("SmallObject did not use a single unpinned subscription")
				}

				streamResponse(w, 0, int64(min(size, PageSize)), int64(size), `"v"`)
			}))
			c := testClient(t, path, 1)

			v, err := c.Get(context.Background(), Request{}, ReadOptions{SmallObject: true})
			if size > PageSize {
				assertKind(t, err, ErrorInvalidArgument)

				if v != nil || c.Stats().BytesRead != 0 || c.Stats().ActiveSmallObjects != 0 {
					t.Fatal("oversized body exposed", c.Stats())
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}

				defer closeBody(v)

				if n, err := v.WriteTo(io.Discard); err != nil || n != int64(size) {
					t.Fatal(n, err)
				}
			}

			if calls.Load() != 1 {
				t.Fatal("unexpected remainder or HEAD", calls.Load())
			}
		})
	}

	c := testClient(t, "unused", 1)
	m := originMeta(PageSize + 1)
	_, err := c.Get(context.Background(), Request{}, ReadOptions{SmallObject: true, Length: 1, Metadata: &m})
	assertKind(t, err, ErrorInvalidArgument)

	if c.Stats().Dials != 0 {
		t.Fatal("oversized snapshot performed I/O")
	}
}

func TestOriginHeadReservedFromFullBodyAdmission(t *testing.T) {
	path, cancel, done := startOrigin(t, OriginConfig{MaxConcurrentRequests: 2, MaxConcurrentHeadRequests: 1}, func(_ context.Context, r OriginRequest) (Metadata, io.ReadCloser, error) {
		if r.Operation() == OperationHead {
			return originMeta(1), nil, nil
		}

		return originMeta(1), &blockedBody{done: make(chan struct{}), first: true}, nil
	})

	defer func() { cancel(); <-done }()

	c := originClient(t, path, 2)
	for range 2 {
		v, err := c.Get(context.Background(), Request{})
		if err != nil {
			t.Fatal(err)
		}
		defer closeBody(v)
	}

	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()

	m, err := c.Stat(ctx, Request{})
	if err != nil || m.Size != 1 {
		t.Fatal("GET bodies starved origin HEAD", err)
	}
}

func TestReservedAdmissionConfigValidation(t *testing.T) {
	for _, config := range []ClientConfig{{SmallObjectConnections: -1}, {SmallObjectQueuedRequests: -1}, {MetadataQueuedRequests: -1}} {
		config.Cache = CacheName{value: "test"}
		_, err := NewClient(config)
		assertKind(t, err, ErrorInvalidArgument)
	}

	_, err := (OriginConfig{Cache: CacheName{value: "test"}, MaxConcurrentHeadRequests: -1}).defaults()
	assertKind(t, err, ErrorInvalidArgument)
}

func TestSmallObjectDefaultQueueAcceptsSynchronizedBurst(t *testing.T) {
	release := make(chan struct{})
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}

		streamResponse(w, 0, 1, 1, `"v"`)
	}))

	c := testClient(t, path, 1)
	if cap(c.smallPool.slots) != 4 || cap(c.smallPool.queued) != 128 {
		t.Fatal("unexpected small-object defaults", c.config)
	}

	start := make(chan struct{})
	results := make(chan error, 64)

	for range 64 {
		go func() {
			<-start

			v, err := c.Get(context.Background(), Request{}, ReadOptions{SmallObject: true})
			if err == nil {
				_, err = v.WriteTo(io.Discard)
				closeBody(v)
			}

			results <- err
		}()
	}

	close(start)

	deadline := time.Now().Add(3 * time.Second)
	for c.Stats().SmallObjectQueueDepth != 60 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	s := c.Stats()

	close(release)

	if s.ActiveSmallObjects != 4 || s.SmallObjectQueueDepth != 60 || s.QueueRejections != 0 {
		t.Error("burst was not bounded and queued", s)
	}

	for range 64 {
		select {
		case err := <-results:
			if err != nil {
				t.Error("burst request failed", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("burst did not complete")
		}
	}

	if s := c.Stats(); s.ActiveSmallObjects != 0 || s.SmallObjectQueueDepth != 0 || s.QueueRejections != 0 || s.BytesRead != 64 {
		t.Fatal("burst leaked admission or lost bytes", s)
	}
}

func TestSmallObjectPinnedRangeAndHeadSizeValidation(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		for _, snapshot := range []bool{false, true} {
			t.Run(strconv.FormatBool(oversized)+"/snapshot="+strconv.FormatBool(snapshot), func(t *testing.T) {
				m := originMeta(3)
				if oversized {
					m.Size = PageSize + 1
				}

				var heads, gets atomic.Int32

				path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == "HEAD" {
						heads.Add(1)
						w.Header().Set("Content-Length", strconv.FormatUint(uint64(m.Size), 10))
						w.Header().Set("ETag", `"v"`)
						w.Header().Set("Racer-Expires-At", "0")

						return
					}

					gets.Add(1)

					pin := ""
					if snapshot {
						pin = `"v"`
					}

					if r.Header.Get("If-Match") != pin || r.Header.Get("Range") != "bytes=1-1" {
						t.Error("small range lost pin or bounds")
					}

					streamResponse(w, 1, 1, int64(m.Size), `"v"`)
				}))
				c := testClient(t, path, 1)

				o := ReadOptions{SmallObject: true, Offset: 1, Length: 1}
				if snapshot {
					o.Metadata = &m
				}

				v, err := c.Get(context.Background(), Request{}, o)
				if oversized {
					assertKind(t, err, ErrorInvalidArgument)

					wantGets := int32(1)
					if snapshot {
						wantGets = 0
					}

					if v != nil || gets.Load() != wantGets {
						t.Fatal("oversized pinned object fetched")
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}

					if c.Stats().ActiveSmallObjects != 1 || c.Stats().ActiveBulk != 0 {
						t.Fatal(c.Stats())
					}

					if n, err := v.WriteTo(io.Discard); n != 1 || err != nil {
						t.Fatal(n, err)
					}

					closeBody(v)

					if gets.Load() != 1 {
						t.Fatal("extra small GET")
					}
				}

				wantHeads := int32(0)

				if heads.Load() != wantHeads {
					t.Fatal("wrong HEAD count", heads.Load())
				}

				closeBody(c)

				if c.Stats().Connections != 0 {
					t.Fatal("small pool connections retained")
				}
			})
		}
	}
}

func TestContentTypeExactCompatibilityAndRawWhitespace(t *testing.T) {
	for _, initial := range []string{"", "text/plain"} {
		for _, current := range []string{"", "text/plain", "application/json"} {
			m := originMeta(3)
			m.ContentType = initial
			path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if current != "" {
					w.Header().Set("Racer-Content-Type", current)
				}

				streamResponse(w, 0, 3, 3, `"v"`)
			}))
			c := testClient(t, path, 1)

			v, err := c.Get(context.Background(), Request{}, ReadOptions{Metadata: &m})
			if initial != current {
				assertKind(t, err, ErrorProtocol)
				continue
			}

			if err != nil {
				t.Fatal(initial, current, err)
			}

			if n, err := v.WriteTo(io.Discard); err != nil || n != 3 {
				t.Fatal(n, err)
			}

			closeBody(v)

			if v.Metadata() != m {
				t.Fatal("initial metadata changed")
			}
		}
	}

	r := OriginRequest{operation: OperationBootstrap, byteRange: bootstrapRange()}

	for _, value := range []string{"text/plain", "  text/plain", "\ttext/plain", " text/plain ", " text/plain\t", " text/plain;\tcharset=utf-8"} {
		head := rawResponse(200, "Content-Length: 0\r\nContent-Type: application/octet-stream\r\nETag: \"v\"\r\nRacer-Expires-At: 0\r\nRacer-Content-Type:"+value+"\r\n")
		if _, err := parseResponseHead(head, r, nil); err == nil {
			t.Fatal("normalized invalid raw MIME", value)
		}
	}

	for _, value := range []string{" text/plain", " text/plain; charset=utf-8", " text/plain; x=\"a b\""} {
		head := rawResponse(200, "Content-Length: 0\r\nContent-Type: application/octet-stream\r\nETag: \"v\"\r\nRacer-Expires-At: 0\r\nRacer-Content-Type:"+value+"\r\n")

		result, err := parseResponseHead(head, r, nil)
		if err != nil || result.metadata.ContentType != strings.TrimPrefix(value, " ") {
			t.Fatal(value, err)
		}
	}
}
