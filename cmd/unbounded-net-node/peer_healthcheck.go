// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"net"
	"time"

	"k8s.io/klog/v2"

	"github.com/Azure/unbounded/internal/net/healthcheck"
)

// A disabled association must block fallback to less-specific enabled profiles.
const disabledHealthCheckProfile = "disabled"

var errRegisterHealthChecks = errors.New("health check registration failed")

func resolvedHealthCheckSettings(name string, profiles map[string]healthcheck.HealthCheckSettings, maxBackoff time.Duration) (healthcheck.HealthCheckSettings, bool, error) {
	if name == "" || name == disabledHealthCheckProfile {
		return healthcheck.HealthCheckSettings{}, false, nil
	}

	settings, ok := profiles[name]
	if !ok {
		return healthcheck.HealthCheckSettings{}, false, fmt.Errorf("health check profile %q is missing from the current reconciliation", name)
	}

	if maxBackoff > 0 {
		settings.MaxBackoff = maxBackoff
	}

	return settings, true, nil
}

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
// Returns desired peer names and registration errors. On error, names are retained
// so a failed configuration does not remove an existing healthy session.
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
	profiles map[string]healthcheck.HealthCheckSettings,
	state *wireGuardState,
	peerIfaceNameFn func(gatewayPeerInfo) string,
	useSiteFallbackForGateway bool,
) (map[string]bool, error) {
	desiredHCPeers := make(map[string]bool)
	if state.healthCheckManager == nil {
		return desiredHCPeers, nil
	}

	var registrationErrors []error

	// Mesh peers.
	for _, peer := range meshPeers {
		overlayIP := getHealthIPFromPodCIDRs(peer.PodCIDRs)
		if overlayIP == "" {
			continue
		}

		hcProfileName := resolveMeshPeerHealthCheckProfileName(isGatewayNode, peer, mySiteName,
			siteHCProfileNames, peeringHCProfileNames, assignmentSiteHCProfileNames)

		settings, enabled, err := resolvedHealthCheckSettings(hcProfileName, profiles, state.healthFlapMaxBackoff)
		if err != nil {
			desiredHCPeers[peer.Name] = true
			registrationErrors = append(registrationErrors, fmt.Errorf("mesh peer %s: %w", peer.Name, err))

			continue
		}

		if !enabled {
			continue
		}

		desiredHCPeers[peer.Name] = true
		if peer.WireGuardPublicKey != "" {
			state.mu.Lock()
			state.meshPeerHealthCheckEnabled[peer.WireGuardPublicKey] = true
			state.mu.Unlock()
		}

		if err := state.healthCheckManager.AddPeer(peer.Name, net.ParseIP(overlayIP), settings); err != nil {
			registrationErrors = append(registrationErrors, fmt.Errorf("register mesh peer %s at %s: %w", peer.Name, overlayIP, err))
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

		settings, enabled, err := resolvedHealthCheckSettings(hcProfileName, profiles, state.healthFlapMaxBackoff)
		if err != nil {
			desiredHCPeers[gwPeer.Name] = true
			registrationErrors = append(registrationErrors, fmt.Errorf("gateway peer %s: %w", gwPeer.Name, err))

			continue
		}

		if !enabled {
			continue
		}

		desiredHCPeers[gwPeer.Name] = true

		state.mu.Lock()
		state.gatewayPeerHealthCheckEnabled[ifName] = true
		state.mu.Unlock()

		if err := state.healthCheckManager.AddPeer(gwPeer.Name, net.ParseIP(overlayIP), settings); err != nil {
			registrationErrors = append(registrationErrors, fmt.Errorf("register gateway peer %s at %s: %w", gwPeer.Name, overlayIP, err))
		} else {
			klog.V(4).Infof("Healthcheck: registered gateway peer %s at %s (iface %s)", gwPeer.Name, overlayIP, ifName)
		}
	}

	if len(registrationErrors) > 0 {
		return desiredHCPeers, fmt.Errorf("%w: %w", errRegisterHealthChecks, errors.Join(registrationErrors...))
	}

	return desiredHCPeers, nil
}

// peerIfaceNameWireGuard maps a gateway peer to its WireGuard interface name
// (<prefix><port>). Returns "" for peers with no port.
func peerIfaceNameWireGuard(cfg *config, gwPeer gatewayPeerInfo) string {
	if gwPeer.GatewayWireguardPort == 0 {
		return ""
	}

	return wireGuardInterfaceName(cfg, int(gwPeer.GatewayWireguardPort))
}
