// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	statusproto "github.com/Azure/unbounded-kube/internal/net/status/proto"
	statusv1alpha1 "github.com/Azure/unbounded-kube/internal/net/status/v1alpha1"
)

// protoToNodeStatus converts a protobuf NodeStatusFull to the Go NodeStatusResponse type.
func protoToNodeStatus(msg *statusproto.NodeStatusFull) NodeStatusResponse {
	var resp NodeStatusResponse
	if msg == nil {
		return resp
	}

	if msg.TimestampUnixNs != 0 {
		resp.Timestamp = time.Unix(0, msg.TimestampUnixNs)
	}

	if msg.NodeInfo != nil {
		resp.NodeInfo = protoToNodeInfo(msg.NodeInfo)
	}

	resp.Peers = protoToPeers(msg.Peers)
	if msg.RoutingTable != nil {
		resp.RoutingTable = protoToRoutingTable(msg.RoutingTable)
	}

	resp.HealthCheck = protoToHealthCheckStatus(msg.HealthCheck)
	resp.NodeErrors = protoToNodeErrors(msg.NodeErrors)

	resp.FetchError = msg.FetchError
	if msg.LastPushTimeUnixNs != 0 {
		t := time.Unix(0, msg.LastPushTimeUnixNs)
		resp.LastPushTime = &t
	}

	resp.StatusSource = msg.StatusSource
	resp.NodePodInfo = protoToNodePodInfo(msg.NodePodInfo)
	resp.BpfEntries = protoToBpfEntries(msg.BpfEntries)

	return resp
}

// protoToNodeInfo converts a protobuf NodeInfo to the Go NodeInfo type.
func protoToNodeInfo(pb *statusproto.NodeInfo) statusv1alpha1.NodeInfo {
	ni := statusv1alpha1.NodeInfo{
		Name:        pb.Name,
		SiteName:    pb.SiteName,
		IsGateway:   pb.IsGateway,
		PodCIDRs:    pb.PodCidrs,
		InternalIPs: pb.InternalIps,
		ExternalIPs: pb.ExternalIps,
		K8sReady:    pb.K8SReady,
		ProviderID:  pb.ProviderId,
		OSImage:     pb.OsImage,
		Kernel:      pb.Kernel,
		Kubelet:     pb.Kubelet,
		Arch:        pb.Arch,
		NodeOS:      pb.NodeOs,
		K8sLabels:   pb.K8SLabels,
	}
	if pb.BuildInfo != nil {
		ni.BuildInfo = &statusv1alpha1.BuildInfo{
			Version:   pb.BuildInfo.Version,
			Commit:    pb.BuildInfo.Commit,
			BuildTime: pb.BuildInfo.BuildTime,
		}
	}

	if pb.WireGuard != nil {
		ni.WireGuard = &statusv1alpha1.WireGuardStatusInfo{
			Interface:  pb.WireGuard.Interface,
			PublicKey:  pb.WireGuard.PublicKey,
			ListenPort: int(pb.WireGuard.ListenPort),
			PeerCount:  int(pb.WireGuard.PeerCount),
		}
	}

	if pb.K8SUpdatedAtUnixNs != 0 {
		t := time.Unix(0, pb.K8SUpdatedAtUnixNs)
		ni.K8sUpdatedAt = &t
	}

	return ni
}

// protoToPeers converts a slice of protobuf PeerStatus to Go PeerStatus values.
func protoToPeers(pbs []*statusproto.PeerStatus) []statusv1alpha1.PeerStatus {
	if len(pbs) == 0 {
		return nil
	}

	peers := make([]statusv1alpha1.PeerStatus, 0, len(pbs))
	for _, pb := range pbs {
		peers = append(peers, protoToPeerStatus(pb))
	}

	return peers
}

// protoToPeerStatus converts a single protobuf PeerStatus to the Go type.
func protoToPeerStatus(pb *statusproto.PeerStatus) statusv1alpha1.PeerStatus {
	ps := statusv1alpha1.PeerStatus{
		Name:              pb.Name,
		PeerType:          pb.PeerType,
		SiteName:          pb.SiteName,
		PodCIDRGateways:   pb.PodCidrGateways,
		SkipPodCIDRRoutes: pb.SkipPodCidrRoutes,
		RouteDestinations: pb.RouteDestinations,
	}
	if len(pb.RouteDistances) > 0 {
		ps.RouteDistances = make(map[string]int, len(pb.RouteDistances))
		for k, v := range pb.RouteDistances {
			ps.RouteDistances[k] = int(v)
		}
	}

	if pb.Tunnel != nil {
		ps.Tunnel = protoToPeerTunnelStatus(pb.Tunnel)
	}

	ps.HealthCheck = protoToHealthCheckPeerStatus(pb.HealthCheck)

	return ps
}

