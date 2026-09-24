// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

type pinnedTestSource struct {
	*os.File
	offset, length int64
	closes         int
}

func (s *pinnedTestSource) FileRange() (*os.File, int64, int64) { return s.File, s.offset, s.length }
func (s *pinnedTestSource) Close() error                        { s.closes++; return s.File.Close() }
func (*pinnedTestSource) Read([]byte) (int, error)              { panic("pinned file capability ignored") }
func (*pinnedTestSource) ReadAt([]byte, int64) (int, error)     { panic("pinned file capability ignored") }

type pinnedTestStore struct {
	meta   Metadata
	source *pinnedTestSource
	err    error
	opens  int
}

func (s *pinnedTestStore) Stat(context.Context, string, []byte) (Metadata, error) { return s.meta, nil }
func (s *pinnedTestStore) Open(context.Context, string, string, []byte) (Source, error) {
	s.opens++
	return s.source, s.err
}

func (s *pinnedTestStore) OpenRange(context.Context, string, string, int64, int64, []byte) (io.ReadCloser, error) {
	s.opens++
	return s.source, s.err
}

type pinnedResolvedStore struct {
	*pinnedTestStore
	closes int
}

func (s *pinnedResolvedStore) ResolveRange(context.Context, string, []byte) (ResolvedRange, error) {
	return pinnedResolvedHandle{s}, nil
}

type pinnedResolvedHandle struct{ s *pinnedResolvedStore }

func (h pinnedResolvedHandle) Metadata() Metadata { return h.s.meta }
func (h pinnedResolvedHandle) OpenRange(ctx context.Context, off, length int64) (io.ReadCloser, error) {
	return h.s.OpenRange(ctx, "", "", off, length, nil)
}

func (h pinnedResolvedHandle) Close() error {
	h.s.closes++
	if h.s.opens > 0 && h.s.err == nil && h.s.source.closes != 1 {
		panic("handle closed before source")
	}

	return nil
}

func pinnedFixture(t *testing.T, data []byte) *pinnedTestStore {
	t.Helper()

	f := destinationFile(t)
	if _, err := f.Write(append([]byte("prefix"), data...)); err != nil {
		t.Fatal(err)
	}

	return &pinnedTestStore{meta: Metadata{Size: int64(len(data)), ETag: checksumTag(data)}, source: &pinnedTestSource{File: f, offset: 6, length: int64(len(data))}}
}

func TestPinnedOriginCapabilitiesAndOwnership(t *testing.T) {
	for _, kind := range []string{"store", "range", "resolved"} {
		for _, mode := range []string{"full", "range", "head", "precondition", "invalid", "short-file", "open-error", "cancel", "write-error"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				store := pinnedFixture(t, []byte("payload"))
				resolved := &pinnedResolvedStore{pinnedTestStore: store}

				var origin *Origin

				switch kind {
				case "store":
					origin, _ = NewOrigin(store)
				case "range":
					origin, _ = NewRangeOrigin(store)
				case "resolved":
					origin, _ = NewRangeOrigin(resolved)
				}

				r := httptest.NewRequest("GET", "/file", nil)
				status := 200
				want := "payload"

				switch mode {
				case "range":
					r.Header.Set("Range", "bytes=2-4")

					status = 206
					want = "ylo"

					if kind != "store" {
						store.source.offset += 2
						store.source.length = 3
					}
				case "head":
					r.Method = "HEAD"
					want = ""
				case "precondition":
					r.Header.Set("If-Match", `"old"`)

					status = 412
					want = ""
				case "invalid":
					store.source.offset = -1
					status = 500
					want = ""
				case "short-file":
					if err := store.source.Truncate(8); err != nil {
						t.Fatal(err)
					}

					status = 500
					want = ""
				case "open-error":
					store.err = ErrVersionChanged
					status = 412
					want = ""
				case "cancel":
					ctx, cancel := context.WithCancel(r.Context())
					cancel()

					r = r.WithContext(ctx)
					status = 500
					want = ""
				}

				w := httptest.NewRecorder()

				var writer http.ResponseWriter = w
				if mode == "write-error" {
					writer = failedResolvedResponse{discardResponse{make(http.Header)}}
				}

				var recovered any

				func() { defer func() { recovered = recover() }(); origin.ServeHTTP(writer, r) }()

				if mode == "write-error" {
					if recovered != http.ErrAbortHandler {
						t.Fatal(recovered)
					}
				} else if recovered != nil || w.Code != status || w.Body.String() != want {
					t.Fatal(recovered, w.Code, w.Body.String())
				}

				closes := 1
				if mode == "head" || mode == "precondition" || mode == "open-error" {
					closes = 0
				}

				if store.source.closes != closes {
					t.Fatal("source close count", store.source.closes)
				}

				_, statErr := store.source.Stat()
				if closes == 1 && !errors.Is(statErr, os.ErrClosed) {
					t.Fatal("source FD retained", statErr)
				}

				if kind == "resolved" && resolved.closes != 1 {
					t.Fatal("handle retained")
				}
			})
		}
	}
}

type truncateResponse struct {
	*httptest.ResponseRecorder
	file *os.File
}

func (w truncateResponse) WriteHeader(status int) {
	w.ResponseRecorder.WriteHeader(status)
	_ = w.file.Truncate(8)
}

func TestPinnedOriginTruncationAfterHeaders(t *testing.T) {
	store := pinnedFixture(t, []byte("payload"))
	origin, _ := NewOrigin(store)
	w := truncateResponse{httptest.NewRecorder(), store.source.File}

	defer func() {
		if p := recover(); p != http.ErrAbortHandler || store.source.closes != 1 || !bytes.Equal(w.Body.Bytes(), []byte("pa")) {
			t.Errorf("panic=%v closes=%d bytes=%q", p, store.source.closes, w.Body.Bytes())
		}
	}()

	origin.ServeHTTP(w, httptest.NewRequest("GET", "/file", nil))
}
