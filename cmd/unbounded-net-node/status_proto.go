// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"encoding/json"

	statusproto "github.com/Azure/unbounded-kube/internal/net/status/proto"
	statusv1alpha1 "github.com/Azure/unbounded-kube/internal/net/status/v1alpha1"
)

// nodeStatusToProto converts a Go NodeStatusResponse to the protobuf NodeStatusFull message.
func nodeStatusToProto(status *NodeStatusResponse) *statusproto.NodeStatusFull {
	if status == nil {
		return nil
	}

	full := &statusproto.NodeStatusFull{
		TimestampUnixNs: status.Timestamp.UnixNano(),
		NodeInfo:        nodeInfoToProto(&status.NodeInfo),
		Peers:           peersToProto(status.Peers),
		RoutingTable:    routingTableToProto(&status.RoutingTable),
		HealthCheck:     healthCheckStatusToProto(status.HealthCheck),
		NodeErrors:      nodeErrorsToProto(status.NodeErrors),
		FetchError:      status.FetchError,
		StatusSource:    status.StatusSource,
		NodePodInfo:     nodePodInfoToProto(status.NodePodInfo),
		BpfEntries:      bpfEntriesToProto(status.BpfEntries),
	}
	if status.LastPushTime != nil {
		full.LastPushTimeUnixNs = status.LastPushTime.UnixNano()
	}

	return full
}

// nodeStatusDeltaToProto converts a JSON delta map to the protobuf NodeStatusDelta message.
// Each key in the map represents an updated top-level field; the values are the
// JSON-encoded field contents (already produced by computeStatusDelta).
func nodeStatusDeltaToProto(delta map[string]json.RawMessage) *statusproto.NodeStatusDelta {
	if len(delta) == 0 {
		return nil
	}

	pb := &statusproto.NodeStatusDelta{
		UpdatedFields: make([]string, 0, len(delta)),
	}

	for key, raw := range delta {
		pb.UpdatedFields = append(pb.UpdatedFields, key)

		switch key {
		case "nodeInfo":
			var ni NodeInfo
			if json.Unmarshal(raw, &ni) == nil {
				pb.NodeInfo = nodeInfoToProto(&ni)
			}
		case "peers":
			var peers []statusv1alpha1.PeerStatus
			if json.Unmarshal(raw, &peers) == nil {
				pb.Peers = peersToProto(peers)
			}
		case "routingTable":
			var rt RoutingTableInfo
			if json.Unmarshal(raw, &rt) == nil {
				pb.RoutingTable = routingTableToProto(&rt)
			}
		case "healthCheck":
			var hc HealthCheckStatus
			if json.Unmarshal(raw, &hc) == nil {
				pb.HealthCheck = healthCheckStatusToProto(&hc)
			}
		case "nodeErrors":
			var errs []NodeError
			if json.Unmarshal(raw, &errs) == nil {
				pb.NodeErrors = nodeErrorsToProto(errs)
			}
		case "bpfEntries":
			var entries []BpfEntry
			if json.Unmarshal(raw, &entries) == nil {
				pb.BpfEntries = bpfEntriesToProto(entries)
			}
		}
	}

	return pb
}

// nodeInfoToProto converts a Go NodeInfo to its protobuf equivalent.
func nodeInfoToProto(ni *NodeInfo) *statusproto.NodeInfo {
	if ni == nil {
		return nil
	}

	pb := &statusproto.NodeInfo{
		Name:        ni.Name,
		SiteName:    ni.SiteName,
		IsGateway:   ni.IsGateway,
		PodCidrs:    ni.PodCIDRs,
		InternalIps: ni.InternalIPs,
		ExternalIps: ni.ExternalIPs,
		BuildInfo:   buildInfoToProto(ni.BuildInfo),
		WireGuard:   wireGuardStatusInfoToProto(ni.WireGuard),
		K8SReady:    ni.K8sReady,
		ProviderId:  ni.ProviderID,
		OsImage:     ni.OSImage,
		Kernel:      ni.Kernel,
		Kubelet:     ni.Kubelet,
		Arch:        ni.Arch,
		NodeOs:      ni.NodeOS,
		K8SLabels:   ni.K8sLabels,
	}
	if ni.K8sUpdatedAt != nil {
		pb.K8SUpdatedAtUnixNs = ni.K8sUpdatedAt.UnixNano()
	}

	return pb
}

