// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type sliceWriter []byte

func (b sliceWriter) WriteAt(p []byte, off int64) (int, error) {
	n := copy(b[off:], p)
	if n != len(p) {
		return n, io.ErrShortWrite
	}

	return n, nil
}

func socketDirectory(t testing.TB) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "racer-")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	return dir
}

func unixTestServer(t testing.TB, handler http.Handler) *httptest.Server {
	t.Helper()

	s := httptest.NewUnstartedServer(handler)
	_ = s.Listener.Close()

	listener, err := net.Listen("unix", filepath.Join(socketDirectory(t), "origin"))
	if err != nil {
		t.Fatal(err)
	}

	s.Listener = listener
	s.Start()
	t.Cleanup(s.Close)

	return s
}

func newTestClient(t testing.TB, handler http.Handler, options ClientOptions) *Client {
	t.Helper()

	s := unixTestServer(t, handler)

	c, err := NewClient(s.Listener.Addr().String(), options)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(c.CloseIdleConnections)

	return c
}

func payload(size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i % 251)
	}

	return b
}

func checksumTag(data []byte) string { return fmt.Sprintf(`"%x"`, sha256.Sum256(data)) }

func TestDownloadHEADThenBoundedConcurrentPages(t *testing.T) {
	data := payload(int(3*PageSize + 17))

	var heads, gets, active, peak atomic.Int32

	gate := make(chan struct{})

	var (
		release sync.Once
		ranges  sync.Map
	)

	target := "/a%2fb//../c?b=2&a=1&a=3"
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.RequestURI != target || r.Header.Get("Accept-Encoding") != "identity" || r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("request identity/headers lost: %s %v", r.RequestURI, r.Header)
		}

		w.Header().Set("ETag", checksumTag(data))
		w.Header().Set("Content-Type", "application/octet-stream")

		if r.Method == "HEAD" {
			heads.Add(1)
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))

			return
		}

		if heads.Load() != 1 || r.Header.Get("If-Match") != checksumTag(data) {
			t.Error("GET before HEAD or without pinned validator")
		}

		gets.Add(1)

		n := active.Add(1)
		defer active.Add(-1)

		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}

		if n == 2 {
			release.Do(func() { close(gate) })
		}

		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}

		start, end, ok := pageRange(r.Header.Get("Range"), int64(len(data)))
		if !ok {
			t.Error("invalid page range")
			w.WriteHeader(416)

			return
		}

		if _, loaded := ranges.LoadOrStore(start, true); loaded {
			t.Error("duplicate page")
		}

		w.Header().Set("Content-Range", contentRange(start, end, int64(len(data))))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(206)
		_, _ = w.Write(data[start : end+1])
	}), ClientOptions{Concurrency: 2, MaxIdleConnections: 1, MaxActiveRequests: 3, Header: http.Header{"Authorization": {"Bearer token"}}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dst := make(sliceWriter, len(data))

	m, err := c.Download(ctx, target, dst)
	if err != nil || m.Size != int64(len(data)) || !bytes.Equal(dst, data) {
		t.Fatalf("download: %+v %v", m, err)
	}

	if heads.Load() != 1 || gets.Load() != 4 || peak.Load() != 2 {
		t.Fatalf("HEAD=%d GET=%d concurrency=%d", heads.Load(), gets.Load(), peak.Load())
	}
}

func TestReadAtBoundariesAndEOF(t *testing.T) {
	data := payload(int(2*PageSize + 17))

	var heads atomic.Int32

	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			heads.Add(1)
		}

		w.Header().Set("ETag", checksumTag(data))
		http.ServeContent(w, r, "object", time.Time{}, bytes.NewReader(data))
	}), ClientOptions{})

	o, err := c.Open(context.Background(), "/object?")
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		off  int64
		size int
		want int
		err  error
	}{
		{PageSize - 3, 10, 10, nil},
		{1, int(PageSize + 5), int(PageSize + 5), nil},
		{int64(len(data)) - 4, 10, 4, io.EOF},
		{int64(len(data)), 1, 0, io.EOF},
		{0, 0, 0, nil},
	} {
		p := make([]byte, tc.size)

		n, err := o.ReadAt(context.Background(), p, tc.off)
		if n != tc.want || !errors.Is(err, tc.err) || !bytes.Equal(p[:n], data[tc.off:tc.off+int64(n)]) {
			t.Fatalf("ReadAt %+v: %d %v", tc, n, err)
		}
	}

	if heads.Load() != 1 {
		t.Fatal("snapshot repeated HEAD")
	}
}

