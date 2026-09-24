// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type memoryStore struct {
	data                 []byte
	meta                 Metadata
	stats, opens, closes atomic.Int32
	err                  error
}

func (s *memoryStore) Stat(context.Context, string, []byte) (Metadata, error) {
	s.stats.Add(1)
	return s.meta, s.err
}

func (s *memoryStore) ResolveRange(ctx context.Context, target string, data []byte) (ResolvedRange, error) {
	m, err := s.Stat(ctx, target, data)

	return &testResolvedRange{meta: m, open: func(ctx context.Context, off, length int64) (io.ReadCloser, error) {
		return s.openRange(ctx, m.ETag, off, length)
	}}, err
}

func (s *memoryStore) openRange(_ context.Context, etag string, off, length int64) (io.ReadCloser, error) {
	s.opens.Add(1)

	if s.err != nil {
		return nil, s.err
	}

	if etag != s.meta.ETag {
		return nil, ErrVersionChanged
	}

	return &memorySource{Reader: io.NewSectionReader(bytes.NewReader(s.data), off, length), closes: &s.closes}, nil
}

type memorySource struct {
	io.Reader
	closes *atomic.Int32
}

func (s *memorySource) Close() error { s.closes.Add(1); return nil }

func durationPointer(d time.Duration) *time.Duration { return &d }

func TestOriginWireContract(t *testing.T) {
	data := payload(int(PageSize + 7))
	store := &memoryStore{data: data, meta: Metadata{Size: int64(len(data)), ETag: checksumTag(data), TTL: durationPointer(time.Minute)}}

	origin, err := NewRangeOrigin(store)
	if err != nil {
		t.Fatal(err)
	}

	s := httptest.NewServer(origin)
	defer s.Close()

	for _, tc := range []struct {
		method, path, byteRange, match string
		status                         int
		length                         int64
	}{
		{"HEAD", "/metadata", "", "", 200, int64(len(data))},
		{"GET", "/page", "bytes=0-67108863", `"v1"`, 206, PageSize},
		{"GET", "/page", "bytes=67108864-67108870", `W/"v1", "other", "v1"`, 206, 7},
		{"GET", "/page", "bytes=0-67108863", "*", 206, PageSize},
		{"GET", "/page", "bytes=0-67108863", `"old"`, 412, 0},
		{"GET", "/page", "bytes=0-67108863", `W/"v1"`, 412, 0},
		{"GET", "/page", "bad", `"old"`, 412, 0}, // preconditions before ranges
		{"GET", "/page", "bytes=0-67108863", `"v1",`, 206, PageSize},
		{"GET", "/page", "bytes=0-67108863", `"v1" junk`, 400, 0},
		{"GET", "/page", "bytes=1-4", `"v1"`, 206, 4},
		{"GET", "/page", "bytes=0-4", `"v1"`, 206, 5},
		{"GET", "/page", "bytes=0-67108870", `"v1"`, 206, int64(len(data))},
		{"GET", "/page", "bytes=0-1,4-5", `"v1"`, 200, int64(len(data))},
		{"GET", "/page", "bytes=9223372036854775808-9", `"v1"`, 200, int64(len(data))},
		{"GET", "/page", "", "", 200, int64(len(data))},
		{"GET", "/metadata", "", "", 200, int64(len(data))},
		{"HEAD", "/page", "", "", 200, int64(len(data))},
		{"HEAD", "/metadata?x=1", "", "", 200, int64(len(data))},
	} {
		t.Run(tc.method+tc.path+tc.byteRange+tc.match, func(t *testing.T) {
			opens := store.opens.Load()
			r, _ := http.NewRequest(tc.method, s.URL+tc.path, nil)
			r.Header.Set("X-Racer-Target", "/a%2Fb?x=2&x=1")

			if tc.byteRange != "" {
				r.Header.Set("Range", tc.byteRange)
			}

			if tc.match != "" {
				r.Header.Set("If-Match", strings.ReplaceAll(tc.match, `"v1"`, store.meta.ETag))
			}

			resp, err := s.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil || resp.StatusCode != tc.status || resp.ContentLength != tc.length {
				t.Fatalf("status=%d length=%d err=%v", resp.StatusCode, resp.ContentLength, err)
			}

			if tc.method == "HEAD" {
				if len(body) != 0 {
					t.Fatal("HEAD body")
				}
			} else if int64(len(body)) != tc.length {
				t.Fatal("body length")
			}

			if tc.status == 206 {
				start, length, _ := objectRange(r.Header, store.meta)

				end := start + length - 1
				if !bytes.Equal(body, data[start:end+1]) || resp.Header.Get("Content-Range") != contentRange(start, end, int64(len(data))) {
					t.Fatal("incorrect page")
				}
			}

			if tc.status == 200 || tc.status == 206 {
				if resp.Header.Get("Cache-Control") != "max-age=60" || resp.Header.Get("ETag") != store.meta.ETag {
					t.Fatal("missing metadata headers")
				}
			} else if store.opens.Load() != opens {
				t.Fatal("rejected request opened a source")
			}

			if len(resp.TransferEncoding) != 0 {
				t.Fatal("chunked response")
			}
		})
	}

	if store.stats.Load() != 18 || store.opens.Load() != 11 || store.opens.Load() != store.closes.Load() {
		t.Fatalf("stats=%d opens=%d closes=%d", store.stats.Load(), store.opens.Load(), store.closes.Load())
	}
}

