// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"net"
	"strings"

	"k8s.io/klog/v2"

	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
	unboundednetnetlink "github.com/Azure/unbounded/internal/net/netlink"
	"github.com/Azure/unbounded/internal/net/routeplan"
)

// vxlanInterfaceName is the fixed name for the shared external/flow-based
// VXLAN interface. Unlike GENEVE/IPIP, all VXLAN peers share this single
// interface; per-peer routing is done via lwt encap ip on each route.
const vxlanInterfaceName = "vxlan0"

// configureVXLANPeers creates a single external/flow-based vxlan0 interface,
// assigns the node's pod CIDR gateway IPs to it, and builds routes with
// lightweight tunnel encap ip directives for each peer. This is fundamentally
// different from GENEVE/IPIP which create per-peer interfaces.
// When cfg.TunnelDataplane is "ebpf", uses an eBPF LPM trie map for endpoint
// resolution instead of lwt encap directives on routes.
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
	// eBPF dataplane: use BPF map instead of lwt encap.
	if cfg.TunnelDataplane == "ebpf" {
		return configureEBPFVXLANPeers(ctx, cfg, meshPeers, gatewayPeers,
			mySiteName, peeredSites,
			siteHealthCheckProfileNames, peeringSiteHealthCheckProfileNames,
			assignmentSiteHealthCheckProfileNames, assignmentPoolHealthCheckProfileNames,
			poolHealthCheckProfileNames,
			siteTunnelMTUs, peeringSiteTunnelMTUs, assignmentSiteTunnelMTUs,
			assignmentPoolTunnelMTUs, poolTunnelMTUs, state)
	}

	_ = ctx
	_ = unboundednetv1alpha1.TunnelProtocolVXLAN // compile-time reference

	if len(meshPeers) == 0 && len(gatewayPeers) == 0 {
		return nil, nil, nil
	}

	// Ensure the shared vxlan0 interface exists.
	lm := unboundednetnetlink.NewLinkManager(vxlanInterfaceName)
	if err := lm.EnsureVXLANInterface(cfg.VXLANPort, cfg.VXLANSrcPortLow, cfg.VXLANSrcPortHigh); err != nil {
		return nil, nil, err
	}

	if err := lm.SetLinkUp(); err != nil {
		return nil, nil, err
	}
	// Disable rp_filter on vxlan0 -- decapsulated inbound packets arrive
	// here with overlay source IPs, but overlay routes point elsewhere.
	// Both strict and loose rp_filter would drop them.
	disableRPFilter(vxlanInterfaceName)
	ensureTunnelForwardAccept(vxlanInterfaceName)

	// Assign this node's pod CIDR gateway IPs to vxlan0 so that
	// the kernel can source packets from the overlay addresses.
	var gatewayAddrs []string

	for _, cidr := range state.nodePodCIDRs {
		gwIP := getGatewayIPFromCIDR(cidr)
		if gwIP == nil {
			continue
		}

		if gwIP.To4() != nil {
			gatewayAddrs = append(gatewayAddrs, gwIP.String()+"/32")
		} else {
			gatewayAddrs = append(gatewayAddrs, gwIP.String()+"/128")
		}
	}

	if _, _, err := lm.SyncAddresses(gatewayAddrs, false); err != nil {
		klog.Warningf("VXLAN: failed to sync addresses on %s: %v", vxlanInterfaceName, err)
	}

	// Compute VXLAN tunnel MTU.
	vxlanMTU := cfg.MTU

	if defaultMTU := unboundednetnetlink.DetectDefaultRouteMTUFromCache(state.netlinkCache); defaultMTU > 0 {
		detected := defaultMTU - unboundednetnetlink.VXLANMTUOverhead
		if detected < vxlanMTU {
			vxlanMTU = detected
		}
	}

	// Set interface MTU.
	if err := lm.EnsureMTU(vxlanMTU); err != nil {
		klog.Warningf("VXLAN: failed to set MTU on %s: %v", vxlanInterfaceName, err)
	}

	// Get the link index for vxlan0.
	iface, err := net.InterfaceByName(vxlanInterfaceName)
	if err != nil {
		return nil, nil, err
	}

	vxlanLinkIdx := iface.Index

	// Resolve local IP for encap src.
	var localIP net.IP
	if len(state.nodeInternalIPs) > 0 {
		localIP = net.ParseIP(state.nodeInternalIPs[0])
	}

	if localIP == nil {
		klog.Warning("VXLAN: no local internal IP available for encap src")
		return nil, nil, nil
	}

	// Build local CIDR sets for skipping routes to our own node.
	localPodCIDRs := routeplan.BuildNormalizedCIDRSet(state.nodePodCIDRs)
	localGatewayHostCIDRs := routeplan.BuildLocalGatewayHostCIDRSetFromPodCIDRs(state.nodePodCIDRs)

	var routes []unboundednetnetlink.DesiredRoute

	peerID := func(name string) string { return name + "/" + vxlanInterfaceName }

	// Process mesh peers.
	for _, peer := range meshPeers {
		if len(peer.InternalIPs) == 0 || len(peer.PodCIDRs) == 0 {
			continue
		}

		underlayIP := net.ParseIP(peer.InternalIPs[0])
		if underlayIP == nil {
			continue
		}

		peerMTU := resolveMeshPeerTunnelMTU(vxlanMTU, peer, mySiteName,
			siteTunnelMTUs, peeringSiteTunnelMTUs, assignmentSiteTunnelMTUs)
		encap := &unboundednetnetlink.IPEncap{Src: localIP, Dst: underlayIP}
		pid := peerID(peer.Name)
		peerRoutes := buildVXLANMeshPeerRoutes(peer, vxlanLinkIdx, pid, mySiteName, peeredSites, cfg, peerMTU, encap, localPodCIDRs, localGatewayHostCIDRs)
		routes = append(routes, peerRoutes...)
	}

	// Process gateway peers.
	for _, gwPeer := range gatewayPeers {
		if len(gwPeer.InternalIPs) == 0 || len(gwPeer.PodCIDRs) == 0 {
			continue
		}

		underlayIP := net.ParseIP(gwPeer.InternalIPs[0])
		if underlayIP == nil {
			continue
		}

		gwPeerMTU := resolveGatewayPeerTunnelMTU(vxlanMTU, mySiteName, gwPeer,
			siteTunnelMTUs, assignmentPoolTunnelMTUs, poolTunnelMTUs)
		encap := &unboundednetnetlink.IPEncap{Src: localIP, Dst: underlayIP}
		pid := peerID(gwPeer.Name)
		gwRoutes := buildVXLANGatewayPeerRoutes(gwPeer, vxlanLinkIdx, pid, mySiteName, cfg, gwPeerMTU, encap, localPodCIDRs, localGatewayHostCIDRs)
		routes = append(routes, gwRoutes...)
	}

	// Register VXLAN peers with healthcheck manager.
	vxlanHCPeers := registerPeersWithHealthCheck(meshPeers, gatewayPeers, mySiteName, false,
		siteHealthCheckProfileNames, peeringSiteHealthCheckProfileNames,
		assignmentSiteHealthCheckProfileNames, assignmentPoolHealthCheckProfileNames,
		poolHealthCheckProfileNames, state, peerIfaceNameVXLAN, true)

	klog.V(2).Infof("VXLAN: configured %d mesh + %d gateway peers on %s, %d routes, %d HC peers",
		len(meshPeers), len(gatewayPeers), vxlanInterfaceName, len(routes), len(vxlanHCPeers))

	return routes, vxlanHCPeers, nil
}

