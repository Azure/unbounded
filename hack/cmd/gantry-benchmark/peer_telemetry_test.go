// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import "testing"

func TestSubtractPeerByteSnapshots(t *testing.T) {
	before := peerByteSnapshot{
		PodNodes: map[string]string{"gantry-a": "node-a", "gantry-b": "node-b"},
		Counters: map[string]map[string]uint64{
			"gantry-a": {"manifest": 1, "config": 2, "layer": 3},
			"gantry-b": {"manifest": 4, "config": 5, "layer": 6},
		},
	}
	after := peerByteSnapshot{
		PodNodes: map[string]string{"gantry-a": "node-a", "gantry-b": "node-b"},
		Counters: map[string]map[string]uint64{
			"gantry-a": {"manifest": 11, "config": 22, "layer": 33},
			"gantry-b": {"manifest": 44, "config": 55, "layer": 66},
		},
	}

	measurement, err := subtractPeerByteSnapshots(before, after)
	if err != nil {
		t.Fatalf("subtractPeerByteSnapshots: %v", err)
	}

	if !measurement.Complete || measurement.Total != 210 || len(measurement.Pods) != 2 {
		t.Fatalf("measurement = %+v, want two pods and 210 bytes", measurement)
	}

	if measurement.Pods[0].PodName != "gantry-a" || measurement.Pods[0].Total != 60 {
		t.Fatalf("first pod = %+v, want gantry-a with 60 bytes", measurement.Pods[0])
	}
}

func TestSubtractPeerByteSnapshotsRejectsRestart(t *testing.T) {
	before := peerByteSnapshot{
		PodNodes: map[string]string{"gantry-old": "node-a"},
		Counters: map[string]map[string]uint64{"gantry-old": {"manifest": 0, "config": 0, "layer": 10}},
	}
	after := peerByteSnapshot{
		PodNodes: map[string]string{"gantry-new": "node-a"},
		Counters: map[string]map[string]uint64{"gantry-new": {"manifest": 0, "config": 0, "layer": 0}},
	}

	if _, err := subtractPeerByteSnapshots(before, after); err == nil {
		t.Fatal("expected a pod restart to invalidate the peer-byte delta")
	}
}

func TestSubtractPeerByteSnapshotsRejectsCounterReset(t *testing.T) {
	before := peerByteSnapshot{
		PodNodes: map[string]string{"gantry-a": "node-a"},
		Counters: map[string]map[string]uint64{"gantry-a": {"manifest": 0, "config": 0, "layer": 10}},
	}
	after := peerByteSnapshot{
		PodNodes: map[string]string{"gantry-a": "node-a"},
		Counters: map[string]map[string]uint64{"gantry-a": {"manifest": 0, "config": 0, "layer": 9}},
	}

	if _, err := subtractPeerByteSnapshots(before, after); err == nil {
		t.Fatal("expected a counter reset to invalidate the peer-byte delta")
	}
}

func TestSubtractPeerByteSnapshotsDefaultsMissingKindsToZero(t *testing.T) {
	before := peerByteSnapshot{
		PodNodes: map[string]string{"gantry-a": "node-a"},
		Counters: map[string]map[string]uint64{"gantry-a": {"layer": 10}},
	}
	after := peerByteSnapshot{
		PodNodes: map[string]string{"gantry-a": "node-a"},
		Counters: map[string]map[string]uint64{"gantry-a": {"layer": 20}},
	}

	measurement, err := subtractPeerByteSnapshots(before, after)
	if err != nil {
		t.Fatalf("subtractPeerByteSnapshots: %v", err)
	}

	if measurement.Total != 10 || measurement.Pods[0].ByKind["manifest"] != 0 || measurement.Pods[0].ByKind["config"] != 0 {
		t.Fatalf("measurement = %+v, want missing kinds treated as zero", measurement)
	}
}