func TestOriginRejectsBadTargetsAndMetadata(t *testing.T) {
	store := &memoryStore{meta: Metadata{ETag: `"ok"`}}
	o, _ := NewRangeOrigin(store)

	for _, target := range []string{"", "relative", "/x#frag", "/has space", "/cr\r\n"} {
		r := httptest.NewRequest("HEAD", "/metadata", nil)
		r.RequestURI = target
		w := httptest.NewRecorder()
		o.ServeHTTP(w, r)

		if w.Code != 400 {
			t.Errorf("target %q: %d", target, w.Code)
		}
	}

	r := httptest.NewRequest("HEAD", "/metadata", nil)
	r.ContentLength = 1
	w := httptest.NewRecorder()
	o.ServeHTTP(w, r)

	if w.Code != 400 || store.stats.Load() != 0 {
		t.Fatal("ambiguous target reached store")
	}

	for _, m := range []Metadata{{Size: -1, ETag: checksumTag(nil)}, {}, {ETag: `"v1"`}, {ETag: "W/" + checksumTag(nil)}, {ETag: strings.ToUpper(checksumTag(nil))}, {ETag: "unquoted"}, {ETag: "\"newline\n\""}, {ETag: checksumTag(nil), TTL: durationPointer(-time.Second)}} {
		store.meta = m
		r.ContentLength = 0
		w = httptest.NewRecorder()
		o.ServeHTTP(w, r)

		if w.Code != 500 {
			t.Fatalf("bad metadata %+v: %d", m, w.Code)
		}
	}
}

func TestOriginErrorsAndTruncation(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{{fs.ErrNotExist, 404}, {fs.ErrPermission, 403}, {ErrVersionChanged, 412}, {errors.New("private backend failure"), 500}} {
		store := &memoryStore{err: tc.err}
		o, _ := NewRangeOrigin(store)

		for _, method := range []string{"HEAD", "GET"} {
			path := "/metadata"
			if method == "GET" {
				path = "/page"
			}

			r := httptest.NewRequest(method, path, nil)
			r.Header.Set("X-Racer-Target", "/x")

			w := httptest.NewRecorder()
			o.ServeHTTP(w, r)

			if w.Code != tc.status || w.Body.Len() != 0 {
				t.Fatalf("error response %d %q", w.Code, w.Body.String())
			}
		}
	}

	store := &memoryStore{data: []byte("ab"), meta: Metadata{Size: 3, ETag: checksumTag([]byte("abc"))}}
	o, _ := NewRangeOrigin(store)

	s := httptest.NewServer(o)
	defer s.Close()

	r, _ := http.NewRequest("GET", s.URL+"/page", nil)
	r.Header.Set("X-Racer-Target", "/x")
	r.Header.Set("Range", "bytes=0-2")

	resp, err := s.Client().Do(r)
	if err == nil {
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
	}

	if err == nil {
		t.Fatal("truncated source was successful")
	}

	if store.closes.Load() != 1 {
		t.Fatal("source not released after abort")
	}
}

type changingStore struct {
	memoryStore
	openErr error
	retain  bool
}

func (s *changingStore) ResolveRange(ctx context.Context, target string, _ []byte) (ResolvedRange, error) {
	m, err := s.Stat(ctx, target, nil)
	// Simulate publication after Stat resolves the old version.
	s.meta.ETag = checksumTag([]byte("new"))

	return &testResolvedRange{meta: m, open: func(ctx context.Context, off, length int64) (io.ReadCloser, error) {
		return s.openRange(ctx, m.ETag, off, length)
	}}, err
}

