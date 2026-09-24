// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
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
	"strings"
	"testing"
	"time"
)

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

func writeObject(ctx context.Context, o *Object, dst io.Writer) (int64, error) {
	s, err := o.Stream(ctx)
	if err != nil {
		return 0, err
	}
	defer s.Close()

	return s.WriteTo(dst)
}

func fetchObject(ctx context.Context, c *Client, target string, dst io.Writer) (Metadata, error) {
	o, err := c.Open(ctx, target)
	if err != nil {
		return Metadata{}, err
	}

	_, err = writeObject(ctx, o, dst)

	return o.Metadata(), err
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

			_, err := fetchObject(t.Context(), c, "/x", io.Discard)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
		})
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
				_, err = c.Stat(t.Context(), "/empty")
			} else {
				_, err = fetchObject(t.Context(), c, "/empty", io.Discard)
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
	_, err := c.Stat(t.Context(), "/missing")

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

	if _, err := NewClient("/cache", ClientOptions{Timeout: -time.Second}); err == nil {
		t.Fatal("accepted negative timeout")
	}

	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Location", "/other"); w.WriteHeader(302) }), ClientOptions{})
	for _, target := range []string{"relative", "/space here", "/x#fragment", "/x\r\nHeader: x", "/bad%zz"} {
		if _, err := c.Stat(t.Context(), target); err == nil {
			t.Errorf("accepted %q", target)
		}
	}

	_, err := c.Stat(t.Context(), "/x")

	var status *HTTPError
	if !errors.As(err, &status) || status.StatusCode != 302 {
		t.Fatal(err)
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestDestinationError(t *testing.T) {
	want := errors.New("disk full")
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", checksumTag([]byte("abc")))
		http.ServeContent(w, r, "x", time.Time{}, strings.NewReader("abc"))
	}), ClientOptions{})

	_, err := fetchObject(t.Context(), c, "/x", failingWriter{want})
	if !errors.Is(err, want) {
		t.Fatal(err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

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
			c, err := NewClient("/run/racer/test/client/socket", ClientOptions{})
			if err != nil {
				t.Fatal(err)
			}

			c.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				r := &http.Response{StatusCode: 200, ContentLength: 3, Header: http.Header{"Etag": {checksumTag([]byte("abc"))}}, Body: io.NopCloser(strings.NewReader(""))}
				tc.mutate(r)

				return r, nil
			})

			_, err = c.Stat(t.Context(), "/x")
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

	o, err := c.Open(t.Context(), "/x")
	if err != nil {
		t.Fatal(err)
	}
	// A 200 is valid for the whole object, but never for a partial request.
	if _, err := writeObject(t.Context(), o, io.Discard); err != nil {
		t.Fatal(err)
	}

	s, err := o.ReadRange(t.Context(), 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.Prepare(); !errors.Is(err, ErrProtocol) {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := o.ReadRange(ctx, 0, 10); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}

	if _, err := o.Stream(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
