// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"k8s.io/klog/v2"
)

type neighborGCTuner struct {
	routeGet    func(net.IP) ([]netlink.Route, error)
	linkByIndex func(int) (netlink.Link, error)
	readFile    func(string) ([]byte, error)
	writeFile   func(string, []byte, os.FileMode) error
}

type neighborWireGuardPeerSource interface {
	GetPeers() ([]wgtypes.Peer, error)
}

func newNeighborGCTuner() *neighborGCTuner {
	return &neighborGCTuner{
		routeGet: func(ip net.IP) ([]netlink.Route, error) {
			return netlink.RouteGetWithOptions(ip, &netlink.RouteGetOptions{FIBMatch: true})
		},
		linkByIndex: netlink.LinkByIndex,
		readFile:    os.ReadFile,
		writeFile:   os.WriteFile,
	}
}

func meshNeighborEndpoints(cfg *config, peers []meshPeerInfo, gateways []gatewayPeerInfo, mySite string, peeredSites, networkPeeredSites map[string]bool) []net.IP {
	var endpoints []net.IP

	for _, peer := range peers {
		if peer.TunnelProtocol == "WireGuard" && !peeredSites[peer.SiteName] {
			continue
		}

		if ip := selectUnderlayIP(peer.InternalIPs, cfg.TunnelIPFamily); ip != nil {
			endpoints = append(endpoints, ip)
		}
	}

	for _, peer := range gateways {
		if peer.TunnelProtocol == "WireGuard" &&
			peer.SiteName != mySite && !networkPeeredSites[peer.SiteName] &&
			peer.PoolType != gatewayPoolTypeExternal {
			continue
		}

		var ip net.IP
		if peer.TunnelProtocol == "WireGuard" {
			ip = gatewayPeerUnderlayIP(peer, mySite, networkPeeredSites, cfg.TunnelIPFamily)
		} else {
			ip = selectUnderlayIP(peer.InternalIPs, cfg.TunnelIPFamily)
		}

		if ip != nil {
			endpoints = append(endpoints, ip)
		}
	}

	return endpoints
}

func liveWireGuardNeighborEndpoints(peers []meshPeerInfo, gateways []gatewayPeerInfo, sources map[string]neighborWireGuardPeerSource) ([]net.IP, error) {
	keys := make(map[string]bool)

	for _, peer := range peers {
		if peer.TunnelProtocol == "WireGuard" {
			keys[peer.WireGuardPublicKey] = true
		}
	}

	for _, peer := range gateways {
		if peer.TunnelProtocol == "WireGuard" {
			keys[peer.WireGuardPublicKey] = true
		}
	}

	if len(keys) == 0 {
		return nil, nil
	}

	var endpoints []net.IP

	for name, source := range sources {
		current, err := source.GetPeers()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENODEV) {
				klog.V(3).Infof("Skipping neighbor endpoints for missing WireGuard interface %s: %v", name, err)
				continue
			}

			return nil, fmt.Errorf("read neighbor endpoints on WireGuard interface %s: %w", name, err)
		}

		for _, peer := range current {
			if keys[peer.PublicKey.String()] && peer.Endpoint != nil {
				endpoints = append(endpoints, peer.Endpoint.IP)
			}
		}
	}

	return endpoints, nil
}

func (state *wireGuardState) reconcileNeighborCapacity(cfg *config, peers []meshPeerInfo, gateways []gatewayPeerInfo, mySite string, peeredSites, networkPeeredSites map[string]bool) error {
	endpoints := meshNeighborEndpoints(cfg, peers, gateways, mySite, peeredSites, networkPeeredSites)

	sources := make(map[string]neighborWireGuardPeerSource)
	if state.wireguardManager != nil {
		sources[wireGuardInterfaceName(cfg, cfg.WireGuardPort)] = state.wireguardManager
	}

	for name, manager := range state.gatewayWireguardManagers {
		if manager != nil {
			sources[name] = manager
		}
	}

	liveEndpoints, err := liveWireGuardNeighborEndpoints(peers, gateways, sources)
	if err != nil {
		return err
	}

	return state.neighborGCTuner.reconcile(cfg, append(endpoints, liveEndpoints...))
}

