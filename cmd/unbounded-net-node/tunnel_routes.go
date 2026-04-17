// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"fmt"
	"net"
	"strings"

	"k8s.io/klog/v2"

	"github.com/Azure/unbounded-kube/internal/net/healthcheck"
	unboundednetnetlink "github.com/Azure/unbounded-kube/internal/net/netlink"
	"github.com/Azure/unbounded-kube/internal/net/routeplan"
)

// buildMeshPeerRoutes builds DesiredRoute entries for a single mesh peer.
// It derives nexthop IPs from the peer's pod CIDRs and creates:
//   - Bootstrap /32 and /128 host routes (link-scope, HealthCheckImmune, with cfg.BaseMetric)
//   - Pod CIDR routes via gateway nexthop (if !peer.SkipPodCIDRRoutes)
//   - Internal IP host routes for non-peered sites
//
// Routes targeting local pod CIDRs or local gateway host CIDRs are skipped.
// peerIDSuffix is the tunnel interface name (e.g. "wg51820" or "gn2886729990").
func buildMeshPeerRoutes(peer meshPeerInfo, linkIndex int, peerIDSuffix, mySiteName string, peeredSites map[string]bool, cfg *config, peerMTU int, localPodCIDRs, localGatewayHostCIDRs map[string]struct{}) []unboundednetnetlink.DesiredRoute {
	// Derive nexthop IPs from pod CIDRs.
	var peerNHv4, peerNHv6 string

	for _, podCIDR := range peer.PodCIDRs {
		gwIP := getGatewayIPFromCIDR(podCIDR)
		if gwIP == nil {
			continue
		}

		if gwIP.To4() != nil && peerNHv4 == "" {
			peerNHv4 = gwIP.String()
		} else if gwIP.To4() == nil && peerNHv6 == "" {
			peerNHv6 = gwIP.String()
		}
	}

	peerID := peer.Name + "/" + peerIDSuffix
	nhForCIDR := func(cidr string) string {
		if strings.Contains(cidr, ":") {
			return peerNHv6
		}

		return peerNHv4
	}

	var routes []unboundednetnetlink.DesiredRoute

	// Bootstrap host routes (link-scope, immune to healthcheck).
	if peerNHv4 != "" {
		hostCIDR := peerNHv4 + "/32"
		if _, isLocal := localGatewayHostCIDRs[hostCIDR]; !isLocal {
			if _, prefix, _ := net.ParseCIDR(hostCIDR); prefix != nil { //nolint:errcheck
				routes = append(routes, unboundednetnetlink.DesiredRoute{
					Prefix:            *prefix,
					Nexthops:          []unboundednetnetlink.DesiredNexthop{{PeerID: peerID, LinkIndex: linkIndex}},
					Metric:            cfg.BaseMetric,
					MTU:               peerMTU,
					HealthCheckImmune: true,
				})
			}
		}
	}

	if peerNHv6 != "" {
		hostCIDR := peerNHv6 + "/128"
		if _, isLocal := localGatewayHostCIDRs[hostCIDR]; !isLocal {
			if _, prefix, _ := net.ParseCIDR(hostCIDR); prefix != nil { //nolint:errcheck
				routes = append(routes, unboundednetnetlink.DesiredRoute{
					Prefix:            *prefix,
					Nexthops:          []unboundednetnetlink.DesiredNexthop{{PeerID: peerID, LinkIndex: linkIndex}},
					Metric:            cfg.BaseMetric,
					MTU:               peerMTU,
					HealthCheckImmune: true,
				})
			}
		}
	}

	// Pod CIDR routes via gateway nexthop.
	if !peer.SkipPodCIDRRoutes {
		for _, cidr := range peer.PodCIDRs {
			normalizedCIDR := routeplan.NormalizeCIDR(cidr)
			if normalizedCIDR == "" {
				continue
			}

			if _, isLocal := localPodCIDRs[normalizedCIDR]; isLocal {
				continue
			}

			nh := nhForCIDR(cidr)

			gwIP := net.ParseIP(nh)
			if gwIP == nil {
				continue
			}

			if _, prefix, _ := net.ParseCIDR(normalizedCIDR); prefix != nil { //nolint:errcheck
				routes = append(routes, unboundednetnetlink.DesiredRoute{
					Prefix:   *prefix,
					Nexthops: []unboundednetnetlink.DesiredNexthop{{PeerID: peerID, LinkIndex: linkIndex, Gateway: gwIP}},
					Metric:   cfg.BaseMetric,
					MTU:      peerMTU,
				})
			}
		}
	}

	// Internal IP host routes for non-peered sites.
	if !peeredSites[peer.SiteName] {
		for _, cidr := range ipsToHostCIDRs(peer.InternalIPs) {
			nh := nhForCIDR(cidr)

			gwIP := net.ParseIP(nh)
			if gwIP == nil {
				continue
			}

			if _, prefix, _ := net.ParseCIDR(cidr); prefix != nil { //nolint:errcheck
				routes = append(routes, unboundednetnetlink.DesiredRoute{
					Prefix:   *prefix,
					Nexthops: []unboundednetnetlink.DesiredNexthop{{PeerID: peerID, LinkIndex: linkIndex, Gateway: gwIP}},
					Metric:   cfg.BaseMetric,
					MTU:      peerMTU,
				})
			}
		}
	}

	return routes
}

