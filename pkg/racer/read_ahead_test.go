// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Synthetic object bytes avoid allocating whole 64 MiB pages in boundary tests.
func readAheadPayload(off int64, size int) []byte {
	p := make([]byte, size)
	for i := range p {
		p[i] = byte((off + int64(i)) % 251)
	}

	return p
}

type readAheadFixture struct {
	gets   atomic.Int32
	ranges sync.Map
	// Installed before requests; returning true overrides the successful body.
	intercept func(http.ResponseWriter, *http.Request, int64, int64) bool
}

func (f *readAheadFixture) open(t *testing.T, size int64) *Object {
	t.Helper()
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("ETag", checksumTag(nil))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))

		if req.Method == http.MethodHead {
			return
		}

		f.gets.Add(1)

		if req.Header.Get("If-Match") != checksumTag(nil) {
			t.Error("missing pinned If-Match")
		}

		start, length, status := objectRange(req.Header, Metadata{Size: size})

		end := start + length - 1
		if status != http.StatusPartialContent || start/PageSize != end/PageSize {
			t.Errorf("invalid range: %s", req.Header.Get("Range"))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)

			return
		}

		f.ranges.Store(req.Header.Get("Range"), true)
		w.Header().Set("Content-Range", contentRange(start, end, size))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))

		if f.intercept != nil && f.intercept(w, req, start, end) {
			return
		}

		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(readAheadPayload(start, int(end-start+1)))
	}), ClientOptions{})

	o, err := c.Open(t.Context(), "/object")
	if err != nil {
		t.Fatal(err)
	}

	return o
}

func newTestReadAhead(t *testing.T, o *Object, limit int) *ReadAhead {
	t.Helper()

	r, err := o.ReadAhead(limit)
	if err != nil {
		t.Fatal(err)
	}

	return r
}

func checkReadAhead(t *testing.T, r *ReadAhead, off int64, size, want int, wantErr error) {
	t.Helper()

	p := make([]byte, size)

	n, err := r.ReadAt(t.Context(), p, off)
	if n != want || !errors.Is(err, wantErr) || !bytes.Equal(p[:n], readAheadPayload(off, n)) {
		t.Fatalf("read at %d size %d: n=%d err=%v, want %d %v", off, size, n, err, want, wantErr)
	}

	if cap(r.buf) > r.maxBytes || len(r.buf) > r.maxBytes {
		t.Fatalf("storage len=%d cap=%d exceeds %d", len(r.buf), cap(r.buf), r.maxBytes)
	}
}

func TestReadAheadGETCountsAndStorage(t *testing.T) {
	f := new(readAheadFixture)
	o := f.open(t, 4096)

	r := newTestReadAhead(t, o, 64)
	if cap(r.buf) != 0 || f.gets.Load() != 0 {
		t.Fatal("constructor allocated payload or issued GET")
	}

	for _, off := range []int64{100, 108, 132, 101, 156} {
		checkReadAhead(t, r, off, 8, 8, nil)
	}

	if f.gets.Load() != 1 {
		t.Fatalf("nearby reads: GET=%d, want 1", f.gets.Load())
	}

	if _, ok := f.ranges.Load("bytes=100-163"); !ok {
		t.Fatal("missing bounded forward window")
	}

	storage := &r.buf[:cap(r.buf)][0]
	for i, off := range []int64{3000, 12, 2048, 100, 900} {
		checkReadAhead(t, r, off, 8, 8, nil)

		if f.gets.Load() != int32(i+2) || &r.buf[:cap(r.buf)][0] != storage {
			t.Fatal("random miss did not replace the sole bounded window")
		}
	}

	checkReadAhead(t, r, 1000, 128, 128, nil)

	if f.gets.Load() != 7 {
		t.Fatalf("large exact read GET=%d", f.gets.Load())
	}

	if _, ok := f.ranges.Load("bytes=1000-1127"); !ok {
		t.Fatal("large read speculated")
	}

	checkReadAhead(t, r, 908, 8, 8, nil)

	if f.gets.Load() != 7 {
		t.Fatal("large bypass discarded valid window")
	}

	for range 2 {
		if n, err := o.ReadAt(t.Context(), make([]byte, 3), 50); n != 3 || err != nil {
			t.Fatal(n, err)
		}
	}

	if f.gets.Load() != 9 {
		t.Fatal("default Object.ReadAt cached bytes")
	}

	if _, ok := f.ranges.Load("bytes=50-52"); !ok {
		t.Fatal("default Object.ReadAt speculated")
	}
}