func (t *neighborGCTuner) reconcile(cfg *config, endpoints []net.IP) error {
	counts, err := t.countNeighbors(cfg, endpoints)
	if err != nil {
		return err
	}

	return applyNeighborGCThresholds(counts, t.readFile, t.writeFile)
}

func (t *neighborGCTuner) countNeighbors(cfg *config, endpoints []net.IP) ([2]int, error) {
	type neighborKey struct {
		link int
		ip   netip.Addr
	}

	neighbors := make(map[neighborKey]struct{})
	seen := make(map[netip.Addr]struct{})
	links := make(map[int]netlink.Link)

	var counts [2]int

	addNextHop := func(destination net.IP, linkIndex int, gateway net.IP, via netlink.Destination) error {
		if linkIndex == 0 {
			return fmt.Errorf("route to %s has no egress interface", destination)
		}

		link := links[linkIndex]
		if link == nil {
			var err error

			link, err = t.linkByIndex(linkIndex)
			if err != nil {
				return fmt.Errorf("resolve neighbor egress interface %d for %s: %w", linkIndex, destination, err)
			}

			links[linkIndex] = link
		}

		attrs := link.Attrs()
		if attrs.Flags&net.FlagLoopback != 0 || attrs.RawFlags&unix.IFF_NOARP != 0 ||
			isManagedTunnelInterface(cfg, attrs.Name) {
			return nil
		}

		ip := destination
		if len(gateway) != 0 && !gateway.IsUnspecified() {
			ip = gateway
		}

		if via != nil {
			nextHop, ok := via.(*netlink.Via)
			if !ok {
				return fmt.Errorf("unsupported next hop for %s: %T", destination, via)
			}

			ip = nextHop.Addr
		}

		addr, ok := netip.AddrFromSlice(ip)
		if !ok {
			return fmt.Errorf("invalid neighbor next hop for %s", destination)
		}

		key := neighborKey{link: linkIndex, ip: addr.Unmap()}
		if _, exists := neighbors[key]; !exists {
			neighbors[key] = struct{}{}
			if key.ip.Is4() {
				counts[0]++
			} else {
				counts[1]++
			}
		}

		return nil
	}

	for _, endpoint := range endpoints {
		addr, ok := netip.AddrFromSlice(endpoint)
		if !ok {
			return [2]int{}, fmt.Errorf("invalid mesh endpoint %q", endpoint)
		}

		addr = addr.Unmap()
		if _, exists := seen[addr]; exists {
			continue
		}

		seen[addr] = struct{}{}

		routes, err := t.routeGet(endpoint)
		if err != nil {
			if errors.Is(err, unix.ENETUNREACH) || errors.Is(err, unix.EHOSTUNREACH) {
				klog.V(3).Infof("No reachable neighbor next hop for mesh endpoint %s: %v", endpoint, err)
				continue
			}

			return [2]int{}, fmt.Errorf("resolve neighbor route to %s: %w", endpoint, err)
		}

		if len(routes) == 0 {
			return [2]int{}, fmt.Errorf("no neighbor route returned for mesh endpoint %s", endpoint)
		}

		for _, route := range routes {
			if route.Type != unix.RTN_UNICAST {
				continue
			}

			if len(route.MultiPath) == 0 {
				if err := addNextHop(endpoint, route.LinkIndex, route.Gw, route.Via); err != nil {
					return [2]int{}, err
				}
			}

			for _, hop := range route.MultiPath {
				if hop.Flags&(unix.RTNH_F_DEAD|unix.RTNH_F_LINKDOWN) != 0 {
					continue
				}

				if err := addNextHop(endpoint, hop.LinkIndex, hop.Gw, hop.Via); err != nil {
					return [2]int{}, err
				}
			}
		}
	}

	return counts, nil
}