// protoToPeerTunnelStatus converts a protobuf PeerTunnelStatus to the Go type.
func protoToPeerTunnelStatus(pb *statusproto.PeerTunnelStatus) statusv1alpha1.PeerTunnelStatus {
	ts := statusv1alpha1.PeerTunnelStatus{
		Protocol:   pb.Protocol,
		Interface:  pb.Interface,
		PublicKey:  pb.PublicKey,
		Endpoint:   pb.Endpoint,
		AllowedIPs: pb.AllowedIps,
		RxBytes:    pb.RxBytes,
		TxBytes:    pb.TxBytes,
	}
	if pb.LastHandshakeUnixNs != 0 {
		ts.LastHandshake = time.Unix(0, pb.LastHandshakeUnixNs)
	}

	return ts
}

// protoToRoutingTable converts a protobuf RoutingTableInfo to the Go type.
func protoToRoutingTable(pb *statusproto.RoutingTableInfo) statusv1alpha1.RoutingTableInfo {
	rt := statusv1alpha1.RoutingTableInfo{
		ManagedRouteCount: int(pb.ManagedRouteCount),
		PendingRouteCount: int(pb.PendingRouteCount),
	}
	if len(pb.Routes) > 0 {
		rt.Routes = make([]statusv1alpha1.RouteEntry, 0, len(pb.Routes))
		for _, re := range pb.Routes {
			rt.Routes = append(rt.Routes, protoToRouteEntry(re))
		}
	}

	return rt
}

// protoToRouteEntry converts a protobuf RouteEntry to the Go type.
func protoToRouteEntry(pb *statusproto.RouteEntry) statusv1alpha1.RouteEntry {
	re := statusv1alpha1.RouteEntry{
		Destination: pb.Destination,
		Family:      pb.Family,
		Table:       int(pb.Table),
	}
	if len(pb.NextHops) > 0 {
		re.NextHops = make([]statusv1alpha1.NextHop, 0, len(pb.NextHops))
		for _, nh := range pb.NextHops {
			re.NextHops = append(re.NextHops, protoToNextHop(nh))
		}
	}

	return re
}

// protoToNextHop converts a protobuf NextHop to the Go type.
func protoToNextHop(pb *statusproto.NextHop) statusv1alpha1.NextHop {
	nh := statusv1alpha1.NextHop{
		Gateway:          pb.Gateway,
		Device:           pb.Device,
		Distance:         int(pb.Distance),
		Weight:           int(pb.Weight),
		MTU:              int(pb.Mtu),
		PeerDestinations: pb.PeerDestinations,
	}
	if len(pb.RouteTypes) > 0 {
		nh.RouteTypes = make([]statusv1alpha1.RouteType, 0, len(pb.RouteTypes))
		for _, rt := range pb.RouteTypes {
			nh.RouteTypes = append(nh.RouteTypes, statusv1alpha1.RouteType{
				Type:       rt.Type,
				Attributes: rt.Attributes,
			})
		}
	}

	if pb.Expected != nil {
		v := pb.Expected.Value
		nh.Expected = &v
	}

	if pb.Present != nil {
		v := pb.Present.Value
		nh.Present = &v
	}

	if pb.Info != nil {
		nh.Info = &statusv1alpha1.NextHopInfo{
			ObjectName: pb.Info.ObjectName,
			ObjectType: pb.Info.ObjectType,
			RouteType:  pb.Info.RouteType,
		}
	}

	return nh
}

// protoToHealthCheckStatus converts a protobuf HealthCheckStatus to the Go type.
func protoToHealthCheckStatus(pb *statusproto.HealthCheckStatus) *statusv1alpha1.HealthCheckStatus {
	if pb == nil {
		return nil
	}

	hc := &statusv1alpha1.HealthCheckStatus{
		Healthy:   pb.Healthy,
		Summary:   pb.Summary,
		PeerCount: int(pb.PeerCount),
	}
	if pb.CheckedAtUnixNs != 0 {
		hc.CheckedAt = time.Unix(0, pb.CheckedAtUnixNs)
	}

	return hc
}

// protoToHealthCheckPeerStatus converts a protobuf HealthCheckPeerStatus to the Go type.
func protoToHealthCheckPeerStatus(pb *statusproto.HealthCheckPeerStatus) *statusv1alpha1.HealthCheckPeerStatus {
	if pb == nil {
		return nil
	}

	return &statusv1alpha1.HealthCheckPeerStatus{
		Enabled: pb.Enabled,
		Status:  pb.Status,
		Uptime:  pb.Uptime,
		RTT:     pb.Rtt,
	}
}

