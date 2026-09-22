// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package status

import (
	"encoding/json"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
	"github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func TestOverviewJSONFields(t *testing.T) {
	data, err := json.Marshal(v1alpha1.NodeStatusOverview{})
	if err != nil {
		t.Fatal(err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"peerCount", "healthyPeers", "routeCount", "routeMismatch"} {
		if _, ok := fields[name]; !ok {
			t.Errorf("zero-valued overview fact %q omitted", name)
		}
	}

	for _, name := range []string{"peers", "routingTable", "bpfEntries", "peerMeasurements"} {
		if _, ok := fields[name]; ok {
			t.Errorf("detail field %q present", name)
		}
	}
}

func TestOverviewProtoContract(t *testing.T) {
	want := &statusproto.NodeStatusMessage{
		Type: "node_status_summary", NodeName: "node",
		Summary: &statusproto.NodeStatusOverview{
			NodeInfo:  &statusproto.NodeInfo{Name: "node"},
			PeerCount: 7, HealthyPeers: 5, RouteCount: 11, RouteMismatch: true,
			NodeErrors: []*statusproto.NodeError{{Type: "cni", Message: "not ready"}},
		},
	}

	data, err := proto.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}

	got := &statusproto.NodeStatusMessage{}
	if err := proto.Unmarshal(data, got); err != nil {
		t.Fatal(err)
	}

	if !proto.Equal(got, want) || got.Status != nil || got.Delta != nil {
		t.Fatalf("summary round trip changed payload: %v", got)
	}

	fields := got.ProtoReflect().Descriptor().Fields()
	for name, number := range map[protoreflect.Name]protoreflect.FieldNumber{
		"type": 1, "node_name": 2, "base_revision": 3, "status": 4, "delta": 5, "summary": 6,
	} {
		if field := fields.ByName(name); field == nil || field.Number() != number {
			t.Errorf("field %s no longer has number %d", name, number)
		}
	}

	summaryFields := got.Summary.ProtoReflect().Descriptor().Fields()
	for _, name := range []protoreflect.Name{"peers", "routing_table", "bpf_entries", "peer_measurements"} {
		if summaryFields.ByName(name) != nil {
			t.Errorf("summary exposes detail field %q", name)
		}
	}
}
