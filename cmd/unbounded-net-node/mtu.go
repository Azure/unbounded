// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"fmt"
	"net"
	"sort"

	"github.com/vishvananda/netlink"

	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
	unboundednetnetlink "github.com/Azure/unbounded/internal/net/netlink"
)

const hostProcDir = "/proc"

type cniBridgeLinkManager interface {
	Exists() bool
	EnsureMTUWithCache(*unboundednetnetlink.NetlinkCache, int) error
	EnsureBridgePortMTUs(int) error
	EnsureBridgePodMTUs(string, int) error
}

var (
	ensureCNIBridgeMTUFunc    = ensureCNIBridgeMTU
	newCNIBridgeLinkManagerFn = func(bridgeName string) cniBridgeLinkManager {
		return unboundednetnetlink.NewLinkManager(bridgeName)
	}
)

func ensureCNIBridgeMTU(bridgeName string, mtu int, cache *unboundednetnetlink.NetlinkCache, reconcilePods bool) error {
	if bridgeName == "" || mtu <= 0 {
		return nil
	}

	linkManager := newCNIBridgeLinkManagerFn(bridgeName)
	if !linkManager.Exists() {
		return nil
	}

	if err := linkManager.EnsureMTUWithCache(cache, mtu); err != nil {
		return err
	}

	if err := linkManager.EnsureBridgePortMTUs(mtu); err != nil {
		return err
	}

	if !reconcilePods {
		return nil
	}

	return linkManager.EnsureBridgePodMTUs(hostProcDir, mtu)
}

func verifyActiveTunnelMTUs(cfg *config, fabricMTU int, meshPeers []meshPeerInfo, gatewayPeers []gatewayPeerInfo) error {
	if fabricMTU <= 0 {
		return nil
	}

	required := make(map[string]bool)
	if len(meshPeers) > 0 || len(gatewayPeers) > 0 {
		required[unbounded0DeviceName] = true
	}

	needsGeneve, needsIPIP, needsVXLAN := neededTunnelProtocols(meshPeers, gatewayPeers)
	if needsGeneve {
		required[cfg.GeneveInterfaceName] = true
	}

	if needsIPIP {
		required[cfg.IPIPInterfaceName] = true
	}

	if needsVXLAN {
		required[cfg.VXLANInterfaceName] = true
	}

	wireGuardMesh := false
	for _, peer := range meshPeers {
		if unboundednetv1alpha1.TunnelProtocol(peer.TunnelProtocol) == unboundednetv1alpha1.TunnelProtocolWireGuard {
			wireGuardMesh = true
			break
		}
	}

	if wireGuardMesh {
		required[wireGuardInterfaceName(cfg, cfg.WireGuardPort)] = true
	}

	for _, peer := range gatewayPeers {
		if unboundednetv1alpha1.TunnelProtocol(peer.TunnelProtocol) != unboundednetv1alpha1.TunnelProtocolWireGuard {
			continue
		}

		if peer.GatewayWireguardPort == 0 {
			return fmt.Errorf("WireGuard gateway peer %s has no interface port", peer.Name)
		}

		required[wireGuardInterfaceName(cfg, int(peer.GatewayWireguardPort))] = true
	}

	names := make([]string, 0, len(required))
	for name := range required {
		if name != "" {
			names = append(names, name)
		}
	}

	sort.Strings(names)

	for _, name := range names {
		link, err := netlink.LinkByName(name)
		if err != nil {
			return fmt.Errorf("required tunnel interface %s is unavailable: %w", name, err)
		}

		if link.Attrs().MTU < fabricMTU {
			return fmt.Errorf("tunnel interface %s MTU %d is below fabric MTU %d", name, link.Attrs().MTU, fabricMTU)
		}
	}

	return nil
}

func resolveInitialCNIConfigMTU(configuredMTU, siteMTU, underlayMTU int) int {
	autoMTU := 0
	if underlayMTU > unboundednetnetlink.WireGuardMTUOverhead {
		autoMTU = underlayMTU - unboundednetnetlink.WireGuardMTUOverhead
	}

	return resolveTunnelMTU(configuredMTU, siteMTU, autoMTU)
}

