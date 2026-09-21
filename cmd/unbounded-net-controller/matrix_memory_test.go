// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"
)

func TestConnectivityMatrixMixedScopesPreservesPeers(t *testing.T) {
	nodes := make([]*NodeStatusResponse, 0, 102)
	for i := range 101 {
		nodes = append(nodes, &NodeStatusResponse{NodeInfo: NodeInfo{
			Name: fmt.Sprintf("node-%d", i), SiteName: "large",
		}})
	}

	nodes[0].Peers = []WireGuardPeerStatus{{
		Name: "gateway", PeerType: "gateway", SiteName: "small",
		Tunnel: PeerTunnelStatus{LastHandshake: time.Unix(1, 0)},
	}}
	nodes[1].Peers = []WireGuardPeerStatus{{Name: "gateway", PeerType: "ignored"}}
	nodes = append(nodes, &NodeStatusResponse{
		NodeInfo: NodeInfo{Name: "gateway", SiteName: "small"},
		Peers: []WireGuardPeerStatus{
			{Name: "node-0", PeerType: "site", HealthCheck: &HealthCheckPeerStatus{Status: "up"}},
			{Name: "node-0", PeerType: "ignored", HealthCheck: &HealthCheckPeerStatus{Status: "down"}},
			{Name: "node-2", PeerType: "ignored"},
			{Name: "gateway", PeerType: "ignored", HealthCheck: &HealthCheckPeerStatus{Status: "down"}},
		},
	})

	before, err := json.Marshal(nodes)
	if err != nil {
		t.Fatal(err)
	}

	matrix := buildConnectivityMatrix(nodes, []GatewayPoolStatus{{Name: "pool", Gateways: []string{"gateway"}}})
	if _, ok := matrix["large"]; ok {
		t.Fatal("oversized site produced a matrix")
	}

	if matrix["small"] == nil || !slices.Equal(matrix["small"].Nodes, []string{"gateway"}) {
		t.Fatalf("small site lost its matrix: %+v", matrix["small"])
	}

	pool := matrix["pool:pool"]
	if pool == nil || !slices.Equal(pool.Nodes, []string{"gateway", "node-0"}) {
		t.Fatalf("small pool crossing a large site has wrong membership: %+v", pool)
	}

	if pool.Results["gateway"]["node-0"] != "up" || pool.Results["node-0"]["gateway"] != "up" {
		t.Fatalf("pool connectivity changed: %+v", pool.Results)
	}

	after, err := json.Marshal(nodes)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(before, after) {
		t.Fatal("matrix construction mutated the shared node snapshots")
	}
}

func TestConnectivityMatrixDoesNotCopyPeerSlices(t *testing.T) {
	nodes := matrixBenchmarkNodes(200)

	peers := matrixBenchmarkNodes(2000)[0].Peers
	for _, node := range nodes {
		node.Peers = peers
	}

	var matrix map[string]*SiteMatrix

	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			matrix = buildConnectivityMatrix(nodes, nil)
		}
	})

	if matrix != nil {
		t.Fatal("oversized site produced a matrix")
	}
	// Allow map bookkeeping, but not storage proportional to every peer.
	if allocated := result.AllocedBytesPerOp(); allocated > 512*1024 {
		t.Fatalf("matrix copied peer data: %d bytes per call", allocated)
	}
}

func TestConnectivityMatrixSizeBoundary(t *testing.T) {
	for _, count := range []int{0, 100, 101} {
		t.Run(fmt.Sprintf("nodes-%d", count), func(t *testing.T) {
			matrix := buildConnectivityMatrix(matrixBenchmarkNodes(count), nil)
			if count != 100 {
				if matrix != nil {
					t.Fatal("empty or oversized site produced a matrix")
				}

				return
			}

			if matrix["site-a"] == nil || len(matrix["site-a"].Nodes) != count {
				t.Fatal("site at the size limit lost its matrix")
			}
		})
	}
}
