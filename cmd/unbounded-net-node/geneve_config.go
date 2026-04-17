// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
	"k8s.io/klog/v2"

	unboundednetv1alpha1 "github.com/Azure/unbounded-kube/api/net/v1alpha1"
	unboundednetnetlink "github.com/Azure/unbounded-kube/internal/net/netlink"
	"github.com/Azure/unbounded-kube/internal/net/routeplan"
)

// geneveIfaceName returns a deterministic interface name for a GENEVE peer
// based on the peer's underlay IPv4 address: gn<decimal IP>.
// For example, 172.16.1.5 -> gn2886729989.
func geneveIfaceName(underlayIP net.IP) string {
	ip4 := underlayIP.To4()
	if ip4 == nil {
		// Fallback for IPv6: use last 4 bytes.
		ip4 = underlayIP[len(underlayIP)-4:]
	}

	return fmt.Sprintf("gn%d", binary.BigEndian.Uint32(ip4))
}

// ipipIfaceName returns a deterministic interface name for an IPIP peer
// based on the peer's underlay IPv4 address: ip<decimal IP>.
func ipipIfaceName(underlayIP net.IP) string {
	ip4 := underlayIP.To4()
	if ip4 == nil {
		ip4 = underlayIP[len(underlayIP)-4:]
	}

	return fmt.Sprintf("ip%d", binary.BigEndian.Uint32(ip4))
}

// tunnelIfaceNameForPeer returns the interface name for a tunnel peer
// based on its tunnel protocol and underlay IP. Returns empty string for
// TunnelProtocolNone since no tunnel interface is created. VXLAN peers
// always use the shared "vxlan0" interface.
func tunnelIfaceNameForPeer(tunnelProto string, underlayIP net.IP) string {
	switch unboundednetv1alpha1.TunnelProtocol(tunnelProto) {
	case unboundednetv1alpha1.TunnelProtocolIPIP:
		return ipipIfaceName(underlayIP)
	case unboundednetv1alpha1.TunnelProtocolVXLAN:
		return "vxlan0"
	case unboundednetv1alpha1.TunnelProtocolNone:
		return ""
	default:
		return geneveIfaceName(underlayIP)
	}
}

