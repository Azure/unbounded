// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
)

const cniGuardTestMessage = "CNI configuration blocked; remaining unready: bridge=cbr0 assignedPodCIDRs=[10.244.2.0/24] interface=eth0 address=10.244.1.5 outside assigned PodCIDRs"

func assertCNIGuardDashboardStatus(t *testing.T, cache *NodeStatusCache, blocked bool) {
	t.Helper()

	cached, ok := cache.Get("node-a")
	if !ok {
		t.Fatal("missing node status")
	}

	status := *cached.Status
	status.StatusSource = cached.Source
	cluster := &ClusterStatusResponse{Nodes: []*NodeStatusResponse{&status}}
	summary := buildClusterSummary(cluster).NodeSummaries[0]

	foundProblem := false

	for _, problem := range collectClusterProblems(cluster) {
		if slices.Contains(problem.Errors, cniGuardTestMessage) {
			foundProblem = true
		}
	}

	if foundProblem != blocked {
		t.Fatalf("dashboard problem present=%v, want %v", foundProblem, blocked)
	}

	if blocked {
		if len(status.NodeErrors) != 1 || status.NodeErrors[0].Type != "configPodCIDRGuard" || status.NodeErrors[0].Message != cniGuardTestMessage {
			t.Fatalf("lost CNI diagnostic: %+v", status.NodeErrors)
		}

		if summary.CniStatus != "Errors" || summary.CniTone != "danger" || summary.ErrorCount != 1 || summary.FirstError != cniGuardTestMessage {
			t.Fatalf("blocked node appears healthy or lost its reason: %+v", summary)
		}
	} else if len(status.NodeErrors) != 0 || summary.ErrorCount != 0 || summary.FirstError != "" || summary.CniStatus != "Healthy" {
		t.Fatalf("recovered node retained its CNI error: errors=%+v summary=%+v", status.NodeErrors, summary)
	}
}

func TestCNIGuardJSONStatusRecovery(t *testing.T) {
	for _, source := range []string{"push", "ws", "apiserver-push", "apiserver-ws"} {
		for _, recovery := range []string{"null", "[]", "full"} {
			t.Run(source+"/"+recovery, func(t *testing.T) {
				cache := NewNodeStatusCache()
				initial := NodeStatusResponse{
					NodeInfo: NodeInfo{Name: "node-a"},
					NodeErrors: []NodeError{
						{Type: "configPodCIDRGuard", Message: cniGuardTestMessage},
					},
				}

				data, err := json.Marshal(initial)
				if err != nil {
					t.Fatal(err)
				}

				var decoded NodeStatusResponse
				if err := json.Unmarshal(data, &decoded); err != nil {
					t.Fatal(err)
				}

				revision := cache.StoreFull("node-a", decoded, source)
				assertCNIGuardDashboardStatus(t, cache, true)

				revision, conflict, err := cache.ApplyDelta("node-a", revision, map[string]json.RawMessage{}, source)
				if err != nil || conflict {
					t.Fatalf("unrelated delta: conflict=%v err=%v", conflict, err)
				}

				assertCNIGuardDashboardStatus(t, cache, true)

				if recovery == "full" {
					cache.StoreFull("node-a", NodeStatusResponse{NodeInfo: initial.NodeInfo}, source)
				} else {
					_, conflict, err = cache.ApplyDelta("node-a", revision, map[string]json.RawMessage{
						"nodeErrors": json.RawMessage(recovery),
					}, source)
					if err != nil || conflict {
						t.Fatalf("recovery delta: conflict=%v err=%v", conflict, err)
					}
				}

				assertCNIGuardDashboardStatus(t, cache, false)
			})
		}
	}
}

func TestCNIGuardProtoStatusRecovery(t *testing.T) {
	for _, source := range []string{"push", "ws", "apiserver-push", "apiserver-ws"} {
		for _, recovery := range []string{"delta", "full"} {
			t.Run(source+"/"+recovery, func(t *testing.T) {
				health := &healthState{statusCache: NewNodeStatusCache()}
				send := func(message *statusproto.NodeStatusMessage) uint64 {
					t.Helper()

					data, err := proto.Marshal(message)
					if err != nil {
						t.Fatal(err)
					}

					var ack NodeStatusPushAck

					if source == "ws" || source == "apiserver-ws" {
						var messageType string

						messageType, ack = handleProtoWSMessage(health, data, source)
						if messageType != "node_status_ack" {
							t.Fatalf("unexpected message type %q: %+v", messageType, ack)
						}
					} else {
						var code int

						ack, code, err = handleProtoPushRequest(health, data, source)
						if err != nil || code != http.StatusOK {
							t.Fatalf("push failed: code=%d err=%v", code, err)
						}
					}

					if ack.Status != "ok" {
						t.Fatalf("unexpected ack: %+v", ack)
					}

					return ack.Revision
				}
				revision := send(&statusproto.NodeStatusMessage{
					Type:     "node_status_full",
					NodeName: "node-a",
					Status: &statusproto.NodeStatusFull{
						NodeInfo: &statusproto.NodeInfo{Name: "node-a"},
						NodeErrors: []*statusproto.NodeError{
							{Type: "configPodCIDRGuard", Message: cniGuardTestMessage},
						},
					},
				})

				assertCNIGuardDashboardStatus(t, health.statusCache, true)

				if recovery == "full" {
					send(&statusproto.NodeStatusMessage{
						Type:     "node_status_full",
						NodeName: "node-a",
						Status:   &statusproto.NodeStatusFull{NodeInfo: &statusproto.NodeInfo{Name: "node-a"}},
					})
				} else {
					send(&statusproto.NodeStatusMessage{
						Type:         "node_status_delta",
						NodeName:     "node-a",
						BaseRevision: revision,
						Delta:        &statusproto.NodeStatusDelta{UpdatedFields: []string{"nodeErrors"}},
					})
				}

				assertCNIGuardDashboardStatus(t, health.statusCache, false)
			})
		}
	}
}
