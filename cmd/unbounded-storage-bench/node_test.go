// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import "testing"

func TestParseNodeSpecsAppliesDefaults(t *testing.T) {
	opts := options{
		sshUser:       "bench",
		remoteWorkdir: "/var/tmp/bench",
		nodeSpecs: stringList{
			"id=2,ssh=host-b,fabric=10.0.0.2:7001,metrics=127.0.0.1:9100",
			"id=1,ssh=root@host-a,fabric=10.0.0.1:7001,metrics=127.0.0.1:9100,block=/dev/nvme0n1",
		},
	}

	nodes, err := parseNodeSpecs(opts)
	if err != nil {
		t.Fatalf("parseNodeSpecs: %v", err)
	}

	if len(nodes) != 2 {
		t.Fatalf("nodes len = %d, want 2", len(nodes))
	}

	if nodes[0].ID != 1 || nodes[1].ID != 2 {
		t.Fatalf("nodes not sorted by id: %+v", nodes)
	}

	if nodes[0].SSHTarget != "root@host-a" {
		t.Fatalf("node 1 ssh target = %q", nodes[0].SSHTarget)
	}

	if nodes[1].SSHTarget != "bench@host-b" {
		t.Fatalf("node 2 ssh target = %q", nodes[1].SSHTarget)
	}

	if nodes[1].ConfigPath != "/var/tmp/bench/unbounded-storage-bench-node-2.toml" {
		t.Fatalf("node 2 config path = %q", nodes[1].ConfigPath)
	}

	if nodes[1].DiskPath != "/var/tmp/bench/unbounded-storage-bench-node-2.disk" {
		t.Fatalf("node 2 disk path = %q", nodes[1].DiskPath)
	}

	if nodes[1].ListenAddr != nodes[1].FabricAddr {
		t.Fatalf("node 2 listen addr = %q, want fabric addr", nodes[1].ListenAddr)
	}

	if nodes[0].DiskPath != "" || nodes[0].BlockDevice != "/dev/nvme0n1" {
		t.Fatalf("node 1 disk/block = %q/%q", nodes[0].DiskPath, nodes[0].BlockDevice)
	}
}

func TestParseNodeSpecsRejectsDuplicateIDs(t *testing.T) {
	_, err := parseNodeSpecs(options{
		nodeSpecs: stringList{
			"id=1,ssh=host-a,fabric=10.0.0.1:7001,metrics=127.0.0.1:9100",
			"id=1,ssh=host-b,fabric=10.0.0.2:7001,metrics=127.0.0.1:9100",
		},
	})
	if err == nil {
		t.Fatal("parseNodeSpecs succeeded with duplicate ids")
	}
}

func TestParseNodeSpecRejectsBadMetricsAddress(t *testing.T) {
	_, err := parseNodeSpec("id=1,ssh=host-a,fabric=10.0.0.1:7001,metrics=127.0.0.1", options{})
	if err == nil {
		t.Fatal("parseNodeSpec succeeded with bad metrics address")
	}
}
