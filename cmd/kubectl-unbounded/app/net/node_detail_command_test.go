// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package net

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"

	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func TestNodeShowRequestsNamedDetails(t *testing.T) {
	for _, mode := range []string{"", "peer", "peers", "route", "routes", "bpf", "json"} {
		for _, refresh := range []bool{false, true} {
			t.Run(mode, func(t *testing.T) {
				result := detailResultFixture()
				result.Details.Status.RoutingTable.Routes = []statusv1alpha1.RouteEntry{{Destination: "10.20.0.0/24"}}
				result.Details.Status.BpfEntries = []statusv1alpha1.BpfEntry{{CIDR: "10.30.0.0/24", Node: "peer"}}

				var posts atomic.Int32

				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")

					switch r.URL.Path {
					case "/apis/status.net.unbounded-cloud.io/v1alpha1/status/json":
						if mode != "" && mode != "peer" && mode != "peers" {
							t.Error("detail-only output fetched cluster status unnecessarily")
						}

						if r.Method != http.MethodGet {
							t.Errorf("metadata request method %s", r.Method)
						}

						_, _ = io.WriteString(w, `{"nodeSummaries":[{"name":"node-a"},{"name":"peer","siteName":"site","k8sReady":"Ready"}]}`)
					case "/apis/status.net.unbounded-cloud.io/v1alpha1/nodes/node-a/details":
						if r.Method != http.MethodPost {
							t.Errorf("cached show must POST once, got %s", r.Method)
						}

						posts.Add(1)

						var body struct {
							Refresh *bool `json:"forceRefresh"`
						}
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Refresh == nil || *body.Refresh != refresh {
							t.Errorf("refresh = %+v, %v; want %v", body, err, refresh)
						}

						if err := json.NewEncoder(w).Encode(result); err != nil {
							t.Error(err)
						}
					default:
						t.Errorf("unexpected endpoint %s", r.URL.Path)
						http.NotFound(w, r)
					}
				}))
				defer server.Close()

				cmd := newNodeRootCommand(newDetailTestRuntime(t, server.URL))

				args := []string{"show", "node-a", "--color=never"}
				if mode != "" {
					args = append(args, mode)
				}

				if refresh {
					args = append(args, "--refresh")
				}

				var out bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetErr(io.Discard)
				cmd.SetArgs(args)

				if err := cmd.Execute(); err != nil {
					t.Fatal(err)
				}

				if posts.Load() != 1 || out.Len() == 0 {
					t.Fatalf("show did not load/render exactly one named snapshot: calls=%d, output=%s", posts.Load(), out.String())
				}

				if mode == "json" {
					var decoded map[string]json.RawMessage
					if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
						t.Fatal(err)
					}

					if decoded["nodeInfo"] == nil || decoded["peers"] == nil || decoded["routingTable"] == nil ||
						decoded["bpfEntries"] == nil || decoded["details"] != nil || decoded["state"] != nil {
						t.Fatalf("raw node JSON shape changed: %s", out.String())
					}
				}
			})
		}
	}
}

func TestNodeShowFailureDoesNotFallBackToFullStatus(t *testing.T) {
	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)

		if !strings.HasSuffix(r.URL.Path, "/nodes/node-a/details") || r.Method != http.MethodPost {
			t.Errorf("unexpected fallback request: %s %s", r.Method, r.URL)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"state":"unavailable","nodeName":"node-a","error":"node is unreachable"}`)
	}))
	defer server.Close()

	cmd := newNodeRootCommand(newDetailTestRuntime(t, server.URL))

	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"show", "node-a", "json"})

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "unreachable") || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("explicit diagnostic failure not surfaced: %v", err)
	}

	if calls.Load() != 1 {
		t.Errorf("failure triggered fallback requests: %d", calls.Load())
	}
}

func TestNodePeerCompletionDoesNotLoadDiagnostics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/nodes" {
			t.Errorf("completion requested diagnostic endpoint: %s %s", r.Method, r.URL)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"apiVersion":"v1","kind":"NodeList","items":[{"metadata":{"name":"node-a"}},{"metadata":{"name":"node-b"}}]}`)
	}))
	defer server.Close()

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	names, err := listNodePeeringsForCompletion(newDetailTestRuntime(t, server.URL), cmd,
		defaultNodeStatusFetchOptions(), "node-a", "node")
	if err != nil || len(names) != 1 || names[0] != "node-b" {
		t.Fatalf("completion names = %v, %v", names, err)
	}
}

func TestNodeShowRejectsWatchAndInvalidModeWithoutRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("invalid/watch command requested %s", r.URL)
	}))
	defer server.Close()

	for _, args := range [][]string{
		{"show", "node-a", "json", "--watch"},
		{"show", "node-a", "unknown"},
	} {
		cmd := newNodeRootCommand(newDetailTestRuntime(t, server.URL))
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(args)

		if err := cmd.Execute(); err == nil {
			t.Errorf("accepted invalid/watch invocation %v", args)
		}
	}
}
