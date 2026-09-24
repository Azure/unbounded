// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

type resolvedTestStore struct {
	RangeStore // Legacy methods must not be called when resolution is available.
	handle     *resolvedTestHandle
	err        error
	resolves   int
	target     string
	data       string
	ctx        context.Context
}

func (s *resolvedTestStore) ResolveRange(ctx context.Context, target string, data []byte) (ResolvedRange, error) {
	s.resolves++

	s.target, s.data, s.ctx = target, string(data), ctx
	if s.handle == nil {
		return nil, s.err
	}

	return s.handle, s.err
}

type resolvedTestHandle struct {
	meta                      Metadata
	body                      string
	err                       error
	nilBody                   bool
	opens, closes, bodyCloses int
	offset, length            int64
	ctx                       context.Context
	closedBeforeBody          bool
}

func (h *resolvedTestHandle) Metadata() Metadata { return h.meta }

func (h *resolvedTestHandle) OpenRange(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
	h.opens++

	h.ctx, h.offset, h.length = ctx, offset, length
	if h.nilBody {
		return nil, h.err
	}

	body := h.body
	if int64(len(body)) >= offset+length {
		body = body[offset : offset+length]
	}

	return &rangeTestBody{Reader: strings.NewReader(body), close: func() { h.bodyCloses++ }}, h.err
}

func (h *resolvedTestHandle) Close() error {
	h.closes++
	h.closedBeforeBody = h.opens > 0 && h.err == nil && !h.nilBody && h.bodyCloses != 1

	return nil
}

func TestResolvedRangeOriginLifecycle(t *testing.T) {
	tag := checksumTag([]byte("payload"))
	for _, tc := range []struct {
		name, method, field, value, body string
		status, opens, bodyCloses        int
		size                             int64
		openErr                          error
		nilBody, abort                   bool
	}{
		{name: "full", method: "GET", body: "payload", size: 7, status: 200, opens: 1, bodyCloses: 1},
		{name: "range", method: "GET", field: "Range", value: "bytes=2-4", body: "payload", size: 7, status: 206, opens: 1, bodyCloses: 1},
		{name: "head", method: "HEAD", field: "Range", value: "bytes=99-", size: 7, status: 200},
		{name: "not-modified", method: "GET", field: "If-None-Match", value: tag, size: 7, status: 304},
		{name: "precondition", method: "GET", field: "If-Match", value: `"old"`, size: 7, status: 412},
		{name: "bad-condition", method: "GET", field: "If-Match", value: "invalid", size: 7, status: 400},
		{name: "unsatisfiable", method: "GET", field: "Range", value: "bytes=7-", size: 7, status: 416},
		{name: "empty", method: "GET", size: 0, status: 200, opens: 1, bodyCloses: 1},
		{name: "empty-range", method: "GET", field: "Range", value: "bytes=0-", size: 0, status: 416},
		{name: "invalid-metadata", method: "GET", size: -1, status: 500},
		{name: "changed", method: "GET", size: 7, status: 412, opens: 1, openErr: ErrVersionChanged},
		{name: "denied", method: "GET", size: 7, status: 403, opens: 1, openErr: fs.ErrPermission},
		{name: "nil-body", method: "GET", size: 7, status: 500, opens: 1, nilBody: true},
		{name: "short", method: "GET", body: "short", size: 7, status: 200, opens: 1, bodyCloses: 1, abort: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &resolvedTestHandle{meta: Metadata{Size: tc.size, ETag: tag, ContentType: "application/octet-stream"}, body: tc.body, err: tc.openErr, nilBody: tc.nilBody}
			store := &resolvedTestStore{handle: h}

			origin, err := NewRangeOrigin(store)
			if err != nil {
				t.Fatal(err)
			}

			r := httptest.NewRequest(tc.method, "/raw%2Ftarget?b=2&a=1", nil)
			r.Header.Set("Racer-Origin-Data", base64.StdEncoding.EncodeToString([]byte("\x00request\xff")))

			if tc.field != "" {
				r.Header.Set(tc.field, tc.value)
			}

			w := httptest.NewRecorder()

			var recovered any

			func() {
				defer func() { recovered = recover() }()

				origin.ServeHTTP(w, r)
			}()

			if (tc.abort && recovered != http.ErrAbortHandler) || (!tc.abort && recovered != nil) {
				t.Fatalf("unexpected panic: %v", recovered)
			}

			if w.Code != tc.status || store.resolves != 1 || h.opens != tc.opens || h.closes != 1 || h.bodyCloses != tc.bodyCloses || h.closedBeforeBody {
				t.Fatalf("status=%d resolves=%d opens=%d closes=%d bodyCloses=%d earlyClose=%v", w.Code, store.resolves, h.opens, h.closes, h.bodyCloses, h.closedBeforeBody)
			}

			if store.target != r.RequestURI || store.data != "\x00request\xff" || store.ctx != r.Context() || (h.opens > 0 && h.ctx != r.Context()) {
				t.Fatal("request target, data, or context changed")
			}

			if tc.name == "range" && (h.offset != 2 || h.length != 3 || w.Body.String() != "ylo" || w.Header().Get("Content-Range") != "bytes 2-4/7") {
				t.Fatal("range not bound to resolved metadata", w.Header(), w.Body.String())
			}

			if tc.name == "full" && (w.Body.String() != "payload" || h.offset != 0 || h.length != 7 || w.Header().Get("ETag") != tag) {
				t.Fatal("full representation changed")
			}

			if tc.status == 416 && w.Header().Get("Content-Range") != "bytes */"+strconv.FormatInt(tc.size, 10) {
				t.Fatal("missing unsatisfied range metadata")
			}

			if tc.opens == 0 && w.Body.Len() != 0 {
				t.Fatal("metadata-only response has a payload")
			}
		})
	}
}

func TestResolvedRangeOriginResolveErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
	}{
		{"missing", fs.ErrNotExist, 404},
		{"permission", fs.ErrPermission, 403},
		{"version", ErrVersionChanged, 412},
		{"backend", errors.New("private"), 500},
		{"nil-handle", nil, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &resolvedTestHandle{}

			store := &resolvedTestStore{err: tc.err, handle: h}
			if tc.err == nil {
				store.handle = nil
			}

			origin, _ := NewRangeOrigin(store)
			w := httptest.NewRecorder()
			origin.ServeHTTP(w, httptest.NewRequest("GET", "/object", nil))

			if w.Code != tc.status || w.Body.Len() != 0 || h.opens != 0 || h.closes != 0 {
				t.Fatal("failed resolution transferred ownership or exposed data", w.Code)
			}
		})
	}
}

func TestResolvedRangeOriginValidatesBeforeResolution(t *testing.T) {
	for _, tc := range []struct {
		name, method, data string
		status             int
	}{
		{"method", "POST", "", 405},
		{"malformed-origin-data", "GET", "!", 400},
		{"oversized-origin-data", "GET", strings.Repeat("A", maxEncodedOriginDataBytes+1), 431},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &resolvedTestStore{}
			origin, _ := NewRangeOrigin(store)
			r := httptest.NewRequest(tc.method, "/object", nil)
			r.Header.Set("Racer-Origin-Data", tc.data)

			w := httptest.NewRecorder()
			origin.ServeHTTP(w, r)

			if w.Code != tc.status || store.resolves != 0 {
				t.Fatal("invalid request reached resolver", w.Code, store.resolves)
			}
		})
	}
}

type failedResolvedResponse struct{ discardResponse }

func (failedResolvedResponse) Write([]byte) (int, error) { return 0, context.Canceled }

func TestResolvedRangeOriginDisconnectedCleanup(t *testing.T) {
	h := &resolvedTestHandle{meta: Metadata{Size: 7, ETag: checksumTag([]byte("payload"))}, body: "payload"}
	origin, _ := NewRangeOrigin(&resolvedTestStore{handle: h})

	defer func() {
		if p := recover(); p != http.ErrAbortHandler || h.bodyCloses != 1 || h.closes != 1 || h.closedBeforeBody {
			t.Errorf("disconnect cleanup: panic=%v body=%d handle=%d early=%v", p, h.bodyCloses, h.closes, h.closedBeforeBody)
		}
	}()

	origin.ServeHTTP(failedResolvedResponse{discardResponse{make(http.Header)}}, httptest.NewRequest("GET", "/object", nil))
}
