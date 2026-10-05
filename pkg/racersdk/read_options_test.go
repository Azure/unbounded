// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestReadOptionsExactRangesAndMetadata(t *testing.T) {
	const size = 3*int64(PageSize) + 13
	for _, options := range []ReadOptions{{}, {Offset: 7, Length: 19}, {Offset: ByteOffset(PageSize) - 5, Length: PageSize + 17}, {Offset: ByteOffset(size - 1)}, {Offset: ByteOffset(size)}, {Offset: 3, Length: 2, Pin: ETag{value: `"v"`}}} {
		t.Run(strconv.FormatUint(uint64(options.Offset), 10)+"/"+strconv.FormatUint(uint64(options.Length), 10), func(t *testing.T) {
			var heads, gets atomic.Int32

			path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Racer-Content-Type", "application/vnd.oci.image.manifest.v1+json")

				if r.Method == "HEAD" {
					heads.Add(1)

					if r.Header.Get("Range") != "" || r.Header.Get("If-Match") != options.Pin.String() {
						t.Error("HEAD envelope")
					}

					w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
					w.Header().Set("ETag", `"v"`)
					w.Header().Set("Racer-Expires-At", "0")

					return
				}

				gets.Add(1)

				length := int64(options.Length)
				if length == 0 {
					length = size - int64(options.Offset)
				}

				want := "bytes=" + strconv.FormatUint(uint64(options.Offset), 10) + "-"
				if options.Length != 0 {
					want += strconv.FormatInt(int64(options.Offset)+length-1, 10)
				}

				if options.Offset == 0 && options.Length == 0 {
					want = ""
				}

				if r.Method != "POST" || r.Header.Get("Range") != want || r.Header.Get("If-Match") != options.Pin.String() {
					t.Error("not an exact pinned range", r.Header)
				}

				if int64(options.Offset) == size {
					w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
					w.Header().Set("Content-Length", "0")
					w.WriteHeader(416)

					return
				}

				streamResponseHead(w, int64(options.Offset), length, size, `"v"`)
				_, _ = io.CopyN(w, &offsetStream{offset: int64(options.Offset)}, length)
			}))
			c := testClient(t, path, 1)

			v, err := c.Get(context.Background(), Request{}, options)
			if int64(options.Offset) == size {
				assertKind(t, err, ErrorUnsatisfiableRange)
				return
			}

			if err != nil {
				t.Fatal(err)
			}

			defer closeBody(v)

			want := int64(options.Length)
			if want == 0 {
				want = size - int64(options.Offset)
			}

			n, err := v.WriteTo(&offsetSink{offset: int64(options.Offset)})
			if err != nil || n != want {
				t.Fatal(n, err)
			}

			wantHeads, wantGets := int32(0), int32(1)

			if v.Metadata().Size != ByteLength(size) || v.Metadata().ContentType != "application/vnd.oci.image.manifest.v1+json" || heads.Load() != wantHeads || gets.Load() != wantGets {
				t.Fatal("metadata or exchange count")
			}
		})
	}
}

