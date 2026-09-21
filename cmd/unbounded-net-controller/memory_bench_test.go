// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"

	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
)

func benchmarkProtoStatus(b *testing.B, peerCount int) []byte {
	b.Helper()

	status := &statusproto.NodeStatusFull{
		NodeInfo: &statusproto.NodeInfo{Name: "node-a", SiteName: "site-a"},
	}

	for i := range peerCount {
		name := fmt.Sprintf("peer-%d", i)
		status.Peers = append(status.Peers, &statusproto.PeerStatus{
			Name: name, PeerType: "site", SiteName: "site-a",
			Tunnel: &statusproto.PeerTunnelStatus{
				Interface: "wg0", PublicKey: name,
				Endpoint: "10.224.0.1:51820", AllowedIps: []string{"10.244.0.0/24"},
			},
			HealthCheck: &statusproto.HealthCheckPeerStatus{Enabled: true, Status: "up"},
		})
		status.BpfEntries = append(status.BpfEntries, &statusproto.BpfEntry{
			Cidr: "10.244.0.0/24", Remote: "10.224.0.1", Node: name,
			InterfaceName: "wg0", Protocol: "WireGuard",
		})
	}

	data, err := proto.Marshal(&statusproto.NodeStatusMessage{
		Type: "node_status_full", NodeName: "node-a", Status: status,
	})
	if err != nil {
		b.Fatal(err)
	}

	return data
}

func BenchmarkProtoWSStatusFrame(b *testing.B) {
	for _, peerCount := range []int{100, 2000} {
		b.Run(fmt.Sprintf("peers-%d", peerCount), func(b *testing.B) {
			data := benchmarkProtoStatus(b, peerCount)
			health := &healthState{statusCache: NewNodeStatusCache()}

			b.ReportAllocs()
			b.ResetTimer()

			for b.Loop() {
				decoded, err := decodeProtoWSMessage(data)
				if err != nil || decoded.nodeName != "node-a" {
					b.Fatalf("unexpected decoded message: %+v, err=%v", decoded, err)
				}

				_, ack := handleProtoWSMessage(health, decoded, "ws")
				if ack.Status != "ok" {
					b.Fatalf("unexpected ack: %+v", ack)
				}
			}
		})
	}
}

func matrixBenchmarkNodes(count int) []*NodeStatusResponse {
	peers := make([]WireGuardPeerStatus, count)

	nodes := make([]*NodeStatusResponse, count)
	for i := range count {
		name := fmt.Sprintf("node-%d", i)
		peers[i] = WireGuardPeerStatus{Name: name, PeerType: "site", SiteName: "site-a"}
		nodes[i] = &NodeStatusResponse{
			NodeInfo: NodeInfo{Name: name, SiteName: "site-a"},
			Peers:    peers,
		}
	}

	return nodes
}

func BenchmarkBuildConnectivityMatrix(b *testing.B) {
	for _, count := range []int{100, 101, 2000} {
		b.Run(fmt.Sprintf("nodes-%d", count), func(b *testing.B) {
			nodes := matrixBenchmarkNodes(count)

			b.ReportAllocs()
			b.ResetTimer()

			for b.Loop() {
				matrix := buildConnectivityMatrix(nodes, nil)
				if count > 100 && matrix != nil {
					b.Fatal("oversized site produced a matrix")
				}
			}
		})
	}
}