// buildGatewayPeerRoutes builds DesiredRoute entries for a single gateway peer.
// It derives nexthop IPs from the peer's pod CIDRs and creates:
//   - Bootstrap /32 and /128 host routes (link-scope, HealthCheckImmune)
//   - Routed CIDR routes with per-distance metrics
//   - Pod CIDR routes (if !gwPeer.SkipPodCIDRRoutes), deduplicated against routed CIDRs
//
// Routes targeting local pod CIDRs or local gateway host CIDRs are skipped.
// peerIDSuffix is the tunnel interface name (e.g. "wg51822" or "gn2886729990").
func buildGatewayPeerRoutes(gwPeer gatewayPeerInfo, linkIndex int, peerIDSuffix, mySiteName string, cfg *config, gwPeerMTU int, localPodCIDRs, localGatewayHostCIDRs map[string]struct{}) []unboundednetnetlink.DesiredRoute {
	// Derive nexthop IPs from pod CIDRs.
	var gwNHv4, gwNHv6 string

	for _, podCIDR := range gwPeer.PodCIDRs {
		gwIP := getGatewayIPFromCIDR(podCIDR)
		if gwIP == nil {
			continue
		}

		if gwIP.To4() != nil && gwNHv4 == "" {
			gwNHv4 = gwIP.String()
		} else if gwIP.To4() == nil && gwNHv6 == "" {
			gwNHv6 = gwIP.String()
		}
	}

	if gwNHv4 == "" && gwNHv6 == "" {
		klog.Warningf("Gateway peer %s has no podCIDRs; routes will use interface-only nexthop (no healthcheck)", gwPeer.Name)
	}

	peerID := gwPeer.Name + "/" + peerIDSuffix
	nhForCIDR := func(cidr string) string {
		if strings.Contains(cidr, ":") {
			return gwNHv6
		}

		return gwNHv4
	}

	var routes []unboundednetnetlink.DesiredRoute

	// Bootstrap host routes (link-scope, immune to healthcheck).
	if gwNHv4 != "" {
		hostCIDR := gwNHv4 + "/32"
		if _, isLocal := localGatewayHostCIDRs[hostCIDR]; !isLocal {
			if _, prefix, _ := net.ParseCIDR(hostCIDR); prefix != nil { //nolint:errcheck
				routes = append(routes, unboundednetnetlink.DesiredRoute{
					Prefix:            *prefix,
					Nexthops:          []unboundednetnetlink.DesiredNexthop{{PeerID: peerID, LinkIndex: linkIndex}},
					Metric:            cfg.BaseMetric,
					MTU:               gwPeerMTU,
					HealthCheckImmune: true,
				})
			}
		}
	}

	if gwNHv6 != "" {
		hostCIDR := gwNHv6 + "/128"
		if _, isLocal := localGatewayHostCIDRs[hostCIDR]; !isLocal {
			if _, prefix, _ := net.ParseCIDR(hostCIDR); prefix != nil { //nolint:errcheck
				routes = append(routes, unboundednetnetlink.DesiredRoute{
					Prefix:            *prefix,
					Nexthops:          []unboundednetnetlink.DesiredNexthop{{PeerID: peerID, LinkIndex: linkIndex}},
					Metric:            cfg.BaseMetric,
					MTU:               gwPeerMTU,
					HealthCheckImmune: true,
				})
			}
		}
	}

	// Routed CIDRs with per-distance metrics.
	routedCIDRSet := make(map[string]struct{}, len(gwPeer.RoutedCidrs))
	for _, cidr := range gwPeer.RoutedCidrs {
		normalizedCIDR := routeplan.NormalizeCIDR(cidr)
		if normalizedCIDR == "" {
			continue
		}

		if _, isLocal := localPodCIDRs[normalizedCIDR]; isLocal {
			continue
		}

		routedCIDRSet[normalizedCIDR] = struct{}{}
		nh := nhForCIDR(cidr)

		gwIP := net.ParseIP(nh)
		if gwIP == nil {
			continue
		}

		if _, prefix, _ := net.ParseCIDR(normalizedCIDR); prefix != nil { //nolint:errcheck
			metric := cfg.BaseMetric

			if gwPeer.RouteDistances != nil {
				if d := gwPeer.RouteDistances[cidr]; d > 0 {
					metric = d
				}
			}

			routes = append(routes, unboundednetnetlink.DesiredRoute{
				Prefix:   *prefix,
				Nexthops: []unboundednetnetlink.DesiredNexthop{{PeerID: peerID, LinkIndex: linkIndex, Gateway: gwIP}},
				Metric:   metric,
				MTU:      gwPeerMTU,
			})
		}
	}

	// Pod CIDR routes, deduplicated against routed CIDRs.
	if !gwPeer.SkipPodCIDRRoutes {
		for _, cidr := range gwPeer.PodCIDRs {
			normalizedCIDR := routeplan.NormalizeCIDR(cidr)
			if normalizedCIDR == "" {
				continue
			}

			if _, isLocal := localPodCIDRs[normalizedCIDR]; isLocal {
				continue
			}

			if _, exists := routedCIDRSet[normalizedCIDR]; exists {
				continue
			}

			nh := nhForCIDR(cidr)

			gwIP := net.ParseIP(nh)
			if gwIP == nil {
				continue
			}

			if _, prefix, _ := net.ParseCIDR(normalizedCIDR); prefix != nil { //nolint:errcheck
				routes = append(routes, unboundednetnetlink.DesiredRoute{
					Prefix:   *prefix,
					Nexthops: []unboundednetnetlink.DesiredNexthop{{PeerID: peerID, LinkIndex: linkIndex, Gateway: gwIP}},
					Metric:   cfg.BaseMetric,
					MTU:      gwPeerMTU,
				})
			}
		}
	}

	return routes
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

// peerIfaceNameWireGuard returns a function that maps a gateway peer to its
// WireGuard interface name (wg<port>). Returns "" for peers with no port.
func peerIfaceNameWireGuard(gwPeer gatewayPeerInfo) string {
	if gwPeer.GatewayWireguardPort == 0 {
		return ""
	}

	return fmt.Sprintf("wg%d", gwPeer.GatewayWireguardPort)
}

// peerIfaceNameGeneve returns a function that maps a gateway peer to its
// GENEVE interface name (gn<decimal IP>). Returns "" for peers with no
// internal IPs.
func peerIfaceNameGeneve(gwPeer gatewayPeerInfo) string {
	if len(gwPeer.InternalIPs) == 0 {
		return ""
	}

	ip := net.ParseIP(gwPeer.InternalIPs[0])
	if ip == nil {
		return ""
	}

	return geneveIfaceName(ip)
}