func resolveCNIConfigMTU(configuredMTU, siteMTU int, meshPeers []meshPeerInfo, gatewayPeers []gatewayPeerInfo, underlayMTU int) int {
	values := []int{siteMTU}
	for _, peer := range meshPeers {
		values = append(values, peer.TunnelMTU)
	}

	for _, peer := range gatewayPeers {
		values = append(values, peer.TunnelMTU)
	}

	mtu := resolveTunnelMTU(configuredMTU, values...)
	if mtu > 0 {
		return mtu
	}

	return resolveInitialCNIConfigMTU(configuredMTU, siteMTU, underlayMTU)
}

func resolvePeerTunnelMTUs(
	cfg *config,
	meshPeers []meshPeerInfo,
	gatewayPeers []gatewayPeerInfo,
	mySiteName string,
	peeredSites, networkPeeredSites map[string]bool,
	siteTunnelMTUs, peeringSiteTunnelMTUs, assignmentSiteTunnelMTUs,
	assignmentPoolTunnelMTUs, poolTunnelMTUs map[string]int,
	cache *unboundednetnetlink.NetlinkCache,
) {
	for i := range meshPeers {
		underlayIP := selectUnderlayIP(meshPeers[i].InternalIPs, cfg.TunnelIPFamily)
		autoMTU := detectEncapsulatedRouteMTU(cfg, underlayIP, meshPeers[i].TunnelProtocol, cache)
		meshPeers[i].TunnelMTU = resolveTunnelMTU(cfg.MTU, autoMTU,
			resolveMeshPeerTunnelMTU(0, meshPeers[i], mySiteName,
				siteTunnelMTUs, peeringSiteTunnelMTUs, assignmentSiteTunnelMTUs))
	}

	for i := range gatewayPeers {
		underlayIP := gatewayPeerUnderlayIP(gatewayPeers[i], mySiteName, networkPeeredSites, cfg.TunnelIPFamily)
		if underlayIP == nil && peeredSites[gatewayPeers[i].SiteName] {
			underlayIP = selectUnderlayIP(gatewayPeers[i].InternalIPs, cfg.TunnelIPFamily)
		}

		autoMTU := detectEncapsulatedRouteMTU(cfg, underlayIP, gatewayPeers[i].TunnelProtocol, cache)
		gatewayPeers[i].TunnelMTU = resolveTunnelMTU(cfg.MTU, autoMTU,
			resolveGatewayPeerTunnelMTU(0, mySiteName, gatewayPeers[i],
				siteTunnelMTUs, assignmentPoolTunnelMTUs, poolTunnelMTUs))
	}
}

func gatewayPeerUnderlayIP(peer gatewayPeerInfo, mySiteName string, networkPeeredSites map[string]bool, ipFamily string) net.IP {
	if peer.SiteName == mySiteName || networkPeeredSites[peer.SiteName] || peer.PoolType != gatewayPoolTypeExternal {
		return selectUnderlayIP(peer.InternalIPs, ipFamily)
	}

	for _, ipStr := range peer.ExternalIPs {
		if ip := net.ParseIP(ipStr); ip != nil {
			if ipFamily == "IPv4" && ip.To4() == nil {
				continue
			}

			if ipFamily == "IPv6" && ip.To4() != nil {
				continue
			}

			return ip
		}
	}

	return nil
}

func detectEncapsulatedRouteMTU(cfg *config, underlayIP net.IP, protocol string, cache *unboundednetnetlink.NetlinkCache) int {
	underlayMTU, ifaceName := unboundednetnetlink.DetectRouteMTUAndInterface(underlayIP, cache)
	if isManagedTunnelInterface(cfg, ifaceName) {
		underlayMTU = detectUnderlyingRouteMTU(cfg, underlayIP, cache)
	}

	if underlayMTU == 0 {
		underlayMTU = unboundednetnetlink.DetectDefaultRouteMTUFromCache(cache)
	}

	overhead := tunnelProtocolMTUOverhead(protocol)
	if underlayMTU <= overhead {
		return 0
	}

	return underlayMTU - overhead
}