func (s *changingStore) openRange(ctx context.Context, etag string, off, length int64) (io.ReadCloser, error) {
	if s.openErr != nil {
		s.opens.Add(1)
		return &memorySource{Reader: bytes.NewReader(nil), closes: &s.closes}, s.openErr
	}

	if s.retain {
		// The old immutable version remains available by ETag.
		s.opens.Add(1)

		if etag != checksumTag(s.data) {
			return nil, ErrVersionChanged
		}

		return &memorySource{Reader: io.NewSectionReader(bytes.NewReader(s.data), off, length), closes: &s.closes}, nil
	}

	return s.memoryStore.openRange(ctx, etag, off, length)
}

func TestOriginVersionBinding(t *testing.T) {
	for _, tc := range []struct {
		name    string
		retain  bool
		openErr error
		status  int
	}{
		{"version unavailable", false, nil, 412},
		{"version retained", true, nil, 206},
		{"open failure retains ownership", false, fs.ErrPermission, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &changingStore{
				memoryStore: memoryStore{data: []byte("old"), meta: Metadata{Size: 3, ETag: checksumTag([]byte("old"))}},
				retain:      tc.retain, openErr: tc.openErr,
			}
			o, _ := NewRangeOrigin(store)
			r := httptest.NewRequest("GET", "/page", nil)
			r.Header.Set("X-Racer-Target", "/object")
			r.Header.Set("If-Match", checksumTag([]byte("old")))
			r.Header.Set("Range", "bytes=0-2")

			w := httptest.NewRecorder()
			o.ServeHTTP(w, r)

			if w.Code != tc.status || store.stats.Load() != 1 || store.opens.Load() != 1 {
				t.Fatalf("status=%d stats=%d opens=%d", w.Code, store.stats.Load(), store.opens.Load())
			}

			if tc.retain {
				if w.Body.String() != "old" || w.Header().Get("ETag") != checksumTag([]byte("old")) || store.closes.Load() != 1 {
					t.Fatal("pinned source was not served and released")
				}
			} else if w.Body.Len() != 0 || store.closes.Load() != 0 {
				t.Fatal("failed open returned data or transferred ownership")
			}
		})
	}
}

type cancelResponse struct {
	discardResponse
	cancel context.CancelFunc
}

func (w cancelResponse) Write(p []byte) (int, error) {
	w.cancel()
	return len(p), nil
}

func TestOriginClosesSourceOnCancellation(t *testing.T) {
	store := &memoryStore{data: payload(64 << 10), meta: Metadata{Size: 64 << 10, ETag: checksumTag(payload(64 << 10))}}
	o, _ := NewRangeOrigin(store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := httptest.NewRequest("GET", "/page", nil).WithContext(ctx)
	r.Header.Set("X-Racer-Target", "/object")
	r.Header.Set("Range", "bytes=0-65535")

	defer func() {
		if p := recover(); p != http.ErrAbortHandler {
			t.Errorf("expected aborted response, got %v", p)
		}

		if store.closes.Load() != 1 {
			t.Error("canceled source was not closed exactly once")
		}
	}()

	o.ServeHTTP(cancelResponse{discardResponse{make(http.Header)}, cancel}, r)
}

func TestETagLists(t *testing.T) {
	for _, tc := range []struct {
		value, tag   string
		match, valid bool
	}{
		{`"a,b", "c"`, `"a,b"`, true, true},
		{`W/"x", "y"`, `"x"`, false, true},
		{`*`, "", true, true},
		{`"x"`, `W/"x"`, false, true},
		{``, `"x"`, false, false},
		{`*, "x"`, `"x"`, false, false},
		{`"x",, "y"`, `"x"`, true, true},
	} {
		match, valid := matchesETag([]string{tc.value}, tc.tag, true)
		if match != tc.match || valid != tc.valid {
			t.Fatalf("%+v: %v %v", tc, match, valid)
		}
	}
}

func BenchmarkOriginPage(b *testing.B) {
	data := payload(int(PageSize))
	store := &memoryStore{data: data, meta: Metadata{Size: PageSize, ETag: checksumTag(data)}}
	o, _ := NewRangeOrigin(store)
	r := httptest.NewRequest("GET", "/page", nil)
	r.Header.Set("X-Racer-Target", "/x")
	r.Header.Set("Range", "bytes=0-"+strconv.FormatInt(PageSize-1, 10))

	w := discardResponse{make(http.Header)}

	b.SetBytes(PageSize)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		o.ServeHTTP(w, r)
	}
}

type discardResponse struct{ h http.Header }

func (w discardResponse) Header() http.Header         { return w.h }
func (w discardResponse) WriteHeader(int)             {}
func (w discardResponse) Write(p []byte) (int, error) { return len(p), nil }