// buildInfoToProto converts a Go BuildInfo to its protobuf equivalent.
func buildInfoToProto(bi *BuildInfo) *statusproto.BuildInfo {
	if bi == nil {
		return nil
	}

	return &statusproto.BuildInfo{
		Version:   bi.Version,
		Commit:    bi.Commit,
		BuildTime: bi.BuildTime,
	}
}

// wireGuardStatusInfoToProto converts a Go WireGuardStatusInfo to its protobuf equivalent.
func wireGuardStatusInfoToProto(wg *WireGuardStatusInfo) *statusproto.WireGuardStatusInfo {
	if wg == nil {
		return nil
	}

	return &statusproto.WireGuardStatusInfo{
		Interface:  wg.Interface,
		PublicKey:  wg.PublicKey,
		ListenPort: int32(wg.ListenPort),
		PeerCount:  int32(wg.PeerCount),
	}
}

// peersToProto converts a slice of Go PeerStatus to protobuf PeerStatus messages.
func peersToProto(peers []statusv1alpha1.PeerStatus) []*statusproto.PeerStatus {
	if len(peers) == 0 {
		return nil
	}

	result := make([]*statusproto.PeerStatus, 0, len(peers))
	for i := range peers {
		result = append(result, peerStatusToProto(&peers[i]))
	}

	return result
}

// peerStatusToProto converts a single Go PeerStatus to its protobuf equivalent.
func peerStatusToProto(p *statusv1alpha1.PeerStatus) *statusproto.PeerStatus {
	if p == nil {
		return nil
	}

	pb := &statusproto.PeerStatus{
		Name:              p.Name,
		PeerType:          p.PeerType,
		SiteName:          p.SiteName,
		PodCidrGateways:   p.PodCIDRGateways,
		SkipPodCidrRoutes: p.SkipPodCIDRRoutes,
		Tunnel:            peerTunnelStatusToProto(&p.Tunnel),
		RouteDestinations: p.RouteDestinations,
		HealthCheck:       healthCheckPeerStatusToProto(p.HealthCheck),
	}
	if len(p.RouteDistances) > 0 {
		pb.RouteDistances = make(map[string]int32, len(p.RouteDistances))
		for k, v := range p.RouteDistances {
			pb.RouteDistances[k] = int32(v)
		}
	}

	return pb
}

// peerTunnelStatusToProto converts a Go PeerTunnelStatus to its protobuf equivalent.
func peerTunnelStatusToProto(t *PeerTunnelStatus) *statusproto.PeerTunnelStatus {
	if t == nil {
		return nil
	}

	pb := &statusproto.PeerTunnelStatus{
		Protocol:   t.Protocol,
		Interface:  t.Interface,
		PublicKey:  t.PublicKey,
		Endpoint:   t.Endpoint,
		AllowedIps: t.AllowedIPs,
		RxBytes:    t.RxBytes,
		TxBytes:    t.TxBytes,
	}
	if !t.LastHandshake.IsZero() {
		pb.LastHandshakeUnixNs = t.LastHandshake.UnixNano()
	}

	return pb
}

// routingTableToProto converts a Go RoutingTableInfo to its protobuf equivalent.
func routingTableToProto(rt *RoutingTableInfo) *statusproto.RoutingTableInfo {
	if rt == nil {
		return nil
	}

	pb := &statusproto.RoutingTableInfo{
		ManagedRouteCount: int32(rt.ManagedRouteCount),
		PendingRouteCount: int32(rt.PendingRouteCount),
	}
	if len(rt.Routes) > 0 {
		pb.Routes = make([]*statusproto.RouteEntry, 0, len(rt.Routes))
		for i := range rt.Routes {
			pb.Routes = append(pb.Routes, routeEntryToProto(&rt.Routes[i]))
		}
	}

	return pb
}

// routeEntryToProto converts a Go RouteEntry to its protobuf equivalent.
func routeEntryToProto(re *RouteEntry) *statusproto.RouteEntry {
	if re == nil {
		return nil
	}

	pb := &statusproto.RouteEntry{
		Destination: re.Destination,
		Family:      re.Family,
		Table:       int32(re.Table),
	}
	if len(re.NextHops) > 0 {
		pb.NextHops = make([]*statusproto.NextHop, 0, len(re.NextHops))
		for i := range re.NextHops {
			pb.NextHops = append(pb.NextHops, nextHopToProto(&re.NextHops[i]))
		}
	}

	return pb
}

