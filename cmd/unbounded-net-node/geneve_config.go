// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"

	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
	unboundednetnetlink "github.com/Azure/unbounded/internal/net/netlink"
)

// configureTunnelPeers configures GENEVE/VXLAN/IPIP/None peers in the
// shared flow-based interfaces and programs their BPF map entries.
func configureTunnelPeers(ctx context.Context, cfg *config, meshPeers []meshPeerInfo, gatewayPeers []gatewayPeerInfo, mySiteName string, peeredSites, networkPeeredSites map[string]bool, siteHealthCheckProfileNames, peeringSiteHealthCheckProfileNames, assignmentSiteHealthCheckProfileNames, assignmentPoolHealthCheckProfileNames, poolHealthCheckProfileNames map[string]string, siteTunnelMTUs, peeringSiteTunnelMTUs, assignmentSiteTunnelMTUs, assignmentPoolTunnelMTUs, poolTunnelMTUs map[string]int, state *wireGuardState) ([]unboundednetnetlink.DesiredRoute, map[string]bool, error) {
	return configureEBPFTunnelPeers(ctx, cfg, meshPeers, gatewayPeers,
		mySiteName, peeredSites, networkPeeredSites,
		siteHealthCheckProfileNames, peeringSiteHealthCheckProfileNames,
		assignmentSiteHealthCheckProfileNames, assignmentPoolHealthCheckProfileNames,
		poolHealthCheckProfileNames,
		siteTunnelMTUs, peeringSiteTunnelMTUs, assignmentSiteTunnelMTUs,
		assignmentPoolTunnelMTUs, poolTunnelMTUs, state)
}

// filterPeersByTunnelProtocol partitions mesh and gateway peer lists into
// WireGuard peers, per-peer tunnel peers (GENEVE, IPIP, None), and VXLAN
// peers. VXLAN peers use a single shared interface and are handled separately
// by configureVXLANPeers.
func filterPeersByTunnelProtocol(meshPeers []meshPeerInfo, gatewayPeers []gatewayPeerInfo) ([]meshPeerInfo, []gatewayPeerInfo, []meshPeerInfo, []gatewayPeerInfo, []meshPeerInfo, []gatewayPeerInfo) {
	var (
		wgMesh, tunnelMesh, vxlanMesh []meshPeerInfo
		wgGw, tunnelGw, vxlanGw       []gatewayPeerInfo
	)

	for _, p := range meshPeers {
		switch unboundednetv1alpha1.TunnelProtocol(p.TunnelProtocol) {
		case unboundednetv1alpha1.TunnelProtocolGENEVE, unboundednetv1alpha1.TunnelProtocolIPIP, unboundednetv1alpha1.TunnelProtocolNone:
			tunnelMesh = append(tunnelMesh, p)
		case unboundednetv1alpha1.TunnelProtocolVXLAN:
			vxlanMesh = append(vxlanMesh, p)
		default:
			wgMesh = append(wgMesh, p)
		}
	}

	for _, p := range gatewayPeers {
		switch unboundednetv1alpha1.TunnelProtocol(p.TunnelProtocol) {
		case unboundednetv1alpha1.TunnelProtocolGENEVE, unboundednetv1alpha1.TunnelProtocolIPIP, unboundednetv1alpha1.TunnelProtocolNone:
			tunnelGw = append(tunnelGw, p)
		case unboundednetv1alpha1.TunnelProtocolVXLAN:
			vxlanGw = append(vxlanGw, p)
		default:
			wgGw = append(wgGw, p)
		}
	}

	return wgMesh, wgGw, tunnelMesh, tunnelGw, vxlanMesh, vxlanGw
}
