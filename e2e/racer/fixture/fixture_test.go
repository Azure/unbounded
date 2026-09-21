// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBackendContract(t *testing.T) {
	o := NewOrigin()
	request := func(method, target, match, byteRange string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, nil)
		r.Header.Set("X-Racer-Target", "/ignored")
		r.Header.Set("If-Match", match)
		r.Header.Set("Range", byteRange)

		w := httptest.NewRecorder()
		o.ServeHTTP(w, r)

		return w
	}

	h := request("HEAD", "/object", "", "")
	if h.Code != 200 || h.Header().Get("Content-Length") != "16384" || h.Body.Len() != 0 {
		t.Fatalf("metadata: %+v", h)
	}

	p := request("GET", "/object", h.Header().Get("ETag"), "bytes=13-1024")
	if p.Code != 206 || p.Header().Get("Content-Range") != "bytes 13-1024/16384" || !bytes.Equal(p.Body.Bytes(), Body(1)[13:1025]) {
		t.Fatalf("page: %+v", p)
	}

	if w := request("POST", "/version?value=2", "", ""); w.Code != 200 {
		t.Fatal(w.Code)
	}

	if w := request("GET", "/object", h.Header().Get("ETag"), "bytes=0-10"); w.Code != http.StatusPreconditionFailed {
		t.Fatal(w.Code)
	}

	if w := request("GET", "/object", ETag(2), "bytes=0-16384"); w.Code != 416 {
		t.Fatal(w.Code)
	}

	if w := request("HEAD", "/missing", "", ""); w.Code != 404 {
		t.Fatal(w.Code)
	}

	if w := request("POST", "/object", "", ""); w.Code != 400 {
		t.Fatal(w.Code)
	}

	for _, target := range []string{"//object%2f?x=1&x=2", "/object?", "/metadata", "/page", "/%68its"} {
		if w := request("HEAD", target, "", ""); w.Code != 200 {
			t.Fatal(w.Code)
		}

		if got := o.hits[len(o.hits)-1].Target; got != target {
			t.Fatalf("target %q != %q", got, target)
		}
	}
}