func detectUnderlyingRouteMTU(cfg *config, destination net.IP, cache *unboundednetnetlink.NetlinkCache) int {
	if destination == nil {
		return 0
	}

	family := netlink.FAMILY_V6
	if destination.To4() != nil {
		family = netlink.FAMILY_V4
	}

	var (
		routes []netlink.Route
		err    error
	)
	if cache != nil {
		routes, err = cache.RouteList(nil, family)
	} else {
		routes, err = netlink.RouteList(nil, family)
	}

	if err != nil {
		return 0
	}

	bestPrefix := -1
	bestPriority := 0
	bestMTU := 0

	for _, route := range routes {
		prefix := 0

		if route.Dst != nil {
			if !route.Dst.Contains(destination) {
				continue
			}

			prefix, _ = route.Dst.Mask.Size()
		}

		var link netlink.Link
		if cache != nil {
			link, err = cache.LinkByIndex(route.LinkIndex)
		} else {
			link, err = netlink.LinkByIndex(route.LinkIndex)
		}

		if err != nil || link == nil || isManagedTunnelInterface(cfg, link.Attrs().Name) {
			continue
		}

		if prefix < bestPrefix || (prefix == bestPrefix && bestPrefix >= 0 && route.Priority >= bestPriority) {
			continue
		}

		mtu := link.Attrs().MTU
		if route.MTU > 0 && (mtu == 0 || route.MTU < mtu) {
			mtu = route.MTU
		}

		if mtu == 0 {
			continue
		}

		bestPrefix = prefix
		bestPriority = route.Priority
		bestMTU = mtu
	}

	return bestMTU
}

func tunnelProtocolMTUOverhead(protocol string) int {
	switch unboundednetv1alpha1.TunnelProtocol(protocol) {
	case unboundednetv1alpha1.TunnelProtocolGENEVE:
		return unboundednetnetlink.GeneveMTUOverhead
	case unboundednetv1alpha1.TunnelProtocolVXLAN:
		return unboundednetnetlink.VXLANMTUOverhead
	case unboundednetv1alpha1.TunnelProtocolIPIP:
		return unboundednetnetlink.IPIPMTUOverhead
	case unboundednetv1alpha1.TunnelProtocolNone:
		return 0
	default:
		return unboundednetnetlink.WireGuardMTUOverhead
	}
}

func minimumPeerTunnelMTU(protocol string, meshPeers []meshPeerInfo, gatewayPeers []gatewayPeerInfo) int {
	values := make([]int, 0, len(meshPeers)+len(gatewayPeers))
	for _, peer := range meshPeers {
		if peer.TunnelProtocol == protocol {
			values = append(values, peer.TunnelMTU)
		}
	}

	for _, peer := range gatewayPeers {
		if peer.TunnelProtocol == protocol {
			values = append(values, peer.TunnelMTU)
		}
	}

	return resolveTunnelMTU(0, values...)
}

func protocolInterfaceMTU(configuredMTU, siteMTU, defaultUnderlayMTU int, protocol string, meshPeers []meshPeerInfo, gatewayPeers []gatewayPeerInfo) int {
	mtu := minimumPeerTunnelMTU(protocol, meshPeers, gatewayPeers)
	if mtu > 0 {
		return mtu
	}

	var (
		autoMTU  int
		overhead = tunnelProtocolMTUOverhead(protocol)
	)

	if defaultUnderlayMTU > overhead {
		autoMTU = defaultUnderlayMTU - overhead
	}

	return resolveTunnelMTU(configuredMTU, siteMTU, autoMTU)
}

func routeTunnelMTU(prefix *net.IPNet, meshPeers []meshPeerInfo, gatewayPeers []gatewayPeerInfo) int {
	var values []int

	addIfCovered := func(cidrStr string, mtu int) {
		if mtu <= 0 {
			return
		}

		_, cidr, err := net.ParseCIDR(cidrStr)
		if err != nil {
			return
		}

		if prefix.Contains(cidr.IP) || cidr.Contains(prefix.IP) {
			values = append(values, mtu)
		}
	}

	for _, peer := range meshPeers {
		for _, cidr := range peer.PodCIDRs {
			addIfCovered(cidr, peer.TunnelMTU)
		}

		for _, cidr := range ipsToHostCIDRs(peer.InternalIPs) {
			addIfCovered(cidr, peer.TunnelMTU)
		}
	}

	for _, peer := range gatewayPeers {
		for _, cidr := range peer.PodCIDRs {
			addIfCovered(cidr, peer.TunnelMTU)
		}

		for _, cidr := range peer.RoutedCidrs {
			addIfCovered(cidr, peer.TunnelMTU)
		}

		for _, cidr := range ipsToHostCIDRs(peer.InternalIPs) {
			addIfCovered(cidr, peer.TunnelMTU)
		}
	}

	if mtu := resolveTunnelMTU(0, values...); mtu > 0 {
		return mtu
	}

	return resolveCNIConfigMTU(0, 0, meshPeers, gatewayPeers, 0)
}
