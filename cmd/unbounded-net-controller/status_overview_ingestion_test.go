// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func submitOverview(t *testing.T, channel string, health *healthState, message *statusproto.NodeStatusMessage) NodeStatusPushAck {
	t.Helper()

	var (
		data []byte
		err  error
	)
	if channel == "proto-http" || channel == "proto-ws" {
		data, err = proto.Marshal(message)
	} else {
		envelope := NodeStatusWSMessage{
			Type: message.Type, NodeName: message.NodeName, DetailRequestID: message.DetailRequestId,
		}
		if message.Summary != nil {
			overview := protoToNodeOverview(message.Summary)
			envelope.Summary = &overview
		}

		if message.Status != nil {
			status := protoToNodeStatus(message.Status)
			envelope.Status = &status
		}

		if message.Delta != nil {
			envelope.Delta = map[string]json.RawMessage{"timestamp": json.RawMessage(`null`)}
		}

		data, err = json.Marshal(envelope)
	}

	if err != nil {
		t.Fatal(err)
	}

	switch channel {
	case "proto-http":
		ack, code, err := handleProtoPushRequest(health, data, "push")
		if err != nil || code != http.StatusOK {
			return NodeStatusPushAck{Status: "rejected"}
		}

		return ack
	case "json-http":
		ack, code, err := handleStatusPushRequestWithSource(health, data, "push")
		if err != nil || code != http.StatusOK {
			return NodeStatusPushAck{Status: "rejected"}
		}

		return ack
	case "proto-ws":
		decoded, err := decodeProtoWSMessage(data)
		if err != nil {
			return NodeStatusPushAck{Status: "rejected"}
		}

		_, ack := handleProtoWSMessage(health, decoded, "ws")

		return ack
	case "json-ws":
		_, ack := handleNodeStatusWSMessageWithSource(health, data, "ws")
		return ack
	default:
		t.Fatalf("unknown test channel %s", channel)
		return NodeStatusPushAck{}
	}
}

func TestOverviewIngestionAllChannels(t *testing.T) {
	for _, channel := range []string{"proto-http", "json-http", "proto-ws", "json-ws"} {
		t.Run(channel, func(t *testing.T) {
			health := &healthState{statusCache: NewNodeStatusCache()}
			message := &statusproto.NodeStatusMessage{
				Type: statusv1alpha1.NodeStatusSummaryType, NodeName: "node",
				Summary: &statusproto.NodeStatusOverview{
					TimestampUnixNs: 1000, LastPushTimeUnixNs: 2000,
					NodeInfo: &statusproto.NodeInfo{
						Name: "node", SiteName: "site", WireGuard: &statusproto.WireGuardStatusInfo{Interface: "wg0"},
					},
					PeerCount: 10, HealthyPeers: 8, RouteCount: 20, RouteMismatch: true,
					NodeErrors:  []*statusproto.NodeError{{Type: "cni", Message: "blocked"}},
					HealthCheck: &statusproto.HealthCheckStatus{Summary: "not healthy"},
					NodePodInfo: &statusproto.NodePodInfo{PodName: "agent"},
				},
			}

			ack := submitOverview(t, channel, health, message)
			if ack.Status != "ok" || ack.Revision != 1 || !ack.SummarySupported {
				t.Fatalf("unexpected ACK: %+v", ack)
			}

			entry, ok := health.statusCache.Get("node")
			if !ok || entry.Overview == nil {
				t.Fatal("overview was not stored")
			}

			overview := entry.Overview
			if overview.PeerCount != 10 || overview.HealthyPeers != 8 || overview.RouteCount != 20 || !overview.RouteMismatch ||
				overview.NodeErrors[0].Message != "blocked" || overview.NodeInfo.SiteName != "site" ||
				overview.NodeInfo.WireGuard.Interface != "wg0" || overview.NodePodInfo.PodName != "agent" ||
				overview.HealthCheck.Summary != "not healthy" ||
				!overview.Timestamp.Equal(time.Unix(0, 1000)) || !overview.LastPushTime.Equal(time.Unix(0, 2000)) {
				t.Fatalf("overview changed during ingestion: %+v", overview)
			}

			if entry.Status.Peers != nil || entry.Status.RoutingTable.Routes != nil || entry.Status.BpfEntries != nil {
				t.Fatal("summary retained diagnostic arrays")
			}

			if next := submitOverview(t, channel, health, message); next.Revision != 2 {
				t.Fatal("summary resync did not advance routine revision")
			}
		})
	}
}

