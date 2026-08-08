// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "application/test" {
			t.Errorf("Accept = %q, want application/test", got)
		}

		_, _ = io.WriteString(w, "payload") //nolint:errcheck // The fetched body is asserted below.
	}))
	defer server.Close()

	var output bytes.Buffer
	if err := fetchURL(context.Background(), server.Client(), server.URL, "application/test", &output); err != nil {
		t.Fatalf("fetchURL: %v", err)
	}

	if got := output.String(); got != "payload" {
		t.Fatalf("body = %q, want payload", got)
	}
}

func TestFetchURLRejectsFailureStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	if err := fetchURL(context.Background(), server.Client(), server.URL, "", io.Discard); err == nil {
		t.Fatal("fetchURL accepted HTTP 502")
	}
}