func TestReadAheadBoundaries(t *testing.T) {
	f := new(readAheadFixture)
	r := newTestReadAhead(t, f.open(t, PageSize+35), 32)
	checkReadAhead(t, r, PageSize-5, 10, 10, nil)
	checkReadAhead(t, r, PageSize+8, 10, 10, nil)

	if f.gets.Load() != 2 {
		t.Fatalf("cross-page window GET=%d, want 2", f.gets.Load())
	}

	checkReadAhead(t, r, PageSize+30, 10, 5, io.EOF)
	checkReadAhead(t, r, PageSize+31, 4, 4, nil)
	checkReadAhead(t, r, PageSize+32, 10, 3, io.EOF)
	checkReadAhead(t, r, PageSize+35, 1, 0, io.EOF)
	checkReadAhead(t, r, math.MaxInt64, 1, 0, io.EOF)
	checkReadAhead(t, r, math.MaxInt64, 0, 0, nil)

	if n, err := r.ReadAt(t.Context(), nil, -1); n != 0 || err == nil {
		t.Fatal("negative offset accepted", n, err)
	}

	if f.gets.Load() != 3 {
		t.Fatalf("EOF and bounds GET=%d, want 3", f.gets.Load())
	}

	for _, size := range []int64{0, 3} {
		f := new(readAheadFixture)

		o := f.open(t, size)
		for _, limit := range []int{-1, 0} {
			if _, err := o.ReadAhead(limit); err == nil {
				t.Fatal("invalid limit accepted")
			}
		}

		r := newTestReadAhead(t, o, 64)
		checkReadAhead(t, r, 0, 0, 0, nil)
		checkReadAhead(t, r, 0, 5, int(size), io.EOF)

		if cap(r.buf) != int(size) || f.gets.Load() != int32(min(size, 1)) {
			t.Fatal("small/empty object storage or GET count", cap(r.buf), f.gets.Load())
		}
	}
}

func TestReadAheadFailedFillRecovery(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fail  func(http.ResponseWriter)
		want  error
		count int
	}{
		{"short", func(w http.ResponseWriter) { w.WriteHeader(206); _, _ = w.Write(readAheadPayload(100, 2)) }, io.ErrUnexpectedEOF, 2},
		{"speculative-short", func(w http.ResponseWriter) { w.WriteHeader(206); _, _ = w.Write(readAheadPayload(100, 8)) }, io.ErrUnexpectedEOF, 4},
		{"version", func(w http.ResponseWriter) { w.Header().Set("ETag", checksumTag([]byte("new"))); w.WriteHeader(206) }, ErrVersionChanged, 0},
		{"precondition", func(w http.ResponseWriter) { w.WriteHeader(412) }, ErrVersionChanged, 0},
		{"range", func(w http.ResponseWriter) { w.Header().Set("Content-Range", "bytes 100-131/999"); w.WriteHeader(206) }, ErrProtocol, 0},
		{"encoding", func(w http.ResponseWriter) { w.Header().Set("Content-Encoding", "gzip"); w.WriteHeader(206) }, ErrProtocol, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var fail atomic.Bool

			f := &readAheadFixture{intercept: func(w http.ResponseWriter, _ *http.Request, _, _ int64) bool {
				if fail.Load() {
					tc.fail(w)
					return true
				}

				return false
			}}
			r := newTestReadAhead(t, f.open(t, 1000), 32)
			checkReadAhead(t, r, 0, 4, 4, nil)
			fail.Store(true)
			checkReadAhead(t, r, 100, 4, tc.count, tc.want)

			if len(r.buf) != 0 {
				t.Fatal("failed data cached")
			}

			fail.Store(false)
			checkReadAhead(t, r, 100, 4, 4, nil)
			checkReadAhead(t, r, 104, 4, 4, nil)
			checkReadAhead(t, r, 0, 4, 4, nil)

			if f.gets.Load() != 4 {
				t.Fatal("failed fill was reused or old window was not invalidated", f.gets.Load())
			}
		})
	}
}

func TestReadAheadCancellationAndConcurrentReads(t *testing.T) {
	started := make(chan struct{})

	var block atomic.Bool
	block.Store(true)

	f := &readAheadFixture{intercept: func(_ http.ResponseWriter, req *http.Request, _, _ int64) bool {
		if block.Swap(false) {
			close(started)
			<-req.Context().Done()

			return true
		}

		return false
	}}
	r := newTestReadAhead(t, f.open(t, 4096), 64)

	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)

	cause := errors.New("read canceled")
	done := make(chan error, 1)

	go func() { _, err := r.ReadAt(ctx, make([]byte, 4), 0); done <- err }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("GET did not start")
	}

	waitCtx, waitCancel := context.WithTimeoutCause(t.Context(), 20*time.Millisecond, cause)
	defer waitCancel()

	if n, err := r.ReadAt(waitCtx, make([]byte, 4), 0); n != 0 || !errors.Is(err, cause) {
		t.Fatal("waiting cancellation lost", n, err)
	}

	if f.gets.Load() != 1 {
		t.Fatal("waiter issued GET")
	}

	cancel(cause)

	select {
	case err := <-done:
		if !errors.Is(err, cause) {
			t.Fatal("in-flight cancellation lost", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight read did not cancel")
	}

	if len(r.buf) != 0 {
		t.Fatal("canceled fill cached")
	}

	checkReadAhead(t, r, 0, 4, 4, nil)

	if n, err := r.ReadAt(ctx, make([]byte, 4), 0); n != 0 || !errors.Is(err, cause) {
		t.Fatal("canceled cache hit succeeded", n, err)
	}

	if n, err := r.ReadAt(ctx, nil, 0); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatal("empty canceled read", n, err)
	}

	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			for _, off := range []int64{int64(i), int64(i*100 + 100), int64(i)} {
				p := make([]byte, 8)

				n, err := r.ReadAt(t.Context(), p, off)
				if n != len(p) || err != nil || !bytes.Equal(p, readAheadPayload(off, len(p))) {
					t.Errorf("concurrent read at %d: %d %v", off, n, err)
				}
			}
		})
	}

	wg.Wait()

	if cap(r.buf) != 64 {
		t.Fatal("concurrent reads exceeded storage bound", cap(r.buf))
	}
}

