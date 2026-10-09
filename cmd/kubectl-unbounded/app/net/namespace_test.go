// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package net

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestNamespaceDoesNotProbeHistoricalNamespaces(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "")

	var paths []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"apiVersion":"v1","kind":"PodList","items":[]}`)
	}))
	defer server.Close()

	rt := newDetailTestRuntime(t, server.URL)
	*rt.configFlags.Namespace = ""

	ns, err := rt.namespace()
	if err != nil || ns != "default" {
		t.Fatalf("namespace = %q, %v", ns, err)
	}

	want := []string{"/api/v1/namespaces/default/pods", "/api/v1/namespaces/unbounded-system/pods"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("probed %v, want %v", paths, want)
	}

	paths = nil
	rt = newDetailTestRuntime(t, server.URL)
	*rt.configFlags.Namespace = "unbounded-net"

	ns, err = rt.namespace()
	if err != nil || ns != "unbounded-net" || len(paths) != 0 {
		t.Fatalf("explicit custom namespace = %q, %v, probes=%v", ns, err, paths)
	}
}
