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
		_, _ = w.Write([]byte("# HELP node_exporter_build_info build info\nnode_exporter_build_info{version=\"1.9.1\"} 1\n"))
	}))
	defer server.Close()

	if err := nodeExporterReady(t.Context(), server.Client(), server.URL); err != nil {
		t.Fatalf("nodeExporterReady() error = %v", err)
	}
}

func TestNodeExporterReadyRejectsMissingMetric(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("node_cpu_seconds_total 1\n"))
	}))
	defer server.Close()

	if err := nodeExporterReady(t.Context(), server.Client(), server.URL); err == nil {
		t.Fatal("nodeExporterReady() error = nil")
	}
}