func TestRejectInvalidPages(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(http.ResponseWriter)
		body   string
		want   error
	}{
		{"version", func(w http.ResponseWriter) { w.Header().Set("ETag", checksumTag([]byte("new"))) }, "abc", ErrVersionChanged},
		{"missing-tag", func(w http.ResponseWriter) { w.Header().Del("ETag") }, "abc", ErrVersionChanged},
		{"range", func(w http.ResponseWriter) { w.Header().Set("Content-Range", "bytes 1-3/4") }, "abc", ErrProtocol},
		{"length", func(w http.ResponseWriter) { w.Header().Set("Content-Length", "4") }, "abcd", ErrProtocol},
		{"gzip", func(w http.ResponseWriter) { w.Header().Set("Content-Encoding", "gzip") }, "abc", ErrProtocol},
		{"short", func(http.ResponseWriter) {}, "ab", io.ErrUnexpectedEOF},
		{"precondition", func(w http.ResponseWriter) { w.WriteHeader(412) }, "", ErrVersionChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", checksumTag([]byte("abc")))
				w.Header().Set("Content-Length", "3")
				w.Header().Set("Content-Type", "application/octet-stream")

				if r.Method == "HEAD" {
					return
				}

				w.Header().Set("Content-Range", "bytes 0-2/3")
				tc.change(w)

				if tc.name != "precondition" {
					w.WriteHeader(206)
					_, _ = io.WriteString(w, tc.body)
				}
			}), ClientOptions{})

			_, err := c.Download(context.Background(), "/x", make(sliceWriter, 3))
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
		})
	}
}

func TestCancellationAndWorkerJoin(t *testing.T) {
	started := make(chan struct{})
	done := make(chan struct{})
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", checksumTag(payload(int(2*PageSize))))
		w.Header().Set("Content-Length", strconv.FormatInt(2*PageSize, 10))

		if r.Method == "HEAD" {
			return
		}

		if r.Header.Get("Range") == fmt.Sprintf("bytes=0-%d", PageSize-1) {
			close(started)
			<-r.Context().Done()
			close(done)

			return
		}

		<-started
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(412)
	}), ClientOptions{Concurrency: 2})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := c.Download(ctx, "/x", make(sliceWriter, 2*PageSize))
	if !errors.Is(err, ErrVersionChanged) {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("sibling request not canceled")
	}
}

func TestValidatorPolicyEmptyAndStatuses(t *testing.T) {
	for _, tag := range []string{"", `W/"weak"`, `"strong"`, checksumTag(nil), "W/" + checksumTag(nil), strings.ToUpper(checksumTag(nil)), `"` + strings.Repeat("g", 64) + `"`, `"` + strings.Repeat("a", 63) + `"`, `"` + strings.Repeat("a", 65) + `"`} {
		for _, stat := range []bool{false, true} {
			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "HEAD" {
					t.Error("empty object fetched pages")
				}

				if tag != "" {
					w.Header().Set("ETag", tag)
				}

				w.Header().Set("Content-Length", "0")
			}), ClientOptions{})

			var err error
			if stat {
				_, err = c.Stat(context.Background(), "/empty")
			} else {
				_, err = c.Download(context.Background(), "/empty", make(sliceWriter, 0))
			}

			if tag != checksumTag(nil) {
				if !errors.Is(err, ErrNoValidator) {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		}
	}

	c := newTestClient(t, http.NotFoundHandler(), ClientOptions{})
	_, err := c.Stat(context.Background(), "/missing")

	var status *HTTPError
	if !errors.Is(err, fs.ErrNotExist) || !errors.As(err, &status) || status.StatusCode != 404 {
		t.Fatal(err)
	}
}

func TestClientValidationAndRedirect(t *testing.T) {
	for _, endpoint := range []string{"", "relative", "@abstract", "/nul\x00socket", "/" + strings.Repeat("a", 107), "ftp://host", "http://host/prefix", "http://host/user/..", "http://host/%2f", "http://user@host", "http://host?", "http://host/#"} {
		if _, err := NewClient(endpoint, ClientOptions{}); err == nil {
			t.Errorf("accepted %q", endpoint)
		}
	}

	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Location", "/other"); w.WriteHeader(302) }), ClientOptions{})
	for _, target := range []string{"relative", "/space here", "/x#fragment", "/x\r\nHeader: x", "/bad%zz"} {
		if _, err := c.Stat(context.Background(), target); err == nil {
			t.Errorf("accepted %q", target)
		}
	}

	_, err := c.Stat(context.Background(), "/x")

	var status *HTTPError
	if !errors.As(err, &status) || status.StatusCode != 302 {
		t.Fatal(err)
	}

	if _, err := NewClient("/run/racer/test-uid/cache", ClientOptions{Header: http.Header{"rAnGe": {"bytes=0-1"}}}); err == nil {
		t.Fatal("accepted reserved header")
	}
}