// nextHopToProto converts a Go NextHop to its protobuf equivalent.
func nextHopToProto(nh *NextHop) *statusproto.NextHop {
	if nh == nil {
		return nil
	}

	pb := &statusproto.NextHop{
		Gateway:          nh.Gateway,
		Device:           nh.Device,
		Distance:         int32(nh.Distance),
		Weight:           int32(nh.Weight),
		Mtu:              int32(nh.MTU),
		PeerDestinations: nh.PeerDestinations,
		Info:             nextHopInfoToProto(nh.Info),
	}
	if len(nh.RouteTypes) > 0 {
		pb.RouteTypes = make([]*statusproto.RouteType, 0, len(nh.RouteTypes))
		for i := range nh.RouteTypes {
			pb.RouteTypes = append(pb.RouteTypes, routeTypeToProto(&nh.RouteTypes[i]))
		}
	}

	if nh.Expected != nil {
		pb.Expected = &statusproto.OptionalBool{Value: *nh.Expected}
	}

	if nh.Present != nil {
		pb.Present = &statusproto.OptionalBool{Value: *nh.Present}
	}

	return pb
}

// nextHopInfoToProto converts a Go NextHopInfo to its protobuf equivalent.
func nextHopInfoToProto(info *NextHopInfo) *statusproto.NextHopInfo {
	if info == nil {
		return nil
	}

	return &statusproto.NextHopInfo{
		ObjectName: info.ObjectName,
		ObjectType: info.ObjectType,
		RouteType:  info.RouteType,
	}
}

// routeTypeToProto converts a Go RouteType to its protobuf equivalent.
func routeTypeToProto(rt *RouteType) *statusproto.RouteType {
	if rt == nil {
		return nil
	}

	return &statusproto.RouteType{
		Type:       rt.Type,
		Attributes: rt.Attributes,
	}
}

// healthCheckStatusToProto converts a Go HealthCheckStatus to its protobuf equivalent.
func healthCheckStatusToProto(hc *HealthCheckStatus) *statusproto.HealthCheckStatus {
	if hc == nil {
		return nil
	}

	return &statusproto.HealthCheckStatus{
		Healthy:         hc.Healthy,
		Summary:         hc.Summary,
		PeerCount:       int32(hc.PeerCount),
		CheckedAtUnixNs: hc.CheckedAt.UnixNano(),
	}
}

// healthCheckPeerStatusToProto converts a Go HealthCheckPeerStatus to its protobuf equivalent.
func healthCheckPeerStatusToProto(hc *HealthCheckPeerStatus) *statusproto.HealthCheckPeerStatus {
	if hc == nil {
		return nil
	}

	return &statusproto.HealthCheckPeerStatus{
		Enabled: hc.Enabled,
		Status:  hc.Status,
		Uptime:  hc.Uptime,
		Rtt:     hc.RTT,
	}
}

// nodeErrorsToProto converts a slice of Go NodeError to protobuf NodeError messages.
func nodeErrorsToProto(errs []NodeError) []*statusproto.NodeError {
	if len(errs) == 0 {
		return nil
	}

	result := make([]*statusproto.NodeError, 0, len(errs))
	for _, e := range errs {
		pe := &statusproto.NodeError{
			Type:    e.Type,
			Message: e.Message,
		}
		if !e.Timestamp.IsZero() {
			pe.TimestampNs = e.Timestamp.UnixNano()
		}

		result = append(result, pe)
	}

	return result
}

// nodePodInfoToProto converts a Go NodePodInfo to its protobuf equivalent.
func nodePodInfoToProto(npi *statusv1alpha1.NodePodInfo) *statusproto.NodePodInfo {
	if npi == nil {
		return nil
	}

	return &statusproto.NodePodInfo{
		PodName:         npi.PodName,
		StartTimeUnixNs: npi.StartTime.UnixNano(),
		Restarts:        npi.Restarts,
	}
}

// bpfEntriesToProto converts a slice of BpfEntry to protobuf BpfEntry messages.
func bpfEntriesToProto(entries []statusv1alpha1.BpfEntry) []*statusproto.BpfEntry {
	if len(entries) == 0 {
		return nil
	}

	result := make([]*statusproto.BpfEntry, len(entries))
	for i, e := range entries {
		result[i] = &statusproto.BpfEntry{
			Cidr:          e.CIDR,
			Remote:        e.Remote,
			Node:          e.Node,
			InterfaceName: e.Interface,
			Protocol:      e.Protocol,
			Healthy:       e.Healthy,
			Vni:           e.VNI,
			Mtu:           int32(e.MTU),
			Ifindex:       e.IfIndex,
		}
	}

	return result
}
