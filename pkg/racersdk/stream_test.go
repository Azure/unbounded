// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestAuthorizationViewsAndLimits(t *testing.T) {
	base := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", checksumTag(nil))
		w.Header().Set("Content-Length", "0")

		data, status := decodeOriginData(r.Header)
		if status != 0 || string(data) != r.URL.Query().Get("data") {
			t.Error("origin data crossed views")
		}
	}), ClientOptions{})
	for _, value := range [][]byte{bytes.Repeat([]byte{0xff}, MaxOriginDataBytes+1)} {
		if _, err := base.WithOriginData(value); err == nil || strings.Contains(err.Error(), string(value)) {
			t.Fatalf("invalid origin data accepted or disclosed: length=%d", len(value))
		}
	}

	if _, err := base.WithOriginData(bytes.Repeat([]byte{0xff}, MaxOriginDataBytes)); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup

	for _, value := range []string{"a", "b", "", "\x00\xff\r\n binary "} {
		input := []byte(value)

		view, err := base.WithOriginData(input)
		if err != nil {
			t.Fatal(err)
		}

		if view.http != base.http || view.streamPool != base.streamPool {
			t.Fatal("pool not shared")
		}

		clear(input)

		cleared, err := view.WithOriginData(nil)
		if err != nil || cleared.header.Get("Racer-Origin-Data") != "" {
			t.Fatal("could not clear view")
		}

		wg.Go(func() {
			for range 5 {
				if _, err := view.Stat(t.Context(), "/?data="+url.QueryEscape(value)); err != nil {
					t.Error(err)
				}
			}
		})
	}

	wg.Wait()

	if base.header.Get("Racer-Origin-Data") != "" {
		t.Fatal("base mutated")
	}
}

