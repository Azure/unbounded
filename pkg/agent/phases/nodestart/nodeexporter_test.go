// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodestart

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNodeExporterReady(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := nodeExporterReady(t.Context(), server.Client(), server.URL); err != nil {
		t.Fatalf("nodeExporterReady() error = %v", err)
	}
}

func TestNodeExporterReadyRejectsErrorStatus(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	if err := nodeExporterReady(t.Context(), server.Client(), server.URL); err == nil {
		t.Fatal("nodeExporterReady() error = nil")
	}
}