// peerIfaceNameVXLAN returns the VXLAN interface name for a gateway peer.
// All VXLAN peers share the "vxlan0" interface.
func peerIfaceNameVXLAN(gwPeer gatewayPeerInfo) string {
	if len(gwPeer.InternalIPs) == 0 {
		return ""
	}

	return vxlanInterfaceName
}

// buildVXLANMeshPeerRoutes builds DesiredRoute entries for a VXLAN mesh peer.
// Routes include lwt encap ip src/dst directives instead of per-peer interfaces.
func buildVXLANMeshPeerRoutes(peer meshPeerInfo, linkIndex int, peerID, mySiteName string, peeredSites map[string]bool, cfg *config, peerMTU int, encap *unboundednetnetlink.IPEncap, localPodCIDRs, localGatewayHostCIDRs map[string]struct{}) []unboundednetnetlink.DesiredRoute {
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

	nhForCIDR := func(cidr string) net.IP {
		var s string
		if strings.Contains(cidr, ":") {
			s = peerNHv6
		} else {
			s = peerNHv4
		}

		if s == "" {
			return nil
		}

		return net.ParseIP(s)
	}

	var routes []unboundednetnetlink.DesiredRoute

	// Bootstrap host routes (link-scope, immune to healthcheck, with encap).
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
					Encap:             encap,
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
					Encap:             encap,
				})
			}
		}
	}

	// Pod CIDR routes via gateway nexthop with encap.
	if !peer.SkipPodCIDRRoutes {
		for _, cidr := range peer.PodCIDRs {
			normalizedCIDR := routeplan.NormalizeCIDR(cidr)
			if normalizedCIDR == "" {
				continue
			}

			if _, isLocal := localPodCIDRs[normalizedCIDR]; isLocal {
				continue
			}

			gwIP := nhForCIDR(cidr)
			if gwIP == nil {
				continue
			}

			if _, prefix, _ := net.ParseCIDR(normalizedCIDR); prefix != nil { //nolint:errcheck
				routes = append(routes, unboundednetnetlink.DesiredRoute{
					Prefix:   *prefix,
					Nexthops: []unboundednetnetlink.DesiredNexthop{{PeerID: peerID, LinkIndex: linkIndex, Gateway: gwIP}},
					Metric:   cfg.BaseMetric,
					MTU:      peerMTU,
					Encap:    encap,
				})
			}
		}
	}

	// Internal IP host routes for non-peered sites.
	if !peeredSites[peer.SiteName] {
		for _, cidr := range ipsToHostCIDRs(peer.InternalIPs) {
			gwIP := nhForCIDR(cidr)
			if gwIP == nil {
				continue
			}

			if _, prefix, _ := net.ParseCIDR(cidr); prefix != nil { //nolint:errcheck
				routes = append(routes, unboundednetnetlink.DesiredRoute{
					Prefix:   *prefix,
					Nexthops: []unboundednetnetlink.DesiredNexthop{{PeerID: peerID, LinkIndex: linkIndex, Gateway: gwIP}},
					Metric:   cfg.BaseMetric,
					MTU:      peerMTU,
					Encap:    encap,
				})
			}
		}
	}

	return routes
}

