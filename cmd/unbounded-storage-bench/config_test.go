// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"strings"
	"testing"
)

func TestRenderBlockDiskConfigUsesCacheAndFileDisk(t *testing.T) {
	nodes := []nodeSpec{{
		ID:          1,
		FabricAddr:  "10.0.0.1:7001",
		ListenAddr:  "0.0.0.0:7001",
		MetricsAddr: "127.0.0.1:9100",
		DiskPath:    "/var/tmp/disk.img",
	}}

	body, err := renderConfig(scenarioConfig{
		Scenario:       scenarioBlockDisk,
		Node:           nodes[0],
		Nodes:          nodes,
		Workers:        3,
		ObjectSize:     64 * 1024 * 1024,
		ObjectCount:    100,
		ReadBytes:      4096,
		StripeSize:     4 * 1024 * 1024,
		DiskSize:       8 * 1024 * 1024,
		MemoryBytes:    128 * 1024 * 1024,
		ServingCores:   2,
		FabricProvider: "tcp",
	})
	if err != nil {
		t.Fatalf("renderConfig: %v", err)
	}

	mustContain(t, body, `[[caches]]`)
	mustContain(t, body, `source = "cache"`)
	mustContain(t, body, `[caches.disks.config.file]`)
	mustContain(t, body, `path = "/var/tmp/disk.img"`)
	mustContain(t, body, `addr = "0.0.0.0:7001"`)
	mustContain(t, body, `disable_rdma = true`)
	mustNotContain(t, body, `[[neighborhoods]]`)
}

func TestRenderIntegratedConfigUsesPeersAndRoutingPlan(t *testing.T) {
	nodes := []nodeSpec{
		{ID: 1, FabricAddr: "10.0.0.1:7001", MetricsAddr: "127.0.0.1:9100", DiskPath: "/tmp/a.disk"},
		{ID: 2, FabricAddr: "10.0.0.2:7001", MetricsAddr: "127.0.0.1:9100", DiskPath: "/tmp/b.disk"},
	}

	body, err := renderConfig(scenarioConfig{
		Scenario:       scenarioIntegrated,
		Node:           nodes[0],
		Nodes:          nodes,
		FabricProvider: "tcp",
	})
	if err != nil {
		t.Fatalf("renderConfig: %v", err)
	}

	mustContain(t, body, `[[neighborhoods]]`)
	mustContain(t, body, `local_node_id = 1`)
	mustContain(t, body, `[[neighborhoods.peers]]`)
	mustContain(t, body, `id = 2`)
	mustContain(t, body, `[neighborhoods.peers.config.tcp]`)
	mustContain(t, body, `addr = "10.0.0.2:7001"`)
	mustContain(t, body, `[neighborhoods.routing_plan]`)
	mustContain(t, body, `source = "p2p"`)
	mustContain(t, body, `source = "cache"`)
}

func TestRenderRoutingPlanUsesStorageRingOrder(t *testing.T) {
	nodes := []nodeSpec{
		{ID: 1, FabricAddr: "10.0.0.1:7001", MetricsAddr: "127.0.0.1:9100", DiskPath: "/tmp/a.disk"},
		{ID: 2, FabricAddr: "10.0.0.2:7001", MetricsAddr: "127.0.0.1:9100", DiskPath: "/tmp/b.disk"},
		{ID: 3, FabricAddr: "10.0.0.3:7001", MetricsAddr: "127.0.0.1:9100", DiskPath: "/tmp/c.disk"},
		{ID: 4, FabricAddr: "10.0.0.4:7001", MetricsAddr: "127.0.0.1:9100", DiskPath: "/tmp/d.disk"},
	}

	body, err := renderConfig(scenarioConfig{
		Scenario:       scenarioFabricRPC,
		Node:           nodes[0],
		Nodes:          nodes,
		FabricProvider: "tcp",
	})
	if err != nil {
		t.Fatalf("renderConfig: %v", err)
	}

	mustContain(t, body, `fingers = [2, 3, 4]`)
	mustContain(t, body, `successor = 4`)
	mustContain(t, body, `predecessor = 3`)
}

func TestNodeToRingMatchesStorageHash(t *testing.T) {
	const want uint64 = 6238072747940578789
	if got := nodeToRing(1); got != want {
		t.Fatalf("nodeToRing(1) = %d, want %d", got, want)
	}
}

func TestRenderRDMAConfigUsesRDMAPeers(t *testing.T) {
	nodes := []nodeSpec{
		{ID: 1, FabricAddr: "hex:deadbeef", MetricsAddr: "127.0.0.1:9100", DiskPath: "/tmp/a.disk"},
		{ID: 2, FabricAddr: "hex:cafebabe", MetricsAddr: "127.0.0.1:9100", DiskPath: "/tmp/b.disk"},
	}

	body, err := renderConfig(scenarioConfig{
		Scenario:       scenarioFabricRPC,
		Node:           nodes[0],
		Nodes:          nodes,
		FabricProvider: "rdma",
	})
	if err != nil {
		t.Fatalf("renderConfig: %v", err)
	}

	mustContain(t, body, `[neighborhoods.peers.config.rdma]`)
	mustContain(t, body, `addr = "hex:cafebabe"`)
	mustContain(t, body, `disable_rdma = false`)
	mustNotContain(t, body, `[neighborhoods.peers.config.tcp]`)
}

func mustContain(t *testing.T, s, want string) {
	t.Helper()
	if !strings.Contains(s, want) {
		t.Fatalf("rendered config missing %q:\n%s", want, s)
	}
}

func mustNotContain(t *testing.T, s, want string) {
	t.Helper()
	if strings.Contains(s, want) {
		t.Fatalf("rendered config unexpectedly contains %q:\n%s", want, s)
	}
}