func TestOverviewIngestionRejectsInvalidEnvelopes(t *testing.T) {
	for _, channel := range []string{"proto-http", "json-http", "proto-ws", "json-ws"} {
		for _, tc := range []struct {
			name   string
			mutate func(*statusproto.NodeStatusMessage)
		}{
			{"missing", func(m *statusproto.NodeStatusMessage) { m.Summary = nil }},
			{"identity mismatch", func(m *statusproto.NodeStatusMessage) { m.Summary.NodeInfo.Name = "other" }},
			{"negative counts", func(m *statusproto.NodeStatusMessage) { m.Summary.PeerCount = -1 }},
			{"impossible counts", func(m *statusproto.NodeStatusMessage) { m.Summary.HealthyPeers = 1 }},
			{"full mixed with summary", func(m *statusproto.NodeStatusMessage) { m.Status = &statusproto.NodeStatusFull{} }},
			{"delta mixed with summary", func(m *statusproto.NodeStatusMessage) { m.Delta = &statusproto.NodeStatusDelta{} }},
			{"detail correlation on summary", func(m *statusproto.NodeStatusMessage) { m.DetailRequestId = "request" }},
			{"summary in full", func(m *statusproto.NodeStatusMessage) { m.Type = "node_status_full" }},
		} {
			t.Run(channel+"/"+tc.name, func(t *testing.T) {
				health := &healthState{statusCache: NewNodeStatusCache()}
				message := &statusproto.NodeStatusMessage{
					Type: statusv1alpha1.NodeStatusSummaryType, NodeName: "node",
					Summary: &statusproto.NodeStatusOverview{NodeInfo: &statusproto.NodeInfo{Name: "node"}},
				}
				tc.mutate(message)

				ack := submitOverview(t, channel, health, message)
				if ack.Status == "ok" || health.statusCache.Len() != 0 {
					t.Fatalf("invalid summary accepted: %+v", ack)
				}
			})
		}
	}
}

func TestOverviewIdentityRejectsDuplicateAndConflictingFields(t *testing.T) {
	for _, data := range []string{
		`{"nodeName":"node","summary":{"nodeInfo":{"name":"other"}}}`,
		`{"nodeName":"node","summary":{"nodeInfo":{"name":"other"}},"Summary":null}`,
		`{"summary":{"nodeInfo":{"name":"other","Name":"node"}}}`,
		`{"summary":{"nodeInfo":{"name":"other"},"NodeInfo":{"name":"node"}}}`,
	} {
		if _, err := extractNodeNameFromWSMessage([]byte(data)); err == nil {
			t.Fatalf("ambiguous summary identity accepted: %s", data)
		}
	}

	name, err := extractNodeNameFromWSMessage([]byte(`{"summary":{"nodeInfo":{"name":"node"}}}`))
	if err != nil || name != "node" {
		t.Fatalf("summary-only identity lost: %q %v", name, err)
	}
}

func TestOverviewCapabilityProtoAck(t *testing.T) {
	data, err := marshalProtoAck("node_status_ack", NodeStatusPushAck{Status: "ok", Revision: 7})
	if err != nil {
		t.Fatal(err)
	}

	var ack statusproto.NodeStatusAck
	if err := proto.Unmarshal(data, &ack); err != nil {
		t.Fatal(err)
	}

	if !ack.SummarySupported || !ack.PeerMeasurements || ack.Revision != 7 {
		t.Fatalf("ACK lost capability or revision: %v", &ack)
	}
}
