// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchRejectsUnsafeCacheUID(t *testing.T) {
	for _, uid := range []string{"", "..", "UPPER", "-edge", "edge-", "a.b", strings.Repeat("a", 64)} {
		if _, err := Fetch("HEAD", "unix://"+uid+"/object", ""); err == nil || !strings.Contains(err.Error(), "cache UID") {
			t.Fatalf("unsafe UID %q was not rejected before dialing: %v", uid, err)
		}
	}
}

// Use a real HTTP server: ResponseRecorder alone does not model the server's
// implicit content sniffing when a handler returns from a bodyless HEAD.
func TestBackendWireRepresentation(t *testing.T) {
	o := NewOrigin()
	s := httptest.NewServer(o)
	t.Cleanup(s.Close)

	for _, version := range []int{1, 2} {
		if version != 1 {
			response, err := Fetch("POST", fmt.Sprintf("%s/version?value=%d", s.URL, version), "")
			if err != nil || response.Status != http.StatusOK {
				t.Fatalf("set version: %+v, %v", response, err)
			}
		}

		head, err := Fetch("HEAD", s.URL+"/live-payload-0", "")
		if err != nil || head.Status != http.StatusOK {
			t.Fatalf("HEAD: %+v, %v", head, err)
		}

		if head.Header.Get("ETag") != ETag(version) || head.Header.Get("Content-Length") != "16384" || len(head.Body) != 0 {
			t.Fatalf("HEAD representation: %+v", head)
		}

		for _, interval := range [][2]int{{0, ObjectSize - 1}, {13, 1024}, {ObjectSize - 1, ObjectSize - 1}} {
			byteRange := fmt.Sprintf("bytes=%d-%d", interval[0], interval[1])

			page, err := Fetch("GET", s.URL+"/live-payload-0", byteRange, "If-Match: "+head.Header.Get("ETag"))
			if err != nil || page.Status != http.StatusPartialContent {
				t.Fatalf("GET %s: %+v, %v", byteRange, page, err)
			}

			if page.Header.Get("Content-Type") != head.Header.Get("Content-Type") || page.Header.Get("ETag") != head.Header.Get("ETag") || !bytes.Equal(page.Body, Body(version)[interval[0]:interval[1]+1]) {
				t.Fatalf("HEAD/GET representation changed: HEAD=%v GET=%v", head.Header, page.Header)
			}

			if page.Header.Get("Content-Type") != "application/octet-stream" {
				t.Fatalf("unexpected content type: %v", page.Header)
			}

			hits := o.Hits()

			hit := hits[len(hits)-1]
			if hit.IfMatch != head.Header.Get("ETag") || hit.ETag != hit.IfMatch || hit.ContentType != page.Header.Get("Content-Type") || hit.Range != byteRange || hit.Status != page.Status {
				t.Fatalf("wire/ledger mismatch: %+v", hit)
			}
		}

		stale, err := Fetch("GET", s.URL+"/live-payload-0", "bytes=0-10", "If-Match: "+ETag(version+1))
		if err != nil || stale.Status != http.StatusPreconditionFailed || stale.Header.Get("ETag") != head.Header.Get("ETag") {
			t.Fatalf("mismatched validator must still fail: %+v, %v", stale, err)
		}
	}
}

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

	if h.Header().Get("Content-Type") != "application/octet-stream" || p.Header().Get("Content-Type") != h.Header().Get("Content-Type") {
		t.Fatal("HEAD and GET must describe the same representation Content-Type")
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
