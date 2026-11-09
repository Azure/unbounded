// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package transfer

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWithExtraRoutes(t *testing.T) {
	s := New(nil, WithExtraRoutes(func(mux *http.ServeMux) {
		mux.HandleFunc("/cachemesh/chunk", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "chunk")
		})
	}))

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	for path, want := range map[string]int{
		"/cachemesh/chunk": http.StatusOK,
		"/healthz":         http.StatusOK,
		"/cachemesh/other": http.StatusNotFound,
	} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}

		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode != want {
			t.Errorf("%s: status %d, want %d", path, resp.StatusCode, want)
		}

		if path == "/cachemesh/chunk" && string(body) != "chunk" {
			t.Errorf("extra route body %q", body)
		}
	}
}

func TestWithoutExtraRoutesUnchanged(t *testing.T) {
	ts := httptest.NewServer(New(nil, WithExtraRoutes(nil)).Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/cachemesh/chunk")
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404 without extra routes", resp.StatusCode)
	}
}