// configureTunnelPeers creates per-peer tunnel interfaces (GENEVE or IPIP)
// and returns DesiredRoute objects and registered healthcheck peer names.
// When cfg.TunnelDataplane is "ebpf", delegates to the eBPF dataplane which
// uses a single flow-based interface with an LPM trie map instead.
func configureTunnelPeers(ctx context.Context, cfg *config, meshPeers []meshPeerInfo, gatewayPeers []gatewayPeerInfo, mySiteName string, peeredSites, networkPeeredSites map[string]bool, siteHealthCheckProfileNames, peeringSiteHealthCheckProfileNames, assignmentSiteHealthCheckProfileNames, assignmentPoolHealthCheckProfileNames, poolHealthCheckProfileNames map[string]string, siteTunnelMTUs, peeringSiteTunnelMTUs, assignmentSiteTunnelMTUs, assignmentPoolTunnelMTUs, poolTunnelMTUs map[string]int, state *wireGuardState) ([]unboundednetnetlink.DesiredRoute, map[string]bool, error) {
	// eBPF dataplane: use a single flow-based interface + BPF map.
	if cfg.TunnelDataplane == "ebpf" {
		return configureEBPFTunnelPeers(ctx, cfg, meshPeers, gatewayPeers,
			mySiteName, peeredSites, networkPeeredSites,
			siteHealthCheckProfileNames, peeringSiteHealthCheckProfileNames,
			assignmentSiteHealthCheckProfileNames, assignmentPoolHealthCheckProfileNames,
			poolHealthCheckProfileNames,
			siteTunnelMTUs, peeringSiteTunnelMTUs, assignmentSiteTunnelMTUs,
			assignmentPoolTunnelMTUs, poolTunnelMTUs, state)
	}

	if state.geneveInterfaces == nil {
		state.geneveInterfaces = make(map[string]*unboundednetnetlink.LinkManager)
	}

	if len(meshPeers) == 0 && len(gatewayPeers) == 0 {
		// Clean up all existing GENEVE interfaces.
		removeStaleGeneveInterfaces(state, nil)
		return nil, nil, nil
	}

	// Use the netlink cache for O(1) interface lookups. If the cache is nil
	// (e.g. in tests), the WithCache methods fall back to individual syscalls.
	nlCache := state.netlinkCache

	// Compute GENEVE tunnel MTU.
	geneveMTU := cfg.MTU

	if defaultMTU := unboundednetnetlink.DetectDefaultRouteMTUFromCache(nlCache); defaultMTU > 0 {
		detected := defaultMTU - unboundednetnetlink.GeneveMTUOverhead
		if detected < geneveMTU {
			geneveMTU = detected
		}
	}

	var routes []unboundednetnetlink.DesiredRoute

	desiredIfaces := make(map[string]bool)

	// Compute addresses for tunnel interfaces: the node's podCIDR gateway
	// IPs as /32 (IPv4) and /128 (IPv6). These are needed so that
	// MASQUERADE and source address selection work correctly when traffic
	// exits via tunnel interfaces (same addresses as WireGuard interfaces).
	var tunnelAddresses []string

	for _, cidr := range state.nodePodCIDRs {
		gwIP := getGatewayIPFromCIDR(cidr)
		if gwIP == nil {
			continue
		}

		if gwIP.To4() != nil {
			tunnelAddresses = append(tunnelAddresses, gwIP.String()+"/32")
		} else {
			tunnelAddresses = append(tunnelAddresses, gwIP.String()+"/128")
		}
	}

	// processPeer creates or ensures a per-peer tunnel interface (GENEVE or IPIP)
	// and returns the link index and peerID, or an error. For None peers, no
	// interface is created and the default route interface is used instead.
	processPeer := func(name, tunnelProto string, underlayIP net.IP, podCIDRs []string, mtu int) (int, string, error) {
		ifName := tunnelIfaceNameForPeer(tunnelProto, underlayIP)

		switch unboundednetv1alpha1.TunnelProtocol(tunnelProto) {
		case unboundednetv1alpha1.TunnelProtocolNone:
			// Direct routing -- no tunnel interface, use default route interface.
			// The peer's internal IP is directly reachable via the underlay.
			defaultIfName, defaultLinkIdx, err := unboundednetnetlink.DetectDefaultRouteInterfaceFromCache(nlCache)
			if err != nil {
				return 0, "", fmt.Errorf("failed to detect default route interface for None peer %s: %w", name, err)
			}

			peerID := name + "/" + defaultIfName

			return defaultLinkIdx, peerID, nil
		default:
			// GENEVE or IPIP -- create a per-peer tunnel interface.
		}

		desiredIfaces[ifName] = true

		lm, exists := state.geneveInterfaces[ifName]
		if !exists {
			lm = unboundednetnetlink.NewLinkManager(ifName)
			state.geneveInterfaces[ifName] = lm
		}

		switch unboundednetv1alpha1.TunnelProtocol(tunnelProto) {
		case unboundednetv1alpha1.TunnelProtocolIPIP:
			// IPIP needs a local IP for the tunnel
			var localIP net.IP
			if len(state.nodeInternalIPs) > 0 {
				localIP = net.ParseIP(state.nodeInternalIPs[0])
			}

			if err := lm.EnsureIPIPInterfaceWithCache(nlCache, localIP, underlayIP); err != nil {
				return 0, "", fmt.Errorf("failed to create IPIP interface %s: %w", ifName, err)
			}
		default:
			if err := lm.EnsureGeneveInterfaceWithCache(nlCache, uint32(cfg.GeneveVNI), cfg.GenevePort, underlayIP); err != nil {
				return 0, "", fmt.Errorf("failed to create GENEVE interface %s: %w", ifName, err)
			}
		}

		if err := lm.SetLinkUpWithCache(nlCache); err != nil {
			return 0, "", fmt.Errorf("failed to bring up %s: %w", ifName, err)
		}

		if len(tunnelAddresses) > 0 {
			if _, _, err := lm.SyncAddresses(tunnelAddresses, false); err != nil {
				klog.Warningf("Tunnel: failed to sync addresses on %s: %v", ifName, err)
			}
		}

		if mtu > 0 {
			if err := lm.EnsureMTUWithCache(nlCache, mtu); err != nil {
				klog.Warningf("Tunnel: failed to set MTU on %s: %v", ifName, err)
			}
		}
		// Disable rp_filter AFTER all link modifications (SetLinkUp,
		// SyncAddresses, EnsureMTU). Some netlink operations can cause
		// the kernel to reset interface sysctls.
		disableRPFilter(ifName)
		ensureTunnelForwardAccept(ifName)

		iface, err := net.InterfaceByName(ifName)
		if err != nil {
			return 0, "", fmt.Errorf("GENEVE interface %s not found after creation: %w", ifName, err)
		}

		peerID := name + "/" + ifName

		return iface.Index, peerID, nil
	}

	// Build local CIDR sets for skipping routes to our own node.
	localPodCIDRs := routeplan.BuildNormalizedCIDRSet(state.nodePodCIDRs)
	localGatewayHostCIDRs := routeplan.BuildLocalGatewayHostCIDRSetFromPodCIDRs(state.nodePodCIDRs)

	// Process mesh peers.
	for _, peer := range meshPeers {
		if len(peer.InternalIPs) == 0 || len(peer.PodCIDRs) == 0 {
			continue
		}

		underlayIP := net.ParseIP(peer.InternalIPs[0])
		if underlayIP == nil {
			continue
		}

		peerMTU := resolveMeshPeerTunnelMTU(geneveMTU, peer, mySiteName,
			siteTunnelMTUs, peeringSiteTunnelMTUs, assignmentSiteTunnelMTUs)

		linkIdx, peerID, err := processPeer(peer.Name, peer.TunnelProtocol, underlayIP, peer.PodCIDRs, peerMTU)
		if err != nil {
			klog.Warningf("Tunnel: %v", err)
			continue
		}

		// Extract the interface suffix from peerID (name/suffix).
		peerIDSuffix := peerID[len(peer.Name)+1:]
		peerRoutes := buildMeshPeerRoutes(peer, linkIdx, peerIDSuffix, mySiteName, peeredSites, cfg, peerMTU, localPodCIDRs, localGatewayHostCIDRs)
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

		gwPeerMTU := resolveGatewayPeerTunnelMTU(geneveMTU, mySiteName, gwPeer,
			siteTunnelMTUs, assignmentPoolTunnelMTUs, poolTunnelMTUs)

		linkIdx, peerID, err := processPeer(gwPeer.Name, gwPeer.TunnelProtocol, underlayIP, gwPeer.PodCIDRs, gwPeerMTU)
		if err != nil {
			klog.Warningf("Tunnel: %v", err)
			continue
		}

		// Extract the interface suffix from peerID (name/suffix).
		peerIDSuffix := peerID[len(gwPeer.Name)+1:]
		gwRoutes := buildGatewayPeerRoutes(gwPeer, linkIdx, peerIDSuffix, mySiteName, cfg, gwPeerMTU, localPodCIDRs, localGatewayHostCIDRs)
		routes = append(routes, gwRoutes...)
	}

	// Remove stale GENEVE interfaces from previous reconciles.
	removeStaleGeneveInterfaces(state, desiredIfaces)

	// Register GENEVE peers with healthcheck manager via shared HC registration.
	// GENEVE uses site-level HC profile fallback for gateway peers since there
	// is no WireGuard handshake to use as a liveness signal.
	geneveHCPeers := registerPeersWithHealthCheck(meshPeers, gatewayPeers, mySiteName, false,
		siteHealthCheckProfileNames, peeringSiteHealthCheckProfileNames,
		assignmentSiteHealthCheckProfileNames, assignmentPoolHealthCheckProfileNames,
		poolHealthCheckProfileNames, state, peerIfaceNameGeneve, true)

	klog.V(2).Infof("Tunnel: configured %d mesh + %d gateway peers across %d interfaces, %d routes, %d HC peers",
		len(meshPeers), len(gatewayPeers), len(desiredIfaces), len(routes), len(geneveHCPeers))

	return routes, geneveHCPeers, nil
}