// buildVXLANGatewayPeerRoutes builds DesiredRoute entries for a VXLAN gateway peer.
// Routes include lwt encap ip src/dst directives instead of per-peer interfaces.
func buildVXLANGatewayPeerRoutes(gwPeer gatewayPeerInfo, linkIndex int, peerID, mySiteName string, cfg *config, gwPeerMTU int, encap *unboundednetnetlink.IPEncap, localPodCIDRs, localGatewayHostCIDRs map[string]struct{}) []unboundednetnetlink.DesiredRoute {
	var peerNHv4, peerNHv6 string

	for _, podCIDR := range gwPeer.PodCIDRs {
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

	nhForCIDR := func(cidr string) net.IP {
		var s string
		if strings.Contains(cidr, ":") {
			s = peerNHv6
		} else {
			s = peerNHv4
		}

		if s == "" {
			return nil
		}

		return net.ParseIP(s)
	}

	var routes []unboundednetnetlink.DesiredRoute

	// Bootstrap host routes (link-scope, immune to healthcheck, with encap).
	if peerNHv4 != "" {
		hostCIDR := peerNHv4 + "/32"
		if _, isLocal := localGatewayHostCIDRs[hostCIDR]; !isLocal {
			if _, prefix, _ := net.ParseCIDR(hostCIDR); prefix != nil { //nolint:errcheck
				routes = append(routes, unboundednetnetlink.DesiredRoute{
					Prefix:            *prefix,
					Nexthops:          []unboundednetnetlink.DesiredNexthop{{PeerID: peerID, LinkIndex: linkIndex}},
					Metric:            cfg.BaseMetric,
					MTU:               gwPeerMTU,
					HealthCheckImmune: true,
					Encap:             encap,
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
					MTU:               gwPeerMTU,
					HealthCheckImmune: true,
					Encap:             encap,
				})
			}
		}
	}

	// Routed CIDR routes with per-distance metrics and encap.
	routedCIDRSet := make(map[string]struct{})

	for _, cidr := range gwPeer.RoutedCidrs {
		normalizedCIDR := routeplan.NormalizeCIDR(cidr)
		if normalizedCIDR == "" {
			continue
		}

		if _, isLocal := localPodCIDRs[normalizedCIDR]; isLocal {
			continue
		}

		routedCIDRSet[normalizedCIDR] = struct{}{}

		gwIP := nhForCIDR(cidr)
		if gwIP == nil {
			continue
		}

		metric := cfg.BaseMetric
		if d, ok := gwPeer.RouteDistances[cidr]; ok && d > 0 {
			metric += d
		}

		if _, prefix, _ := net.ParseCIDR(normalizedCIDR); prefix != nil { //nolint:errcheck
			routes = append(routes, unboundednetnetlink.DesiredRoute{
				Prefix:   *prefix,
				Nexthops: []unboundednetnetlink.DesiredNexthop{{PeerID: peerID, LinkIndex: linkIndex, Gateway: gwIP}},
				Metric:   metric,
				MTU:      gwPeerMTU,
				Encap:    encap,
			})
		}
	}

	// Pod CIDR routes (deduplicated against routed CIDRs) with encap.
	if !gwPeer.SkipPodCIDRRoutes {
		for _, cidr := range gwPeer.PodCIDRs {
			normalizedCIDR := routeplan.NormalizeCIDR(cidr)
			if normalizedCIDR == "" {
				continue
			}

			if _, isLocal := localPodCIDRs[normalizedCIDR]; isLocal {
				continue
			}

			if _, alreadyRouted := routedCIDRSet[normalizedCIDR]; alreadyRouted {
				continue
			}

			gwIP := nhForCIDR(cidr)
			if gwIP == nil {
				continue
			}

			if _, prefix, _ := net.ParseCIDR(normalizedCIDR); prefix != nil { //nolint:errcheck
				routes = append(routes, unboundednetnetlink.DesiredRoute{
					Prefix:   *prefix,
					Nexthops: []unboundednetnetlink.DesiredNexthop{{PeerID: peerID, LinkIndex: linkIndex, Gateway: gwIP}},
					Metric:   cfg.BaseMetric,
					MTU:      gwPeerMTU,
					Encap:    encap,
				})
			}
		}
	}

	return routes
}
