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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk/internal/connpool"
)

type connTestTimer struct {
	delay time.Duration
	fire  func()
}

func (*connTestTimer) Stop() bool { return true }

type connTestClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*connTestTimer
	ages   atomic.Int32
}

func installConnClock(c *Client, age time.Duration) *connTestClock {
	clock := &connTestClock{now: time.Now()}
	config := c.bulk.Config()
	config.Now = func() time.Time { clock.mu.Lock(); defer clock.mu.Unlock(); return clock.now }
	config.MaxAge = age
	config.Jitter = func(n int64) int64 { clock.ages.Add(1); return n - 1 }
	config.AfterFunc = func(d time.Duration, f func()) connpool.Timer {
		clock.mu.Lock()
		defer clock.mu.Unlock()

		timer := &connTestTimer{delay: d, fire: f}
		clock.timers = append(clock.timers, timer)

		return timer
	}
	c.configurePools(config)

	return clock
}

func (clock *connTestClock) advance(d time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()

	clock.now = clock.now.Add(d)
}

func TestConnectionAgeConfigAndJitter(t *testing.T) {
	c := testClient(t, "unused", 1)
	if c.config.MaxConnAge != 5*time.Minute || c.Stats().Dials != 0 {
		t.Fatal("default or eager dial", c.config.MaxConnAge, c.Stats())
	}

	_, err := NewClient(ClientConfig{Cache: CacheName{value: "test"}, MaxConnAge: -1})
	assertKind(t, err, ErrorInvalidArgument)

	for _, maxAge := range []time.Duration{1, 2, 3, 4, 5, 7, 8, 9, time.Minute, math.MaxInt64} {
		t.Run(strconv.FormatInt(int64(maxAge), 10), func(t *testing.T) {
			configured, err := NewClient(ClientConfig{Cache: CacheName{value: "test"}, MaxConnAge: maxAge})
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(configured)

			if configured.config.MaxConnAge != maxAge {
				t.Fatal("positive duration changed")
			}
		})
	}
}

func TestConnectionAgeBusyPools(t *testing.T) {
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", "1")
			w.Header().Set("ETag", `"v"`)
			w.Header().Set("Racer-Expires-At", "0")

			return
		}

		streamResponse(w, 0, 1, 1, `"v"`)
	}))
	c := testClient(t, path, 1)
	clock := installConnClock(c, 4*time.Second)

	for range 12 {
		for _, small := range []bool{false, true} {
			v, err := c.Get(context.Background(), Request{}, ReadOptions{SmallObject: small})
			if err != nil {
				t.Fatal(err)
			}

			if n, err := io.Copy(io.Discard, v); err != nil || n != 1 {
				t.Fatal(n, err)
			}

			closeBody(v)
		}

		if _, err := c.Stat(context.Background(), Request{}); err != nil {
			t.Fatal(err)
		}

		clock.advance(time.Second)
	}

	if s := c.Stats(); s.Dials != 27 || s.ConnectionRotations != 2 || s.ConnectionReuses != 9 || s.Retries != 0 || s.Connections != 1 || s.ActiveBulk != 0 || s.ActiveMetadata != 0 || s.ActiveSmallObjects != 0 {
		t.Fatal("busy reuse prevented rotation or changed admission", s)
	}
}

func TestConnectionAgeActiveResponseCompletes(t *testing.T) {
	release := make(chan struct{})

	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })

	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamResponseHead(w, 0, 3, 3, `"v"`)

		if err := http.NewResponseController(w).Flush(); err != nil {
			return
		}

		select {
		case <-release:
			_, _ = io.WriteString(w, "abc")
		case <-r.Context().Done():
		}
	}))
	c := testClient(t, path, 1)
	clock := installConnClock(c, 4*time.Second)

	v, err := c.Get(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}

	clock.advance(time.Hour)

	if s := c.Stats(); s.ConnectionRotations != 0 || s.Connections != 1 || s.ActiveBulk != 1 || len(clock.timers) != 0 {
		t.Fatal("age interrupted an active body", s)
	}

	releaseOnce.Do(func() { close(release) })

	b, err := io.ReadAll(v)
	if err != nil || string(b) != "abc" {
		t.Fatal(string(b), err)
	}

	closeBody(v)

	if s := c.Stats(); s.ConnectionRotations != 0 || s.Dials != 1 || s.Retries != 0 || s.Connections != 0 || s.ActiveBulk != 0 {
		t.Fatal(s)
	}
}

