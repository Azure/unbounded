// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package net

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

const legacyOverviewFixture = `{
	"seq":7,"pullEnabled":false,"leaderInfo":{"podName":"leader"},
	"sites":[{"name":"site","manageCniPlugin":true}],
	"gatewayPools":[{"name":"pool","gateways":["node-a"]}],
	"warnings":["controller warning"],
	"nodes":[{
		"nodeInfo":{"name":"node-a","siteName":"site","isGateway":true,"k8sReady":"Ready"},
		"statusSource":"ws","nodeErrors":[{"type":"cni","message":"not ready"}],
		"peers":[{"name":"peer","healthCheck":{"enabled":true,"status":"up"}},{"name":"down"}],
		"routingTable":{"routes":[{"nextHops":[{"expected":true,"present":false}]}]},
		"bpfEntries":[{"cidr":"10.0.0.0/24","node":"secret-detail"}]
	}]
}`

func TestDecodeClusterSummaryCompatibility(t *testing.T) {
	summary, err := decodeClusterSummary([]byte(legacyOverviewFixture))
	if err != nil {
		t.Fatal(err)
	}

	if summary.Seq != 7 || summary.LeaderInfo.PodName != "leader" || len(summary.NodeSummaries) != 1 {
		t.Fatalf("missing cluster metadata: %+v", summary)
	}

	node := summary.NodeSummaries[0]
	if node.PeerCount != 2 || node.HealthyPeers != 1 || node.RouteCount != 1 || !node.RouteMismatch ||
		node.ErrorCount != 1 || node.FirstError != "not ready" || node.Name != "node-a" {
		t.Fatalf("overview facts changed: %+v", node)
	}

	raw, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}

	for _, forbidden := range []string{`"nodes"`, `"peers"`, `"routingTable"`, `"bpfEntries"`, "secret-detail", "10.0.0.0/24"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("retained detail %q in summary: %s", forbidden, raw)
		}
	}

	got, err := decodeClusterSummary(raw)
	if err != nil || !reflect.DeepEqual(got.NodeSummaries[0], node) {
		t.Fatalf("summary round trip changed: %+v, %v", got, err)
	}

	for _, raw := range []string{`{`, `{}`, `null`, `{"nodeSummaries":"bad"}`, `{"nodes":"bad"}`} {
		if _, err := decodeClusterSummary([]byte(raw)); err == nil {
			t.Errorf("accepted malformed response %s", raw)
		}
	}

	if got, err := decodeClusterSummary([]byte(`{"nodeSummaries":[],"nodes":"ignored legacy field"}`)); err != nil || len(got.NodeSummaries) != 0 {
		t.Fatalf("summary must take precedence: %+v, %v", got, err)
	}
}

func TestMergeClusterSummaryDelta(t *testing.T) {
	summary := clusterSummary{
		Seq: 1, PullEnabled: true, NodeCount: 2,
		NodeSummaries: []nodeSummary{
			{Name: "keep", PeerCount: 3, HealthyPeers: 2},
			{Name: "remove", PeerCount: 4},
		},
		Warnings: []string{"old warning"},
	}

	raw := []byte(`{
							"seq":2,"nodeCount":2,"pullEnabled":false,"warnings":[],
							"removedNodes":["remove"],
							"nodeSummaries":[{"name":"new","peerCount":7,"healthyPeers":5}]
						}`)
	if err := mergeClusterSummaryDelta(&summary, raw); err != nil {
		t.Fatal(err)
	}

	if summary.Seq != 2 || summary.PullEnabled || len(summary.Warnings) != 0 || len(summary.NodeSummaries) != 2 {
		t.Fatalf("delta metadata not applied: %+v", summary)
	}

	nodes := make(map[string]nodeSummary)
	for _, node := range summary.NodeSummaries {
		nodes[node.Name] = node
	}

	if nodes["keep"].PeerCount != 3 || nodes["new"].HealthyPeers != 5 {
		t.Fatalf("delta lost existing/updated facts: %+v", nodes)
	}

	if _, ok := nodes["remove"]; ok {
		t.Fatal("removed node still retained")
	}

	if err := mergeClusterSummaryDelta(&summary, []byte(`{"nodeSummaries":[{"name":"keep","peerCount":0}]}`)); err != nil {
		t.Fatal(err)
	}

	for _, node := range summary.NodeSummaries {
		if node.Name == "keep" && (node.PeerCount != 0 || node.HealthyPeers != 0) {
			t.Fatalf("zero-valued update not applied: %+v", node)
		}
	}

	for _, raw := range []string{`null`, `{`, `{"nodeSummaries":"bad"}`, `{"pullEnabled":"bad"}`} {
		if err := mergeClusterSummaryDelta(&summary, []byte(raw)); err == nil {
			t.Errorf("accepted malformed summary delta %s", raw)
		}
	}
}

func TestNodeListUsesOnlyOverview(t *testing.T) {
	for _, fixture := range []string{
		legacyOverviewFixture,
		`{"nodeSummaries":[{"name":"node-a","peerCount":2,"healthyPeers":1}],"sites":[],"gatewayPools":[]}`,
	} {
		for _, output := range []string{"json", "table", "wide"} {
			t.Run(output, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet || r.URL.Path != "/apis/status.net.unbounded-cloud.io/v1alpha1/status/json" {
						t.Errorf("list requested non-overview endpoint: %s %s", r.Method, r.URL)
						http.Error(w, "unexpected request", http.StatusBadRequest)

						return
					}

					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, fixture)
				}))
				defer server.Close()

				cmd := newNodeRootCommand(newDetailTestRuntime(t, server.URL))

				var out bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetErr(io.Discard)
				cmd.SetArgs([]string{"list", "-o", output, "--color=never", "--suppress-warnings"})

				if err := cmd.Execute(); err != nil {
					t.Fatal(err)
				}

				if !strings.Contains(out.String(), "node-a") || !strings.Contains(out.String(), "1/2") {
					t.Fatalf("missing summary row: %s", out.String())
				}

				if strings.Contains(out.String(), "secret-detail") {
					t.Fatal("list leaked details")
				}
			})
		}
	}
}