func TestBoundedResponseFields(t *testing.T) {
	for _, field := range []struct {
		name  string
		limit int
	}{{"Content-Type", 256}, {"WWW-Authenticate", 1024}, {"Retry-After", 128}} {
		for _, value := range []string{"", "bad\x7f", strings.Repeat("x", field.limit+1)} {
			if _, err := boundedField(http.Header{http.CanonicalHeaderKey(field.name): {value}}, field.name, field.limit); err == nil {
				t.Fatal(field.name, len(value))
			}
		}

		if _, err := boundedField(http.Header{http.CanonicalHeaderKey(field.name): {"a", "b"}}, field.name, field.limit); err == nil {
			t.Fatal("duplicates")
		}

		value := strings.Repeat("x", field.limit)
		if got, err := boundedField(http.Header{http.CanonicalHeaderKey(field.name): {value}}, field.name, field.limit); err != nil || got != value {
			t.Fatal(err)
		}
	}

	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="registry"`)
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(401)
	}), ClientOptions{})
	_, err := c.Stat(t.Context(), "/object")

	var status *HTTPError
	if !errors.As(err, &status) || status.StatusCode != 401 || status.WWWAuthenticate != `Bearer realm="registry"` || status.RetryAfter != "12" {
		t.Fatal(err)
	}
}

type rangeTestStore struct {
	data           []byte
	auth           string
	opens          int
	closed         int
	offset, length int64
}

func (s *rangeTestStore) ResolveRange(ctx context.Context, target string, data []byte) (ResolvedRange, error) {
	m, err := s.Stat(ctx, target, data)

	return &testResolvedRange{meta: m, open: func(ctx context.Context, off, length int64) (io.ReadCloser, error) {
		return s.OpenRange(ctx, target, m.ETag, off, length, data)
	}}, err
}

type testResolvedRange struct {
	meta Metadata
	open func(context.Context, int64, int64) (io.ReadCloser, error)
}

func (r *testResolvedRange) Metadata() Metadata { return r.meta }
func (r *testResolvedRange) Close() error       { return nil }
func (r *testResolvedRange) OpenRange(ctx context.Context, off, length int64) (io.ReadCloser, error) {
	return r.open(ctx, off, length)
}

func (s *rangeTestStore) Stat(_ context.Context, _ string, originData []byte) (Metadata, error) {
	s.auth = string(originData)
	return Metadata{Size: int64(len(s.data)), ETag: checksumTag(s.data), ContentType: "application/vnd.oci.image.manifest.v1+json"}, nil
}

func (s *rangeTestStore) OpenRange(_ context.Context, _, etag string, offset, length int64, originData []byte) (io.ReadCloser, error) {
	s.opens++

	s.offset, s.length = offset, length
	if string(originData) != s.auth || etag != checksumTag(s.data) {
		return nil, ErrVersionChanged
	}

	return &rangeTestBody{Reader: bytes.NewReader(s.data[offset : offset+length]), close: func() { s.closed++ }}, nil
}

type rangeTestBody struct {
	io.Reader
	close func()
}

func (b *rangeTestBody) Close() error { b.close(); return nil }

func TestRangeOriginSingleOpenAndContentType(t *testing.T) {
	store := &rangeTestStore{data: payload(128 << 10)}

	origin, err := NewRangeOrigin(store)
	if err != nil {
		t.Fatal(err)
	}

	c := newTestClient(t, origin, ClientOptions{})

	c, err = c.WithOriginData([]byte("\x00\xff\r\n request"))
	if err != nil {
		t.Fatal(err)
	}

	o, err := c.Open(t.Context(), "/blob")
	if err != nil {
		t.Fatal(err)
	}

	if o.Metadata().ContentType != "application/vnd.oci.image.manifest.v1+json" {
		t.Fatal(o.Metadata())
	}

	s, err := o.ReadRange(t.Context(), 7, 100000)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var got bytes.Buffer

	_, err = s.WriteTo(&got)
	if err != nil || !bytes.Equal(got.Bytes(), store.data[7:100007]) {
		t.Fatal(err)
	}

	if store.opens != 1 || store.closed != 1 || store.offset != 7 || store.length != 100000 || store.auth != "\x00\xff\r\n request" {
		t.Fatal("range store request changed")
	}
}

// A generated representation tests multi-page streams without page-sized Go
// allocations in either the fixture or its consumer.
func generatedStreamClient(t *testing.T, size int64, requests *[]string) *Client {
	t.Helper()

	return newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", checksumTag(nil))
		w.Header().Set("Content-Type", "application/octet-stream")

		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			return
		}

		if r.Header.Get("If-Match") != checksumTag(nil) {
			t.Error("missing version pin")
		}

		start, length, status := objectRange(r.Header, Metadata{Size: size})

		end := start + length - 1
		if status != 206 || start/PageSize != end/PageSize {
			w.WriteHeader(416)
			return
		}

		if requests != nil {
			*requests = append(*requests, r.Header.Get("Range"))
		}

		w.Header().Set("Content-Range", contentRange(start, end, size))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(206)

		buf := make([]byte, 32<<10)
		for left := end - start + 1; left > 0; {
			n, err := w.Write(buf[:min(left, int64(len(buf)))])
			if err != nil {
				return
			}

			left -= int64(n)
		}
	}), ClientOptions{})
}

func TestStreamSequentialPagesAndRanges(t *testing.T) {
	var requests []string

	c := generatedStreamClient(t, PageSize+23, &requests)

	o, err := c.Open(t.Context(), "/blob")
	if err != nil {
		t.Fatal(err)
	}

	s, err := o.Stream(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	n, err := s.WriteTo(io.Discard)
	_ = s.Close()

	if err != nil || n != PageSize+23 {
		t.Fatal(n, err)
	}

	want := []string{fmt.Sprintf("bytes=0-%d", PageSize-1), fmt.Sprintf("bytes=%d-%d", PageSize, PageSize+22)}
	if fmt.Sprint(requests) != fmt.Sprint(want) {
		t.Fatal(requests)
	}

	for _, bounds := range [][2]int64{{-1, 1}, {0, -1}, {PageSize + 24, 0}, {PageSize + 22, 2}} {
		if _, err := o.ReadRange(t.Context(), bounds[0], bounds[1]); err == nil {
			t.Fatal(bounds)
		}
	}

	s, err = o.ReadRange(t.Context(), PageSize-3, 10)
	if err != nil {
		t.Fatal(err)
	}

	var data bytes.Buffer

	_, err = s.WriteTo(&data)
	_ = s.Close()

	if err != nil || data.Len() != 10 {
		t.Fatal(err)
	}
}

func TestStreamBuffered(t *testing.T) {
	store := &rangeTestStore{data: payload(100000)}
	origin, _ := NewRangeOrigin(store)
	c := newTestClient(t, origin, ClientOptions{})

	o, err := c.Open(t.Context(), "/blob")
	if err != nil {
		t.Fatal(err)
	}

	s, err := o.Stream(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer

	n, err := s.WriteTo(&out)
	_ = s.Close()

	if err != nil || n != int64(len(store.data)) || !bytes.Equal(out.Bytes(), store.data) {
		t.Fatal(n, err)
	}

	if stats := s.Stats(); stats.BufferedBytes != n || stats.SpliceBytes != 0 || stats.SpliceCalls != 0 {
		t.Fatal(stats)
	}
}

type failingRangeStore struct {
	rangeTestStore
	err error
}

func (s *failingRangeStore) ResolveRange(ctx context.Context, target string, data []byte) (ResolvedRange, error) {
	m, err := s.Stat(ctx, target, data)

	return &testResolvedRange{meta: m, open: func(ctx context.Context, off, length int64) (io.ReadCloser, error) {
		return s.OpenRange(ctx, target, m.ETag, off, length, data)
	}}, err
}

func (s *failingRangeStore) OpenRange(ctx context.Context, target, etag string, offset, length int64, originData []byte) (io.ReadCloser, error) {
	if s.err != nil {
		return nil, s.err
	}

	return &rangeTestBody{Reader: strings.NewReader("short"), close: func() { s.closed++ }}, nil
}

func TestRangeOriginErrorsAndShortClose(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
	}{
		{"version", ErrVersionChanged, 412},
		{"auth", &HTTPError{StatusCode: 401, WWWAuthenticate: "Bearer registry", RetryAfter: "10"}, 401},
		{"oversize", &HTTPError{StatusCode: 403, WWWAuthenticate: strings.Repeat("x", 1025)}, 500},
		{"short", nil, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &failingRangeStore{rangeTestStore: rangeTestStore{data: payload(100)}, err: tc.err}

			origin, err := NewRangeOrigin(store)
			if err != nil {
				t.Fatal(err)
			}

			w := httptest.NewRecorder()

			var recovered any

			func() {
				defer func() { recovered = recover() }()

				origin.ServeHTTP(w, httptest.NewRequest("GET", "/blob", nil))
			}()

			if w.Code != tc.status {
				t.Fatal(w.Code)
			}

			if tc.name == "short" {
				if recovered != http.ErrAbortHandler || store.closed != 1 {
					t.Fatal(recovered, store.closed)
				}
			} else if recovered != nil {
				t.Fatal(recovered)
			}

			if tc.name == "auth" && (w.Header().Get("WWW-Authenticate") != "Bearer registry" || w.Header().Get("Retry-After") != "10") {
				t.Fatal(w.Header())
			}
		})
	}
}

func TestAuthorizationMaximumOnWire(t *testing.T) {
	value := make([]byte, MaxOriginDataBytes)
	for i := range value {
		value[i] = byte(i)
	}

	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Racer-Origin-Data") != base64.StdEncoding.EncodeToString(value) || len(r.Header.Values("Racer-Origin-Data")) != 1 {
			t.Error("origin data changed")
		}

		w.Header().Set("ETag", checksumTag(nil))
		w.Header().Set("Content-Length", "1")

		w.Header()["Content-Type"] = nil
		if r.Method == "GET" {
			w.Header().Set("Content-Range", "bytes 0-0/1")
			w.WriteHeader(206)
			_, _ = w.Write([]byte{0})
		}
	}), ClientOptions{})

	c, err := c.WithOriginData(value)
	if err != nil {
		t.Fatal(err)
	}

	o, err := c.Open(t.Context(), "/blob")
	if err != nil {
		t.Fatal(err)
	}

	s, err := o.Stream(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.Prepare(); err != nil {
		t.Fatal(err)
	}

	if n, err := s.WriteTo(io.Discard); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}