func TestClientQueueBoundsAndMetadataReservation(t *testing.T) {
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", "1")
			w.Header().Set("ETag", `"v"`)
			w.Header().Set("Racer-Expires-At", "0")

			return
		}

		streamResponse(w, 0, 1, 1, `"v"`)
	}))

	c, err := newClient(ClientConfig{Cache: CacheName{value: "test"}, MaxConnections: 1, MetadataConnections: 1, MaxQueuedRequests: 1, QueueTimeout: 100 * time.Millisecond}, path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(c)

	v, err := c.Get(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(v)

	for _, cancelWait := range []bool{true, false} {
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)

		go func() { _, err := c.Get(ctx, Request{}); result <- err }()

		until := time.Now().Add(time.Second)
		for len(c.queued) != 1 && time.Now().Before(until) {
			time.Sleep(time.Millisecond)
		}

		if len(c.queued) != 1 {
			t.Fatal("waiter not admitted")
		}

		c.mu.Lock()
		active := len(c.active)
		c.mu.Unlock()

		if active != 1 {
			t.Fatal("queued request allocated active state")
		}

		_, err := c.Get(context.Background(), Request{})
		assertKind(t, err, ErrorUnavailable)

		m, err := c.Stat(context.Background(), Request{})
		if err != nil || m.Size != 1 {
			t.Fatal("bulk queue starved reserved metadata", err)
		}

		if cancelWait {
			cancel()
		}

		err = <-result
		if cancelWait {
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		} else {
			assertKind(t, err, ErrorDeadline)
		}

		cancel()

		if len(c.queued) != 0 {
			t.Fatal("queue slot leaked")
		}
	}

	closeBody(v)

	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			v, err := c.Get(context.Background(), Request{})
			if err != nil {
				var typed *Error
				if !errors.As(err, &typed) || typed.Kind() != ErrorUnavailable {
					t.Error(err)
				}

				return
			}
			defer closeBody(v)

			if n, err := io.Copy(io.Discard, v); err != nil || n != 1 {
				t.Error(n, err)
			}
		})
	}

	wg.Wait()

	if len(c.slots) != 0 || len(c.metadataPool.slots) != 0 || len(c.queued) != 0 {
		t.Fatal("concurrent calls leaked admission")
	}
}

func TestReadOptionsValidationAndPinnedHead(t *testing.T) {
	var calls atomic.Int32

	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)

		if r.Method != "POST" {
			t.Error("range must use one subscription")
		}

		if r.Header.Get("If-Match") == `"old"` {
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(412)

			return
		}

		w.Header().Set("Content-Length", "0")
		w.Header().Set("Content-Range", "bytes */3")
		w.WriteHeader(416)
	}))

	c := testClient(t, path, 1)
	for _, o := range []ReadOptions{{Offset: math.MaxUint64}, {Length: math.MaxUint64}, {Offset: math.MaxInt64, Length: 1}, {Pin: ETag{value: "bad"}}} {
		_, err := c.Get(context.Background(), Request{}, o)
		assertKind(t, err, ErrorInvalidArgument)
	}

	_, err := c.Get(context.Background(), Request{}, ReadOptions{}, ReadOptions{})
	assertKind(t, err, ErrorInvalidArgument)

	if calls.Load() != 0 {
		t.Fatal("invalid arguments performed I/O")
	}

	for _, o := range []ReadOptions{{Offset: 4}, {Offset: 2, Length: 2}} {
		_, err := c.Get(context.Background(), Request{}, o)
		assertKind(t, err, ErrorUnsatisfiableRange)
	}

	_, err = c.Get(context.Background(), Request{}, ReadOptions{Pin: ETag{value: `"old"`}})
	assertKind(t, err, ErrorVersionUnavailable)
}