// removeStaleGeneveInterfaces deletes GENEVE interfaces that are no longer
// needed. Also scans for unmanaged gn* interfaces left from previous runs.
func removeStaleGeneveInterfaces(state *wireGuardState, desired map[string]bool) {
	// Remove tracked interfaces that are no longer desired.
	for ifName, lm := range state.geneveInterfaces {
		if desired != nil && desired[ifName] {
			continue
		}

		if err := lm.DeleteLink(); err != nil {
			klog.V(2).Infof("Tunnel: failed to remove stale interface %s: %v", ifName, err)
		} else {
			klog.Infof("Tunnel: removed stale interface %s", ifName)
		}

		removeTunnelForwardAccept(ifName)
		delete(state.geneveInterfaces, ifName)
	}

	// Scan for unmanaged gn* interfaces (e.g., from a previous agent crash).
	// Use the netlink cache if available, otherwise fall back to a direct list.
	var links []netlink.Link
	if state.netlinkCache != nil {
		links = state.netlinkCache.LinkList()
	} else {
		var listErr error

		links, listErr = netlink.LinkList()
		if listErr != nil {
			return
		}
	}

	for _, link := range links {
		name := link.Attrs().Name
		if len(name) < 3 || (name[:2] != "gn" && name[:2] != "ip") {
			continue
		}

		if _, ok := link.(*netlink.Geneve); !ok {
			if _, ok := link.(*netlink.Iptun); !ok {
				continue
			}
		}

		if desired != nil && desired[name] {
			continue
		}

		if _, tracked := state.geneveInterfaces[name]; tracked {
			continue
		}

		if err := netlink.LinkDel(link); err != nil {
			klog.V(2).Infof("Tunnel: failed to remove unmanaged interface %s: %v", name, err)
		} else {
			klog.Infof("Tunnel: removed unmanaged interface %s", name)
		}

		removeTunnelForwardAccept(name)
	}
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
