// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func TestSummaryPreservesNodeInfoAndFreshnessWithoutDetails(t *testing.T) {
	received := time.Now()
	node := retentionFixture(10000)
	node.LastPushTime = &received
	node.NodeInfo.ProviderID = "azure://vm"
	node.NodeInfo.InternalIPs = []string{"192.0.2.1"}
	node.NodeInfo.K8sLabels = map[string]string{"node.kubernetes.io/instance-type": "test"}
	node.NodeInfo.K8sUpdatedAt = &received
	node.NodeInfo.BuildInfo = &statusv1alpha1.BuildInfo{Version: "test"}
	cluster := &ClusterStatusResponse{Nodes: []*NodeStatusResponse{&node}}
	summary := buildClusterSummary(cluster)

	row := summary.NodeSummaries[0]
	if row.NodeInfo == &node.NodeInfo || !reflect.DeepEqual(row.NodeInfo, &node.NodeInfo) ||
		row.LastPushTime == nil || !row.LastPushTime.Equal(received) {
		t.Fatal("summary lost metadata/freshness or retained the full node allocation")
	}

	data, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}

	for _, forbidden := range []string{`"peers"`, `"routingTable"`, `"bpfEntries"`, "private-detail-marker"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("summary retained diagnostic data: %s", forbidden)
		}
	}

	if delta := computeClusterSummaryDelta(summary, buildClusterSummary(cluster)); delta != nil {
		t.Fatalf("equal metadata copies emitted a spurious update: %+v", delta)
	}

	node.NodeInfo = NodeInfo{Name: "node", ProviderID: "azure://replacement"}

	next := buildClusterSummary(cluster)
	if delta := computeClusterSummaryDelta(summary, next); delta == nil || len(delta.NodeSummaries) != 1 {
		t.Fatal("metadata-only update was omitted")
	}

	later := received.Add(time.Second)
	node.LastPushTime = &later

	if delta := computeClusterSummaryDelta(next, buildClusterSummary(cluster)); delta == nil || len(delta.NodeSummaries) != 1 {
		t.Fatal("freshness-only update was omitted")
	}
}

func TestSummaryDeltaEncodesExplicitMetadataClears(t *testing.T) {
	prev := &ClusterSummary{
		LeaderInfo: &LeaderInfo{PodName: "leader"},
		BuildInfo:  &BuildInfo{Version: "version"},
		Sites:      []SiteStatus{{Name: "site"}},
		GatewayPools: []GatewayPoolStatus{{
			Name: "pool",
		}},
		Peerings: []PeeringStatus{{Name: "peering"}},
		Errors:   []string{"error"},
		Warnings: []string{"warning"},
		Problems: []StatusProblem{{Name: "problem"}},
	}

	delta := computeClusterSummaryDelta(prev, &ClusterSummary{})
	if delta == nil {
		t.Fatal("metadata clear produced no delta")
	}

	data, err := json.Marshal(delta)
	if err != nil {
		t.Fatal(err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{
		"leaderInfo", "buildInfo", "sites", "gatewayPools", "peerings",
		"errors", "warnings", "problems",
	} {
		if _, ok := fields[name]; !ok {
			t.Errorf("clear for %s was omitted: %s", name, data)
		}
	}

	for _, name := range []string{"sites", "gatewayPools", "peerings", "errors", "warnings", "problems"} {
		if string(fields[name]) != "[]" {
			t.Errorf("clear for %s = %s, want []", name, fields[name])
		}
	}

	if string(fields["leaderInfo"]) != "null" || string(fields["buildInfo"]) != "null" {
		t.Errorf("pointer clears were not explicit nulls: %s", data)
	}
}