// protoToNodeErrors converts protobuf NodeError messages to Go NodeError values.
func protoToNodeErrors(pbs []*statusproto.NodeError) []statusv1alpha1.NodeError {
	if len(pbs) == 0 {
		return nil
	}

	errs := make([]statusv1alpha1.NodeError, 0, len(pbs))
	for _, e := range pbs {
		ne := statusv1alpha1.NodeError{
			Type:    e.Type,
			Message: e.Message,
		}
		if e.TimestampNs != 0 {
			ne.Timestamp = time.Unix(0, e.TimestampNs)
		}

		errs = append(errs, ne)
	}

	return errs
}

// protoToNodePodInfo converts a protobuf NodePodInfo to the Go type.
func protoToNodePodInfo(pb *statusproto.NodePodInfo) *statusv1alpha1.NodePodInfo {
	if pb == nil {
		return nil
	}

	npi := &statusv1alpha1.NodePodInfo{
		PodName:  pb.PodName,
		Restarts: pb.Restarts,
	}
	if pb.StartTimeUnixNs != 0 {
		npi.StartTime = time.Unix(0, pb.StartTimeUnixNs)
	}

	return npi
}

// protoToBpfEntries converts protobuf BpfEntry messages to the Go type.
func protoToBpfEntries(entries []*statusproto.BpfEntry) []statusv1alpha1.BpfEntry {
	if len(entries) == 0 {
		return nil
	}

	result := make([]statusv1alpha1.BpfEntry, len(entries))
	for i, e := range entries {
		result[i] = statusv1alpha1.BpfEntry{
			CIDR:      e.Cidr,
			Remote:    e.Remote,
			Node:      e.Node,
			Interface: e.InterfaceName,
			Protocol:  e.Protocol,
			Healthy:   e.Healthy,
			VNI:       e.Vni,
			MTU:       int(e.Mtu),
			IfIndex:   e.Ifindex,
		}
	}

	return result
}

// protoToParsedDelta converts a protobuf NodeStatusDelta into a parsedDelta
// that can be applied directly via NodeStatusCache.applyParsedDelta. This
// avoids the JSON round-trip that the regular ApplyDelta path uses.
func protoToParsedDelta(msg *statusproto.NodeStatusDelta) parsedDelta {
	pd := parsedDelta{nullFields: make(map[string]bool)}
	if msg == nil {
		return pd
	}

	updatedSet := make(map[string]bool, len(msg.UpdatedFields))
	for _, f := range msg.UpdatedFields {
		updatedSet[f] = true
	}

	if updatedSet["nodeInfo"] {
		if msg.NodeInfo != nil {
			ni := protoToNodeInfo(msg.NodeInfo)
			pd.nodeInfo = &ni
		}
	}

	if updatedSet["peers"] {
		pd.peers = protoToPeers(msg.Peers)
		// An explicit update with an empty list means clear the peers.
		if pd.peers == nil {
			pd.peers = []statusv1alpha1.PeerStatus{}
		}
	}

	if updatedSet["routingTable"] {
		if msg.RoutingTable != nil {
			rt := protoToRoutingTable(msg.RoutingTable)
			pd.routingTable = &rt
		}
	}

	if updatedSet["healthCheck"] {
		pd.healthCheck = protoToHealthCheckStatus(msg.HealthCheck)
		// If the field is listed but nil, the sender cleared it.
		if pd.healthCheck == nil {
			pd.nullFields["healthCheck"] = true
		}
	}

	if updatedSet["nodeErrors"] {
		pd.nodeErrors = protoToNodeErrors(msg.NodeErrors)
		if pd.nodeErrors == nil {
			pd.nodeErrors = []statusv1alpha1.NodeError{}
		}
	}

	if updatedSet["bpfEntries"] {
		pd.bpfEntries = protoToBpfEntries(msg.BpfEntries)
		if pd.bpfEntries == nil {
			pd.bpfEntries = []statusv1alpha1.BpfEntry{}
		}
	}

	return pd
}

// extractNodeNameFromProtoMessage extracts the node name from a protobuf
// NodeStatusMessage for early identification on WebSocket connections.
func extractNodeNameFromProtoMessage(data []byte) string {
	var msg statusproto.NodeStatusMessage
	if err := proto.Unmarshal(data, &msg); err != nil {
		return ""
	}

	if msg.NodeName != "" {
		return msg.NodeName
	}

	if msg.Status != nil && msg.Status.NodeInfo != nil {
		return msg.Status.NodeInfo.Name
	}

	return ""
}