type failingWriter struct{ err error }

func (w failingWriter) WriteAt([]byte, int64) (int, error) { return 0, w.err }

func TestDestinationError(t *testing.T) {
	want := errors.New("disk full")
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", checksumTag([]byte("abc")))
		http.ServeContent(w, r, "x", time.Time{}, strings.NewReader("abc"))
	}), ClientOptions{})

	_, err := c.Download(context.Background(), "/x", failingWriter{want})
	if !errors.Is(err, want) {
		t.Fatal(err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type signalingBody struct {
	io.Reader
	done chan struct{}
}

func (b signalingBody) Close() error { close(b.done); return nil }

func TestReadAtPartialCountWithOutOfOrderCompletion(t *testing.T) {
	// The final page completes before the first one truncates. Only the first
	// page's successful prefix contributes to ReaderAt's count.
	done := make(chan struct{})

	cl, err := NewClient("/run/racer/test-uid/cache", ClientOptions{Concurrency: 2})
	if err != nil {
		t.Fatal(err)
	}

	cl.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		h := http.Header{"Etag": {checksumTag(append(payload(int(PageSize)), []byte("xyz")...))}}

		resp := &http.Response{StatusCode: 200, Header: h, ContentLength: PageSize + 3, Body: io.NopCloser(strings.NewReader(""))}
		if r.Method == "HEAD" {
			return resp, nil
		}

		start, end, _ := pageRange(r.Header.Get("Range"), PageSize+3)
		resp.StatusCode = 206
		resp.ContentLength = end - start + 1
		h.Set("Content-Range", contentRange(start, end, PageSize+3))

		if start == 0 {
			select {
			case <-done:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}

			resp.Body = io.NopCloser(strings.NewReader("ab"))
		} else {
			resp.Body = signalingBody{strings.NewReader("xyz"), done}
		}

		return resp, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	o, err := cl.Open(ctx, "/x")
	if err != nil {
		t.Fatal(err)
	}

	p := make([]byte, PageSize+3)

	n, err := o.ReadAt(ctx, p, 0)
	if n != 2 || !errors.Is(err, io.ErrUnexpectedEOF) || string(p[:2]) != "ab" || string(p[PageSize:]) != "xyz" {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestProtocolFramingAndCanceledContext(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*http.Response)
		want   error
	}{
		{"chunked", func(r *http.Response) { r.TransferEncoding = []string{"chunked"} }, ErrProtocol},
		{"missing-length", func(r *http.Response) { r.ContentLength = -1 }, ErrProtocol},
		{"duplicate-etag", func(r *http.Response) { r.Header.Add("ETag", checksumTag([]byte("abc"))) }, ErrProtocol},
		{"invalid-etag", func(r *http.Response) { r.Header.Set("ETag", "unquoted") }, ErrProtocol},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewClient("/run/racer/test-uid/cache", ClientOptions{})
			if err != nil {
				t.Fatal(err)
			}

			c.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				r := &http.Response{StatusCode: 200, ContentLength: 3, Header: http.Header{"Etag": {checksumTag([]byte("abc"))}}, Body: io.NopCloser(strings.NewReader(""))}
				tc.mutate(r)

				return r, nil
			})

			_, err = c.Stat(context.Background(), "/x")
			if !errors.Is(err, tc.want) {
				t.Fatal(err)
			}
		})
	}

	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", checksumTag([]byte("0123456789")))
		w.Header().Set("Content-Length", "10")
		w.Header().Set("Content-Type", "application/octet-stream")

		if r.Method == "GET" {
			_, _ = io.WriteString(w, "0123456789")
		}
	}), ClientOptions{})

	o, err := c.Open(context.Background(), "/x")
	if err != nil {
		t.Fatal(err)
	}
	// A 200 is valid for the whole object, but never for a partial request.
	if _, err := o.Download(context.Background(), make(sliceWriter, 10)); err != nil {
		t.Fatal(err)
	}

	if _, err := o.ReadAt(context.Background(), make([]byte, 3), 0); !errors.Is(err, ErrProtocol) {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if n, err := o.ReadAt(ctx, make([]byte, 10), 0); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatal(n, err)
	}

	if n, err := o.Download(ctx, make(sliceWriter, 10)); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatal(n, err)
	}
}
