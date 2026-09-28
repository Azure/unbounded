// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func retentionHealth(t *testing.T, ip string, port int) *healthState {
	t.Helper()

	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node", UID: "uid"},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}},
		},
	}
	if err := indexer.Add(node); err != nil {
		t.Fatal(err)
	}

	health := &healthState{
		clientset: k8sfake.NewClientset(), statusCache: NewNodeStatusCache(),
		nodeLister: corev1listers.NewNodeLister(indexer), nodeAgentHealthPort: port,
		siteInformer:   cache.NewSharedIndexInformer(&cache.ListWatch{}, &unstructured.Unstructured{}, 0, cache.Indexers{}),
		staleThreshold: time.Minute,
	}
	health.isLeader.Store(true)

	return health
}

func TestClusterRetentionAndBulkSummary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		health := retentionHealth(t, "127.0.0.1", 0)
		manager := testDetailRequests(t, nodeDetailRequestHooks{})
		health.statusCache.BindDetails(manager)

		full := retentionFixture(10000)
		health.statusCache.StoreFull("node", full, "ws")
		health.clusterStatusCache = NewClusterStatusCache(health)
		health.clusterStatusCache.Rebuild(t.Context())

		for _, patch := range []bool{false, true} {
			if patch {
				health.clusterStatusCache.PatchNode("node", full)
			}

			snapshot := health.clusterStatusCache.Get()
			if len(snapshot.Nodes) != 1 || snapshot.NodeOverviews["node"].PeerCount != 10000 {
				t.Fatal("cluster snapshot lost observed facts")
			}

			node := snapshot.Nodes[0]
			if len(node.Peers) != 0 || len(node.RoutingTable.Routes) != 0 || len(node.BpfEntries) != 0 {
				t.Fatal("cluster cache retained heavy details")
			}

			recorder := httptest.NewRecorder()
			serveStatusJSON(health, recorder, httptest.NewRequest(http.MethodGet, "/status/json", nil))

			var body map[string]json.RawMessage
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}

			if recorder.Code != http.StatusOK || body["nodeSummaries"] == nil || body["nodes"] != nil ||
				strings.Contains(recorder.Body.String(), "private-detail-marker") || strings.Contains(recorder.Body.String(), "routeDistances") {
				t.Fatal("bulk JSON exposed detailed payloads")
			}
		}

		time.Sleep(manager.cache.ttl)
		synctest.Wait()
		assertNodeDetailEntries(t, manager.cache, 0)
		assertThinStatus(t, health.statusCache.entries["node"], 10000)

		if len(health.clusterStatusCache.Get().Nodes[0].Peers) != 0 {
			t.Fatal("cluster snapshot still owns expired peer data")
		}
	})
}

func TestBackgroundPullUsesSummaryOnly(t *testing.T) {
	for _, mode := range []string{"summary", "unsupported", "legacy-full", "wrong-node", "invalid-counts", "negative-mismatches", "negative-links", "inconsistent-mismatches"} {
		t.Run(mode, func(t *testing.T) {
			var summaryCalls, fullCalls atomic.Int32

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/status/summary" {
					fullCalls.Add(1)
					http.Error(w, "full pull forbidden", http.StatusBadRequest)

					return
				}

				summaryCalls.Add(1)

				if mode == "unsupported" {
					http.NotFound(w, r)

					return
				}

				if mode == "legacy-full" {
					json.NewEncoder(w).Encode(retentionFixture(3))

					return
				}

				overview := statusv1alpha1.NodeStatusOverview{
					NodeInfo: NodeInfo{Name: "node"}, PeerCount: 17, HealthyPeers: 15, RouteCount: 33, RouteMismatch: true,
					RouteMismatchCount: 2, UnhealthyPeerLinks: 3, UsesIPIP: true,
				}
				if mode == "wrong-node" {
					overview.NodeInfo.Name = "other"
				}

				if mode == "invalid-counts" {
					overview.HealthyPeers = 100
				}

				if mode == "negative-mismatches" {
					overview.RouteMismatchCount = -1
				}

				if mode == "negative-links" {
					overview.UnhealthyPeerLinks = -1
				}

				if mode == "inconsistent-mismatches" {
					overview.RouteMismatch = false
				}

				json.NewEncoder(w).Encode(overview)
			}))
			defer server.Close()

			host, portText, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}

			port, err := strconv.Atoi(portText)
			if err != nil {
				t.Fatal(err)
			}

			health := retentionHealth(t, host, port)
			if _, err := health.statusCache.StoreOverview("node", statusv1alpha1.NodeStatusOverview{
				NodeInfo: NodeInfo{Name: "node"}, PeerCount: 7, HealthyPeers: 7,
			}, "ws"); err != nil {
				t.Fatal(err)
			}

			health.statusCache.entries["node"].ReceivedAt = time.Now().Add(-time.Hour)

			status := fetchClusterStatus(t.Context(), health, true)
			if summaryCalls.Load() != 1 || fullCalls.Load() != 0 {
				t.Fatal("background pull did not use only the summary endpoint")
			}

			if mode == "summary" {
				overview := status.NodeOverviews["node"]
				if overview.PeerCount != 17 || overview.RouteCount != 33 || !overview.RouteMismatch ||
					overview.RouteMismatchCount != 2 || overview.UnhealthyPeerLinks != 3 || !overview.UsesIPIP || status.Nodes[0].FetchError != "" {
					t.Fatal("summary pull lost observed facts")
				}
			} else if status.NodeOverviews["node"].PeerCount != 7 || status.Nodes[0].FetchError == "" {
				t.Fatal("failed summary pull discarded stale facts or concealed the failure")
			}

			if len(status.Nodes[0].Peers) != 0 || len(status.Nodes[0].RoutingTable.Routes) != 0 {
				t.Fatal("background pull retained detailed arrays")
			}
		})
	}
}

func BenchmarkClusterSummaryRetainedDetails(b *testing.B) {
	for _, peerCount := range []int{1, 10000} {
		b.Run(fmt.Sprint(peerCount), func(b *testing.B) {
			cache := NewClusterStatusCache(&healthState{})
			cache.status = &ClusterStatusResponse{}
			cache.PatchNode("node", retentionFixture(peerCount))

			if _, err := json.Marshal(buildClusterSummary(cache.Get())); err != nil {
				b.Fatal(err)
			}

			b.ReportAllocs()

			for b.Loop() {
				data, err := json.Marshal(buildClusterSummary(cache.Get()))
				if err != nil {
					b.Fatal(err)
				}

				b.ReportMetric(float64(len(data)), "wire-bytes")
			}
		})
	}
}