// handleProtoWSMessage processes a binary (protobuf) WebSocket message and
// returns the ack type string and ack struct, identical to the JSON path.
func handleProtoWSMessage(health *healthState, data []byte, source string) (string, NodeStatusPushAck) {
	var msg statusproto.NodeStatusMessage
	if err := proto.Unmarshal(data, &msg); err != nil {
		return "node_status_resync", NodeStatusPushAck{Status: "resync_required", Reason: "invalid protobuf message"}
	}

	nodeName := msg.NodeName
	if nodeName == "" && msg.Status != nil && msg.Status.NodeInfo != nil {
		nodeName = msg.Status.NodeInfo.Name
	}

	if nodeName == "" {
		return "node_status_resync", NodeStatusPushAck{Status: "resync_required", Reason: "nodeName is required"}
	}

	switch msg.Type {
	case "node_status_full":
		if msg.Status == nil {
			return "node_status_resync", NodeStatusPushAck{Status: "resync_required", Reason: "full message missing status"}
		}

		status := protoToNodeStatus(msg.Status)
		if status.NodeInfo.Name == "" {
			status.NodeInfo.Name = nodeName
		}

		rev := health.statusCache.StoreFull(nodeName, status, source)

		return "node_status_ack", NodeStatusPushAck{Status: "ok", Revision: rev}
	case "node_status_delta":
		if msg.Delta == nil || len(msg.Delta.UpdatedFields) == 0 {
			return "node_status_resync", NodeStatusPushAck{Status: "resync_required", Reason: "delta message missing delta"}
		}

		pd := protoToParsedDelta(msg.Delta)

		rev, conflict, applyErr := health.statusCache.ApplyParsedDelta(nodeName, msg.BaseRevision, pd, source)
		if applyErr != nil {
			return "node_status_resync", NodeStatusPushAck{Status: "resync_required", Reason: applyErr.Error()}
		}

		if conflict {
			return "node_status_resync", NodeStatusPushAck{Status: "resync_required", Revision: rev, Reason: "base revision mismatch"}
		}

		return "node_status_ack", NodeStatusPushAck{Status: "ok", Revision: rev}
	default:
		return "node_status_resync", NodeStatusPushAck{Status: "resync_required", Reason: "unsupported message type"}
	}
}

// handleProtoPushRequest processes an HTTP push request with protobuf body.
func handleProtoPushRequest(health *healthState, bodyBytes []byte, source string) (NodeStatusPushAck, int, error) {
	var msg statusproto.NodeStatusMessage
	if err := proto.Unmarshal(bodyBytes, &msg); err != nil {
		return NodeStatusPushAck{}, 400, fmt.Errorf("invalid protobuf body: %v", err)
	}

	nodeName := msg.NodeName
	if nodeName == "" && msg.Status != nil && msg.Status.NodeInfo != nil {
		nodeName = msg.Status.NodeInfo.Name
	}

	if nodeName == "" {
		return NodeStatusPushAck{}, 400, fmt.Errorf("nodeName is required")
	}

	ack := NodeStatusPushAck{Status: "ok"}

	switch msg.Type {
	case "node_status_full":
		if msg.Status == nil {
			return NodeStatusPushAck{}, 400, fmt.Errorf("status is required for full mode")
		}

		status := protoToNodeStatus(msg.Status)
		if status.NodeInfo.Name == "" {
			status.NodeInfo.Name = nodeName
		}

		ack.Revision = health.statusCache.StoreFull(nodeName, status, source)

		return ack, 200, nil
	case "node_status_delta":
		if msg.Delta == nil || len(msg.Delta.UpdatedFields) == 0 {
			return NodeStatusPushAck{}, 400, fmt.Errorf("delta is required for delta mode")
		}

		pd := protoToParsedDelta(msg.Delta)

		rev, conflict, applyErr := health.statusCache.ApplyParsedDelta(nodeName, msg.BaseRevision, pd, source)
		if applyErr != nil {
			return NodeStatusPushAck{}, 400, fmt.Errorf("failed to apply delta: %v", applyErr)
		}

		if conflict {
			return NodeStatusPushAck{Status: "resync_required", Revision: rev, Reason: "base revision mismatch"}, 429, nil
		}

		ack.Revision = rev

		return ack, 200, nil
	default:
		return NodeStatusPushAck{}, 400, fmt.Errorf("type must be node_status_full or node_status_delta")
	}
}

// marshalProtoAck serializes a NodeStatusPushAck into a protobuf NodeStatusAck.
func marshalProtoAck(ackType string, ack NodeStatusPushAck) ([]byte, error) {
	pbAck := &statusproto.NodeStatusAck{
		Status:   ack.Status,
		Revision: ack.Revision,
		Reason:   ack.Reason,
	}

	return proto.Marshal(pbAck)
}
