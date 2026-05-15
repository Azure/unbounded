// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"

	unboundednetnetlink "github.com/Azure/unbounded/internal/net/netlink"
)

// vxlanInterfaceName is the shared flow-based VXLAN interface used by the
// eBPF dataplane. All VXLAN peers share this single interface.
const vxlanInterfaceName = "vxlan0"

// configureVXLANPeers configures VXLAN peers on the shared vxlan0 interface
// and programs their BPF map entries.
func configureVXLANPeers(
	ctx context.Context,
	cfg *config,
	meshPeers []meshPeerInfo,
	gatewayPeers []gatewayPeerInfo,
	mySiteName string,
	peeredSites map[string]bool,
	siteHealthCheckProfileNames map[string]string,
	peeringSiteHealthCheckProfileNames map[string]string,
	assignmentSiteHealthCheckProfileNames map[string]string,
	assignmentPoolHealthCheckProfileNames map[string]string,
	poolHealthCheckProfileNames map[string]string,
	siteTunnelMTUs map[string]int,
	peeringSiteTunnelMTUs map[string]int,
	assignmentSiteTunnelMTUs map[string]int,
	assignmentPoolTunnelMTUs map[string]int,
	poolTunnelMTUs map[string]int,
	state *wireGuardState,
) ([]unboundednetnetlink.DesiredRoute, map[string]bool, error) {
	return configureEBPFVXLANPeers(ctx, cfg, meshPeers, gatewayPeers,
		mySiteName, peeredSites,
		siteHealthCheckProfileNames, peeringSiteHealthCheckProfileNames,
		assignmentSiteHealthCheckProfileNames, assignmentPoolHealthCheckProfileNames,
		poolHealthCheckProfileNames,
		siteTunnelMTUs, peeringSiteTunnelMTUs, assignmentSiteTunnelMTUs,
		assignmentPoolTunnelMTUs, poolTunnelMTUs, state)
}