func TestConnectionAgeDialFailureAfterRotation(t *testing.T) {
	c := testClient(t, "unused", 1)
	config := c.bulk.Config()

	var fail atomic.Bool

	config.Dial = func(context.Context, string, string) (net.Conn, error) {
		if fail.Load() {
			return nil, io.EOF
		}

		conn, peer := net.Pipe()

		t.Cleanup(func() { closeBody(peer) })

		return conn, nil
	}
	c.configurePools(config)
	clock := installConnClock(c, 4*time.Second)

	conn, _, err := c.metadataPool.Get(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}

	c.metadataPool.Recycle(conn)
	clock.advance(4 * time.Second)
	fail.Store(true)

	_, err = c.Stat(context.Background(), Request{})
	if !errors.Is(err, io.EOF) {
		t.Fatal("dial failure lost", err)
	}

	if s := c.Stats(); s.ConnectionRotations != 1 || s.Dials != 2 || s.Retries != 0 || s.ConnectionReuses != 0 || s.Connections != 0 || s.ActiveBulk != 0 || clock.ages.Load() != 1 {
		t.Fatal("rotation became a retry or retained capacity", s)
	}
}

func TestConnectionAgeCancelRecycleRace(t *testing.T) {
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { streamResponse(w, 0, 1, 1, `"v"`) }))
	for range 50 {
		c := testClient(t, path, 1)
		clock := installConnClock(c, 4*time.Second)
		ctx, cancel := context.WithCancel(context.Background())

		v, err := c.Get(ctx, Request{})
		if err != nil {
			cancel()
			t.Fatal(err)
		}

		clock.advance(4 * time.Second)

		var wg sync.WaitGroup
		wg.Go(func() { _, _ = io.Copy(io.Discard, v) })
		wg.Go(cancel)
		wg.Go(func() { closeBody(v) })
		wg.Wait()
		closeBody(c)

		if s := c.Stats(); s.ConnectionRotations > 1 || s.Connections != 0 || s.ActiveBulk != 0 || s.Retries != 0 || s.Dials != 1 {
			t.Fatal("cancel/recycle race double counted or leaked", s)
		}
	}
}

func TestConnectionAgeBootstrapContinuation(t *testing.T) {
	const size = int64(PageSize) + 37

	var calls atomic.Int32

	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)

		if r.Method != "POST" || r.URL.Path != "/v2/objects/"+(Key{7}).String() || r.Header.Get("Authorization") != "secret\xff" || r.Header.Get("Racer-Metadata") != "opaque  bytes" {
			t.Error("rotation changed request envelope")
		}

		first, length := int64(0), size

		if r.Header.Get("If-Match") != "" || r.Header.Get("Range") != "" {
			t.Error("unexpected pin or range")
		}

		streamResponseHead(w, first, length, size, `"v"`)
		_, _ = io.CopyN(w, &offsetStream{offset: first}, length)
	}))
	c := testClient(t, path, 1)
	clock := installConnClock(c, 4*time.Second)
	request := Request{Key: Key{7}, Context: FetchContext{authorization: Authorization{value: "secret\xff"}, metadata: AdapterMetadata{value: "opaque  bytes"}}}

	v, err := c.Get(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}

	snapshot := v.Metadata()

	sink := &offsetSink{}
	if n, err := io.CopyN(sink, v, int64(PageSize)); n != int64(PageSize) || err != nil {
		t.Fatal(n, err)
	}

	clock.advance(4 * time.Second)

	if c.Stats().Dials != 1 || c.Stats().ActiveBulk != 1 || calls.Load() != 1 {
		t.Fatal("eager continuation or lost admission")
	}

	if n, err := io.Copy(sink, v); n != 37 || err != nil {
		t.Fatal("continuation bytes changed", n, err)
	}

	closeBody(v)

	if s := c.Stats(); s.Dials != 1 || s.ConnectionRotations != 0 || s.Retries != 0 || s.BytesRead != uint64(size) || s.ActiveBulk != 0 || calls.Load() != 1 || v.Metadata() != snapshot {
		t.Fatal("rotation changed continuation contract", s)
	}
}

func TestConnectionAgeContinuationKeepsContext(t *testing.T) {
	entered := make(chan struct{})
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamResponseHead(w, 0, int64(PageSize)+1, int64(PageSize)+1, `"v"`)
		_, _ = io.CopyN(w, repeatedByte('x'), int64(PageSize))

		close(entered)
		<-r.Context().Done()
	}))
	c := testClient(t, path, 1)
	clock := installConnClock(c, 4*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	v, err := c.Get(ctx, Request{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := io.CopyN(io.Discard, v, int64(PageSize)); err != nil {
		t.Fatal(err)
	}

	clock.advance(4 * time.Second)

	done := make(chan error, 1)

	go func() { _, err := io.Copy(io.Discard, v); done <- err }()

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("rotated continuation not opened")
	}

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("continuation lost original context", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("rotated continuation ignored cancellation")
	}

	closeBody(v)

	if s := c.Stats(); s.ConnectionRotations != 0 || s.Dials != 1 || s.Retries != 0 || s.Connections != 0 || s.ActiveBulk != 0 {
		t.Fatal(s)
	}
}
