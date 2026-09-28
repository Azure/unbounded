// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package status

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
	"github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func TestOverviewDiagnosticFactsPreserveLegacyRules(t *testing.T) {
	now := time.Unix(1000, 0)
	yes := true
	full := &v1alpha1.NodeStatusResponse{
		NodeInfo:   v1alpha1.NodeInfo{Name: "node", ProviderID: "azure://vm"},
		NodeErrors: []v1alpha1.NodeError{{Type: "cni", Message: "not ready"}},
		Peers: []v1alpha1.PeerStatus{
			{Name: "same", Tunnel: v1alpha1.PeerTunnelStatus{Protocol: "IPIP"}, HealthCheck: &v1alpha1.HealthCheckPeerStatus{Enabled: true, Status: " UP "}},
			{Name: "same", HealthCheck: &v1alpha1.HealthCheckPeerStatus{Status: " up "}},
			{Name: "same", Tunnel: v1alpha1.PeerTunnelStatus{LastHandshake: now}, HealthCheck: &v1alpha1.HealthCheckPeerStatus{Status: " down "}},
		},
		RoutingTable: v1alpha1.RoutingTableInfo{Routes: []v1alpha1.RouteEntry{
			{NextHops: []v1alpha1.NextHop{{Expected: &yes}, {Present: &yes}}},
			{NextHops: []v1alpha1.NextHop{{Expected: &yes}}},
		}},
	}

	got := OverviewFromStatus(full, now)
	if got.RouteMismatchCount != 3 || !got.RouteMismatch || got.UnhealthyPeerLinks != 1 || !got.UsesIPIP {
		t.Fatalf("diagnostic facts changed: %+v", got)
	}

	if got.UnhealthyPeerLinks == got.PeerCount-got.HealthyPeers {
		t.Fatal("fixture must distinguish diagnostic normalization from overview peer counts")
	}

	if !reflect.DeepEqual(got.NodeErrors, full.NodeErrors) {
		t.Fatal("IPIP warning must not be injected into node errors or alter CNI status")
	}

	full.Peers = nil
	full.RoutingTable.Routes = nil

	got = OverviewFromStatus(full, now)
	if got.RouteMismatchCount != 0 || got.RouteMismatch || got.UnhealthyPeerLinks != 0 || got.UsesIPIP {
		t.Fatalf("removed diagnostics remained in projection: %+v", got)
	}
}

func TestOverviewDiagnosticWireFields(t *testing.T) {
	data, err := json.Marshal(v1alpha1.NodeStatusOverview{})
	if err != nil {
		t.Fatal(err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}

	for _, field := range []string{"routeMismatchCount", "unhealthyPeerLinks", "usesIPIP"} {
		if fields[field] == nil {
			t.Errorf("zero-valued diagnostic fact %q was omitted", field)
		}
	}

	want := &statusproto.NodeStatusOverview{RouteMismatchCount: 3, UnhealthyPeerLinks: 2, UsesIpip: true}

	data, err = proto.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}

	got := &statusproto.NodeStatusOverview{}
	if err := proto.Unmarshal(data, got); err != nil {
		t.Fatal(err)
	}

	if !proto.Equal(got, want) {
		t.Fatalf("diagnostic facts lost on protobuf round trip: %v", got)
	}

	protoFields := got.ProtoReflect().Descriptor().Fields()
	for name, number := range map[protoreflect.Name]protoreflect.FieldNumber{
		"route_mismatch_count": 13, "unhealthy_peer_links": 14, "uses_ipip": 15,
	} {
		if field := protoFields.ByName(name); field == nil || field.Number() != number {
			t.Errorf("field %s no longer has number %d", name, number)
		}
	}
}
