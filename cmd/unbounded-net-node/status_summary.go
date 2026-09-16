// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"time"

	"k8s.io/klog/v2"

	"github.com/Azure/unbounded/internal/net/routeplan"
	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

type NodeStatusOverview = statusv1alpha1.NodeStatusOverview

// getNodeSummary collects overview facts directly. Only route-planning inputs
// survive peer visitation; full peer, route, and BPF snapshots are never built.
func (s *nodeStatusServer) getNodeSummary() *NodeStatusOverview {
	summary := &NodeStatusOverview{}

	var routePeers []routeplan.Peer

	interfaceHealthy := make(map[string]bool)
	now := time.Now()
	facts := s.inspectNodePeers(func(peer WireGuardPeerStatus) {
		summary.PeerCount++
		if summaryPeerHealthy(peer, now) {
			summary.HealthyPeers++
		}

		previous, seen := interfaceHealthy[peer.Tunnel.Interface]
		interfaceHealthy[peer.Tunnel.Interface] = (!seen || previous) && peerStatusHealthy(peer, now)
		routePeers = append(routePeers, routeplan.Peer{
			Name: peer.Name, PeerType: peer.PeerType, SiteName: peer.SiteName,
			SkipPodCIDRRoutes: peer.SkipPodCIDRRoutes,
			Interface:         peer.Tunnel.Interface, Endpoint: peer.Tunnel.Endpoint,
			PodCIDRGateways: peer.PodCIDRGateways, AllowedIPs: peer.Tunnel.AllowedIPs,
			RouteDistances: peer.RouteDistances,
		})
	})
	summary.Timestamp = facts.Timestamp
	summary.NodeInfo = facts.NodeInfo
	summary.NodeErrors = facts.NodeErrors
	summary.HealthCheck = facts.HealthCheck
	summary.RouteCount, summary.RouteMismatch = s.collectRouteSummary(routePeers, facts.NodeInfo.SiteName)

	if s.state.linkStatsMonitor != nil {
		for _, warning := range s.state.linkStatsMonitor.GetWarnings() {
			if !suppressHealthyInterfaceRxErrors(warning, func(iface string) bool { return interfaceHealthy[iface] }) {
				summary.NodeErrors = append(summary.NodeErrors, NodeError{Type: "link-stats", Message: warning})
			}
		}
	}

	return summary
}

// This is deliberately stricter than link-warning suppression: the controller
// counts only "up"/"Up" when enabled, and otherwise uses handshake freshness.
func summaryPeerHealthy(peer WireGuardPeerStatus, now time.Time) bool {
	if peer.HealthCheck != nil && peer.HealthCheck.Enabled {
		return peer.HealthCheck.Status == "up" || peer.HealthCheck.Status == "Up"
	}

	return !peer.Tunnel.LastHandshake.IsZero() && now.Sub(peer.Tunnel.LastHandshake) < 3*time.Minute
}

func (h *nodeHealthState) getSummarySnapshot() *NodeStatusOverview {
	h.mu.RLock()
	srv := h.statusServer

	summary := &NodeStatusOverview{
		Timestamp: time.Now(),
		NodeInfo: NodeInfo{
			Name: h.nodeName, SiteName: h.siteName, IsGateway: h.isGateway,
			PodCIDRs: append([]string(nil), h.podCIDRs...), BuildInfo: nodeAgentBuildInfo(),
		},
	}
	if h.pubKey != "" {
		summary.NodeInfo.WireGuard = &WireGuardStatusInfo{PublicKey: h.pubKey}
	}

	cniManaged, cniReady, cniReason := h.cniManaged, h.cniReady, h.cniReason
	transientErrors := append([]NodeError(nil), h.transientErrors...)
	h.mu.RUnlock()

	if srv != nil {
		summary = srv.getNodeSummary()
	}

	summary.NodeErrors = mergeNodeErrors(summary.NodeErrors, filterExpiredNodeErrors(transientErrors, time.Now(), time.Minute))

	summary.NodeErrors = removeNodeErrorsByType(summary.NodeErrors, configPodCIDRGuard)
	if cniManaged && !cniReady && cniReason != "" {
		summary.NodeErrors = append(summary.NodeErrors, NodeError{Type: configPodCIDRGuard, Message: cniReason})
	}

	return summary
}

func (h *nodeHealthState) handleStatusSummary(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(h.getSummarySnapshot()); err != nil {
		klog.V(4).Infof("status summary json encode failed: %v", err)
	}
}
