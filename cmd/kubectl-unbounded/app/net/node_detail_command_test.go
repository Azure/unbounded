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
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func TestNodeShowRequestsNamedDetails(t *testing.T) {
	for _, mode := range []string{"", "peer", "peers", "route", "routes", "bpf", "json"} {
		for _, refresh := range []bool{false, true} {
			t.Run(mode, func(t *testing.T) {
				result := detailResultFixture()
				result.Details.Status.RoutingTable.Routes = []statusv1alpha1.RouteEntry{{
					Destination: "10.20.0.0/24", NextHops: []statusv1alpha1.NextHop{{Gateway: "10.1.0.1"}},
				}}
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

				want := map[string]string{"peer": "peer", "peers": "peer", "route": "10.20.0.0/24", "routes": "10.20.0.0/24", "bpf": "10.30.0.0/24"}[mode]
				if want != "" && !strings.Contains(out.String(), want) {
					t.Fatalf("diagnostic content lost: %s", out.String())
				}

				if mode == "json" {
					var snapshot statusv1alpha1.NodeStatusResponse
					if err := json.Unmarshal(out.Bytes(), &snapshot); err != nil || !reflect.DeepEqual(&snapshot, result.Details.Status) {
						t.Fatalf("raw diagnostic content changed: %s, %v", out.String(), err)
					}

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

func TestNodeShowOverviewConsistency(t *testing.T) {
	for _, tc := range []struct {
		name, ready, cni         string
		old, missingInfo, legacy bool
	}{
		{name: "unenriched diagnostics", ready: "Ready", cni: "Healthy"},
		{name: "older diagnostics", ready: "Ready", cni: "Healthy", old: true},
		{name: "stale", ready: "NotReady", cni: "Stale"},
		{name: "no data", ready: "NotReady", cni: "No data"},
		{name: "fallback", ready: "Ready", cni: "Fallback"},
		{name: "errors", ready: "Ready", cni: "Errors"},
		{name: "route mismatch", ready: "Ready", cni: "Route mismatch"},
		{name: "missing metadata", ready: "Ready", cni: "Healthy", missingInfo: true, old: true},
		{name: "missing health", missingInfo: true, old: true},
		{name: "legacy overview", ready: "Ready", cni: "Healthy", legacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := detailResultFixture()
			now := result.Details.ReceivedAt
			info := &statusv1alpha1.NodeInfo{
				Name: "node-a", K8sReady: tc.ready, SiteName: "site",
				OSImage: "Ubuntu 24.04.4", Kernel: "6.8.0-1067-azure", Kubelet: "v1.35.7",
				InternalIPs: []string{"10.224.0.103"}, PodCIDRs: []string{"10.20.0.0/24"},
				K8sUpdatedAt: &now, BuildInfo: &statusv1alpha1.BuildInfo{Commit: "current-build"},
				WireGuard: &statusv1alpha1.WireGuardStatusInfo{PublicKey: "current-key"},
				K8sLabels: map[string]string{
					"node.kubernetes.io/instance-type": "Standard_D2ads_v6",
					"topology.kubernetes.io/region":    "canadacentral",
					"topology.kubernetes.io/zone":      "canadacentral-1",
				},
			}

			entry := nodeSummary{
				Name: "node-a", SiteName: "site", K8sReady: tc.ready, CniStatus: tc.cni,
				StatusSource: "ws", NodeInfo: info, LastPushTime: &now,
			}
			if tc.cni == "Errors" {
				entry.ErrorCount, entry.FirstError, entry.FetchError = 2, "current error", "current fetch error"
			}

			if tc.missingInfo {
				entry.NodeInfo, entry.LastPushTime = nil, nil
			}

			if tc.old {
				result.Details.Status.NodeInfo = statusv1alpha1.NodeInfo{
					Name: "node-a", K8sReady: "NotReady", OSImage: "old-image",
					Kernel: "old-kernel", Kubelet: "old-kubelet", InternalIPs: []string{"10.0.0.1"},
					K8sLabels: map[string]string{"topology.kubernetes.io/region": "old-region"},
				}
				result.Details.Status.StatusSource = "stale-cache"
				result.Details.Status.NodeErrors = []statusv1alpha1.NodeError{{Message: "old diagnostic error"}}
			}

			var overview any = clusterSummary{NodeSummaries: []nodeSummary{entry}}
			if tc.legacy {
				overview = clusterStatusResponse{Nodes: []statusv1alpha1.NodeStatusResponse{entry.statusMetadata()}}
			}

			var summaries, details atomic.Int32

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")

				switch {
				case strings.HasSuffix(r.URL.Path, "/status/json") && r.Method == http.MethodGet:
					summaries.Add(1)

					_ = json.NewEncoder(w).Encode(overview)
				case strings.HasSuffix(r.URL.Path, "/nodes/node-a/details") && r.Method == http.MethodPost:
					details.Add(1)

					_ = json.NewEncoder(w).Encode(result)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			execute := func(args ...string) string {
				t.Helper()
				cmd := newNodeRootCommand(newDetailTestRuntime(t, server.URL))

				var out bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetErr(io.Discard)
				cmd.SetArgs(args)

				if err := cmd.Execute(); err != nil {
					t.Fatal(err)
				}

				return out.String()
			}

			var rows []nodeListRow
			if err := json.Unmarshal([]byte(execute("list", "-o", "json", "--suppress-warnings")), &rows); err != nil || len(rows) != 1 {
				t.Fatalf("list rows: %+v, %v", rows, err)
			}

			output := execute("show", "node-a", "--color=never")
			fields := map[string]string{}

			for _, line := range strings.Split(output, "\n") {
				if key, value, ok := strings.Cut(line, "  "); ok {
					fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
				}
			}

			if fields["K8s Status"] != rows[0].K8sStatus || fields["UN Status"] != rows[0].WGStatus ||
				fields["K8s Status"] != valueOr(tc.ready, "Unknown") || fields["UN Status"] != valueOr(tc.cni, "Unknown") {
				t.Fatalf("list/show health disagreement: %+v\n%s", rows, output)
			}

			want := map[string]string{
				"Node Image": "Ubuntu 24.04.4", "Kernel": "6.8.0-1067-azure", "Kubelet Version": "v1.35.7",
				"Instance Type": "Standard_D2ads_v6", "Region": "canadacentral", "Availability Zone": "canadacentral-1",
				"Internal IPs": "10.224.0.103", "Pod CIDRs": "10.20.0.0/24", "WireGuard Public Key": "current-key",
				"Node Agent Build": "Commit: current-build",
			}
			for key, value := range want {
				if tc.missingInfo {
					value = "-"
				}

				if fields[key] != value {
					t.Errorf("%s = %q, want %q", key, fields[key], value)
				}
			}

			for _, key := range []string{"K8s Node Updated", "Status Push Updated"} {
				if (fields[key] == "Never") != tc.missingInfo || fields[key] == "" {
					t.Errorf("%s did not use summary timestamp: %q", key, fields[key])
				}
			}

			if tc.cni == "Errors" && (fields["Node Error Count"] != "2" || fields["First Node Error"] != "current error" ||
				fields["Fetch Error"] != "current fetch error") {
				t.Fatalf("current errors lost: %s", output)
			}

			if strings.Contains(output, "old") || summaries.Load() != 2 || details.Load() != 1 {
				t.Fatalf("stale metadata or extra requests: summaries=%d details=%d\n%s", summaries.Load(), details.Load(), output)
			}
		})
	}
}

func TestNodeShowOverviewFailures(t *testing.T) {
	for _, tc := range []struct {
		body, want string
		status     int
	}{
		{body: `{"nodeSummaries":[]}`, want: "not found in cluster overview"},
		{body: `{}`, want: "missing nodeSummaries or nodes"},
		{body: `{`, want: "decode cluster overview"},
		{body: "forbidden", status: http.StatusForbidden, want: "failed"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/details") {
					t.Error("overview failure requested diagnostics")
				}

				w.Header().Set("Content-Type", "application/json")

				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}

				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()

			cmd := newNodeRootCommand(newDetailTestRuntime(t, server.URL))

			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(io.Discard)
			cmd.SetArgs([]string{"show", "node-a", "--color=never"})

			cmd.SilenceUsage = true
			if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), tc.want) || out.Len() != 0 {
				t.Fatalf("overview failure not surfaced: %v, %s", err, out.String())
			}
		})
	}
}

func TestNodeShowRejectsExpiredDiagnostics(t *testing.T) {
	result := detailResultFixture()
	result.Details.ReceivedAt = result.Details.ReceivedAt.Add(-2 * time.Minute)
	result.Details.ExpiresAt = result.Details.ExpiresAt.Add(-2 * time.Minute)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/nodes/node-a/details") || r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	}))
	defer server.Close()

	cmd := newNodeRootCommand(newDetailTestRuntime(t, server.URL))
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"show", "node-a", "json"})

	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired diagnostics rendered: %v", err)
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