func TestReadAheadCrossPageFailurePrefix(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)

	f := &readAheadFixture{intercept: func(w http.ResponseWriter, _ *http.Request, start, _ int64) bool {
		if fail.Load() && start < PageSize {
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(readAheadPayload(start, 2))

			return true
		}

		return false
	}}
	r := newTestReadAhead(t, f.open(t, PageSize+100), 32)
	checkReadAhead(t, r, PageSize-4, 8, 2, io.ErrUnexpectedEOF)

	if len(r.buf) != 0 {
		t.Fatal("partial cross-page window cached")
	}

	fail.Store(false)

	before := f.gets.Load()

	checkReadAhead(t, r, PageSize-4, 8, 8, nil)
	checkReadAhead(t, r, PageSize+4, 8, 8, nil)

	if f.gets.Load()-before != 2 {
		t.Fatal("recovery did not refill both pages", f.gets.Load()-before)
	}
}

func TestReadAheadConcurrentNearbyGETCount(t *testing.T) {
	f := new(readAheadFixture)
	r := newTestReadAhead(t, f.open(t, 1024), 64)

	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			p := make([]byte, 8)
			if n, err := r.ReadAt(t.Context(), p, 100); n != 8 || err != nil || !bytes.Equal(p, readAheadPayload(100, 8)) {
				t.Errorf("concurrent read: %d %v", n, err)
			}
		})
	}

	wg.Wait()

	if f.gets.Load() != 1 {
		t.Fatal("concurrent fills not coalesced", f.gets.Load())
	}
}

func TestSinglePageSynchronousContextAndErrors(t *testing.T) {
	o := &Object{meta: Metadata{Size: 2 * PageSize}}
	failure := errors.New("page failed")

	for _, mode := range []string{"success", "failure", "cancel-success", "cancel-failure", "pre-canceled"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)

			cause := errors.New("custom cancellation")
			if mode == "pre-canceled" {
				cancel(cause)
			}

			calls := 0
			err := o.pages(ctx, PageSize+3, 5, func(got context.Context, start, end int64) error {
				calls++

				if got != ctx || start != PageSize+3 || end != PageSize+7 {
					t.Error("single page context or bounds changed")
				}

				if strings.HasPrefix(mode, "cancel-") {
					cancel(cause)
				}

				if strings.HasSuffix(mode, "failure") {
					return failure
				}

				return nil
			})

			switch mode {
			case "pre-canceled":
				if calls != 0 || err != context.Canceled {
					t.Fatal(calls, err)
				}
			case "cancel-success", "cancel-failure":
				if err != cause || calls != 1 {
					t.Fatal(calls, err)
				}
			case "failure":
				if !errors.Is(err, failure) || err.Error() != fmt.Sprintf("racer: page at %d: page failed", PageSize+3) {
					t.Fatal(err)
				}
			case "success":
				if err != nil || calls != 1 {
					t.Fatal(calls, err)
				}
			}
		})
	}
}

func TestSinglePagePartialReadAndDownload(t *testing.T) {
	f := &readAheadFixture{intercept: func(w http.ResponseWriter, _ *http.Request, start, _ int64) bool {
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(readAheadPayload(start, 2))

		return true
	}}
	o := f.open(t, 10)
	p := make([]byte, 5)

	n, err := o.ReadAt(t.Context(), p, 3)
	if n != 2 || !errors.Is(err, io.ErrUnexpectedEOF) || !bytes.Equal(p[:n], readAheadPayload(3, n)) || !strings.Contains(err.Error(), "page at 3:") {
		t.Fatal(n, err)
	}

	dst := make(sliceWriter, 10)

	written, err := o.Download(t.Context(), dst)
	if written != 2 || !errors.Is(err, io.ErrUnexpectedEOF) || !bytes.Equal(dst[:written], readAheadPayload(0, int(written))) || !strings.Contains(err.Error(), "page at 0:") {
		t.Fatal(written, err)
	}
}