func TestContentTypeWireAndOrigin(t *testing.T) {
	for _, s := range []string{"application/vnd.oci.image.manifest.v1+json", "text/plain; charset=utf-8", `text/plain; x="a;b\"c"`, "a/" + strings.Repeat("b", 254)} {
		if err := validateContentType(s); err != nil {
			t.Fatal(s, err)
		}
	}

	for _, s := range []string{"text", "text/", "/plain", " text/plain", "text/plain ", "text/plain, text/html", "text/plain;", "text/plain;x", "text/plain;x=", `text/plain;x="`, "text/plain;x=a;X=a", "text/plain\r\nx:y", "text/\tplain", "text/pläin", "a/" + strings.Repeat("b", 255)} {
		if validateContentType(s) == nil {
			t.Fatal("accepted invalid MIME", s)
		}
	}

	m := originMeta(3)
	m.ExpiresAt = m.ExpiresAt.UTC()
	m.ContentType = "text/plain; charset=utf-8"

	c, cleanup, err := newFakeClient(t, func(_ context.Context, r OriginRequest) (Metadata, io.ReadCloser, error) {
		if r.Operation() == OperationHead {
			return m, nil, nil
		}

		return m, io.NopCloser(strings.NewReader("abc")), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	got, err := c.Stat(context.Background(), Request{})
	if err != nil || got != m {
		t.Fatal("origin MIME did not survive HEAD", got, err)
	}

	v, err := c.Get(context.Background(), Request{}, ReadOptions{Offset: 1, Length: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(v)

	b, err := io.ReadAll(v)
	if err != nil || string(b) != "b" || v.Metadata() != m {
		t.Fatal("fake range or MIME", string(b), err)
	}

	r := OriginRequest{operation: OperationBootstrap, byteRange: bootstrapRange()}
	for _, field := range []string{"Racer-Content-Type: \r\n", "Racer-Content-Type: text\r\n", "Racer-Content-Type: text/plain\r\nRacer-Content-Type: text/plain\r\n"} {
		_, err := parseResponseHead(rawResponse(200, "Content-Length: 0\r\nContent-Type: application/octet-stream\r\nETag: \"v\"\r\nRacer-Expires-At: 0\r\n"+field), r, nil)
		assertKind(t, err, ErrorProtocol)
	}
}

func TestClientIdleAndCloseConnectionPolicy(t *testing.T) {
	for _, policy := range []string{"reuse", "idle", "close"} {
		t.Run(policy, func(t *testing.T) {
			path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if policy == "close" {
					w.Header().Set("Connection", "close")
				}

				streamResponse(w, 0, 1, 1, `"v"`)
			}))

			config := ClientConfig{Cache: CacheName{value: "test"}, MaxConnections: 1}
			if policy == "idle" {
				config.IdleConnTimeout = 20 * time.Millisecond
			}

			c, err := newClient(config, path)
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(c)

			var dials atomic.Int32

			poolConfig := c.bulk.Config()
			dial := poolConfig.Dial

			poolConfig.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
				dials.Add(1)
				return dial(ctx, network, address)
			}
			c.configurePools(poolConfig)

			for i := range 2 {
				v, err := c.Get(context.Background(), Request{})
				if err != nil {
					t.Fatal(err)
				}

				if n, err := v.WriteTo(io.Discard); n != 1 || err != nil {
					t.Fatal(n, err)
				}

				closeBody(v)

				if policy == "idle" && i == 0 {
					deadline := time.Now().Add(time.Second)

					for {
						idle := c.bulk.Stats().IdleConnections

						if idle == 0 {
							break
						}

						if time.Now().After(deadline) {
							t.Fatal("idle connection retained")
						}

						time.Sleep(time.Millisecond)
					}
				}
			}

			want := int32(2)

			if dials.Load() != want {
				t.Fatal("connection policy", dials.Load(), want)
			}
		})
	}
}

func TestClientReservedPoolFiniteAdmissionAndCancellation(t *testing.T) {
	entered := make(chan struct{}, 2)
	path := clientPeer(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}

		<-r.Context().Done()
	}))

	c, err := newClient(ClientConfig{Cache: CacheName{value: "test"}, MetadataConnections: 2, MetadataQueuedRequests: 3}, path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(c)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	results := make(chan error, 5)

	for range 2 {
		go func() { _, err := c.Stat(ctx, Request{}); results <- err }()
	}

	for range 2 {
		<-entered
	}

	for range 3 {
		go func() { _, err := c.Stat(ctx, Request{}); results <- err }()
	}

	deadline := time.Now().Add(time.Second)
	for len(c.metadataPool.queued) != 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	if len(c.metadataPool.queued) != 3 || len(c.metadataPool.slots) != 2 {
		t.Fatal("metadata admission not bounded")
	}

	_, err = c.Stat(context.Background(), Request{})
	assertKind(t, err, ErrorUnavailable)
	cancel()

	for range 5 {
		select {
		case err := <-results:
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("Stat cancellation blocked")
		}
	}

	if len(c.metadataPool.slots) != 0 || len(c.metadataPool.queued) != 0 {
		t.Fatal("Stat retained admission")
	}
}
