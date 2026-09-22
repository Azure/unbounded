// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package net

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestAggregatedRouteUnavailableOnlyForMissingRoute(t *testing.T) {
	if !aggregatedRouteUnavailable(apierrors.NewNotFound(schema.GroupResource{
		Group: "status.net.unbounded-cloud.io", Resource: "nodes",
	}, "node")) {
		t.Fatal("missing aggregated route did not permit direct fallback")
	}

	for _, err := range []error{
		apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "node", errors.New("denied")),
		apierrors.NewServiceUnavailable("leadership changed"),
		errors.New("transport failed"),
	} {
		if aggregatedRouteUnavailable(err) {
			t.Fatalf("controller or transport failure permitted fallback: %v", err)
		}
	}
}

func newDetailTestRuntime(t *testing.T, serverURL string) *pluginRuntime {
	t.Helper()

	path := filepath.Join(t.TempDir(), "kubeconfig")

	data := fmt.Sprintf(`apiVersion: v1
kind: Config
current-context: test
clusters:
- name: test
  cluster:
    server: %s
contexts:
- name: test
  context:
    cluster: test
    user: test
users:
- name: test
  user:
    token: test-token
`, serverURL)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	rt := newPluginRuntime()
	*rt.configFlags.KubeConfig = path
	*rt.configFlags.Namespace = "unbounded-system"

	return rt
}

func TestStatusRequestAggregatedTransport(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		t.Run(method, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != method || r.URL.Path != "/apis/status.net.unbounded-cloud.io/v1alpha1/nodes/node-a/details" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
				}

				if r.Header.Get("Authorization") != "Bearer test-token" {
					t.Error("request did not reuse Kubernetes authentication")
				}

				if method == http.MethodPost {
					body, err := io.ReadAll(r.Body)
					if err != nil || string(body) != `{"forceRefresh":true}` || r.Header.Get("Content-Type") != "application/json" {
						t.Errorf("unexpected request body %s, %v", body, err)
					}
				} else if r.URL.Query().Get("requestId") != "id/+ ?" {
					t.Errorf("request ID changed: %q", r.URL.Query().Get("requestId"))
				}

				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_, _ = io.WriteString(w, `{"state":"pending"}`)
			}))
			defer server.Close()

			client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL, BearerToken: "test-token"})
			if err != nil {
				t.Fatal(err)
			}

			path := "/status/node/node-a/details"

			var body []byte
			if method == http.MethodPost {
				body = []byte(`{"forceRefresh":true}`)
			} else {
				path += "?requestId=id%2F%2B+%3F"
			}

			raw, err := requestStatusViaAggregatedAPI(context.Background(), client, method, path, body)
			if err != nil || string(raw) != `{"state":"pending"}` {
				t.Fatalf("request = %s, %v", raw, err)
			}
		})
	}
}

func TestStatusRequestPreservesHTTPFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"state":"unavailable","error":"leadership changed"}`)
	}))
	defer server.Close()

	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}

	raw, err := requestStatusViaAggregatedAPI(context.Background(), client, http.MethodGet, "/status/node/node-a/details", nil)
	if err == nil || !strings.Contains(string(raw), "leadership changed") {
		t.Fatalf("lost explicit controller failure: %s, %v", raw, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := requestStatusViaAggregatedAPI(ctx, client, http.MethodGet, "/status/node/node-a/details", nil); err == nil {
		t.Fatal("canceled request succeeded")
	}

	for _, path := range []string{"https://other.invalid/status", "://invalid"} {
		if _, err := requestStatusViaAggregatedAPI(context.Background(), client, http.MethodGet, path, nil); err == nil {
			t.Errorf("accepted invalid path %q", path)
		}
	}

	request, err := newStatusRequest(newDetailTestRuntime(t, server.URL), defaultNodeStatusFetchOptions())
	if err != nil {
		t.Fatal(err)
	}

	raw, err = request(context.Background(), http.MethodGet, "/status/node/node-a/details", nil)
	if err == nil || !strings.Contains(string(raw), "leadership changed") {
		t.Fatalf("fallback hid controller failure: %s, %v", raw, err)
	}
}
