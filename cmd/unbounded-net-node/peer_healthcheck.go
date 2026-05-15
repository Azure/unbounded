// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"fmt"
	"net"

	"k8s.io/klog/v2"

	"github.com/Azure/unbounded/internal/net/healthcheck"
)

// registerPeersWithHealthCheck registers mesh and gateway peers with the
// healthcheck manager, resolving HC profiles for each peer. It sets
// state.meshPeerHealthCheckEnabled and state.gatewayPeerHealthCheckEnabled
// for peers that are registered.
//
// peerIfaceNameFn returns the tunnel interface name for a gateway peer (e.g.
// "wg51822" for WireGuard or "gn2886729990" for GENEVE). Returning "" skips
// the peer.
//
// useSiteFallbackForGateway enables falling back to the site-level HC profile
// when no pool/assignment-level profile is found for a gateway peer. This is
// used by GENEVE which has no WireGuard handshake as a liveness signal.
//
// Returns the set of peer names that were registered (desiredHCPeers).
func registerPeersWithHealthCheck(
	meshPeers []meshPeerInfo,
	gatewayPeers []gatewayPeerInfo,
	mySiteName string,
	isGatewayNode bool,
	siteHCProfileNames map[string]string,
	peeringHCProfileNames map[string]string,
	assignmentSiteHCProfileNames map[string]string,
	assignmentPoolHCProfileNames map[string]string,
	poolHCProfileNames map[string]string,
	state *wireGuardState,
	peerIfaceNameFn func(gatewayPeerInfo) string,
	useSiteFallbackForGateway bool,
) map[string]bool {
	desiredHCPeers := make(map[string]bool)
	if state.healthCheckManager == nil {
		return desiredHCPeers
	}

	// Mesh peers.
	for _, peer := range meshPeers {
		overlayIP := getHealthIPFromPodCIDRs(peer.PodCIDRs)
		if overlayIP == "" {
			continue
		}

		hcProfileName := resolveMeshPeerHealthCheckProfileName(isGatewayNode, peer, mySiteName,
			siteHCProfileNames, peeringHCProfileNames, assignmentSiteHCProfileNames)
		if hcProfileName == "" {
			continue
		}

		desiredHCPeers[peer.Name] = true
		if peer.WireGuardPublicKey != "" {
			state.mu.Lock()
			state.meshPeerHealthCheckEnabled[peer.WireGuardPublicKey] = true
			state.mu.Unlock()
		}

		settings := healthcheck.DefaultSettings()
		if state.healthFlapMaxBackoff > 0 {
			settings.MaxBackoff = state.healthFlapMaxBackoff
		}

		if err := state.healthCheckManager.AddPeer(peer.Name, net.ParseIP(overlayIP), settings); err != nil {
			klog.V(2).Infof("Healthcheck: failed to register mesh peer %s at %s: %v", peer.Name, overlayIP, err)
		} else {
			klog.V(4).Infof("Healthcheck: registered mesh peer %s at %s", peer.Name, overlayIP)
		}
	}

	// Gateway peers.
	for _, gwPeer := range gatewayPeers {
		overlayIP := getHealthIPFromPodCIDRs(gwPeer.PodCIDRs)
		if overlayIP == "" {
			continue
		}

		ifName := peerIfaceNameFn(gwPeer)
		if ifName == "" {
			continue
		}

		hcProfileName := resolveGatewayPeerHealthCheckProfileName(isGatewayNode, mySiteName, gwPeer,
			assignmentPoolHCProfileNames, poolHCProfileNames)
		if hcProfileName == "" && useSiteFallbackForGateway {
			hcProfileName = siteHCProfileNames[mySiteName]
		}

		if hcProfileName == "" {
			continue
		}

		desiredHCPeers[gwPeer.Name] = true

		state.mu.Lock()
		state.gatewayPeerHealthCheckEnabled[ifName] = true
		state.mu.Unlock()

		settings := healthcheck.DefaultSettings()
		if state.healthFlapMaxBackoff > 0 {
			settings.MaxBackoff = state.healthFlapMaxBackoff
		}

		if err := state.healthCheckManager.AddPeer(gwPeer.Name, net.ParseIP(overlayIP), settings); err != nil {
			klog.V(2).Infof("Healthcheck: failed to register gateway peer %s at %s: %v", gwPeer.Name, overlayIP, err)
		} else {
			klog.V(4).Infof("Healthcheck: registered gateway peer %s at %s (iface %s)", gwPeer.Name, overlayIP, ifName)
		}
	}

	return desiredHCPeers
}

// peerIfaceNameWireGuard maps a gateway peer to its WireGuard interface name
// (wg<port>). Returns "" for peers with no port.
func peerIfaceNameWireGuard(gwPeer gatewayPeerInfo) string {
	if gwPeer.GatewayWireguardPort == 0 {
		return ""
	}

	return fmt.Sprintf("wg%d", gwPeer.GatewayWireguardPort)
}
