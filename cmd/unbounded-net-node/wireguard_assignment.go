// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"net"
	"sort"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	ebpfpkg "github.com/Azure/unbounded/internal/net/ebpf"
	unboundednetnetlink "github.com/Azure/unbounded/internal/net/netlink"
)

const (
	assignmentReturnTableBase  = 65536
	assignmentReturnPriority   = 11000
	assignmentReturnDeviceName = "unbounded-rtn"
)

var (
	assignmentRuleList  = netlink.RuleList
	assignmentRuleAdd   = netlink.RuleAdd
	assignmentRuleDel   = netlink.RuleDel
	assignmentRouteList = netlink.RouteListFiltered
	assignmentRouteDel  = netlink.RouteDel
)

// Ingress-only rules return forwarded pod and node traffic over assignment
// downlinks without replacing ordinary same-site routes.
func assignmentReturnPlan(prefixes []string, returnIndex int, interfaces map[string]int) ([]unboundednetnetlink.DesiredRoute, []netlink.Rule) {
	var (
		routes []unboundednetnetlink.DesiredRoute
		rules  []netlink.Rule
	)

	names := make([]string, 0, len(interfaces))
	for name := range interfaces {
		names = append(names, name)
	}

	sort.Strings(names)

	families := make(map[int]bool)

	for _, cidr := range prefixes {
		ip, prefix, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}

		family := netlink.FAMILY_V6
		if ip.To4() != nil {
			family = netlink.FAMILY_V4
		}

		families[family] = true

		routes = append(routes, unboundednetnetlink.DesiredRoute{
			Prefix: *prefix, Table: assignmentReturnTableBase + 1,
			Nexthops: []unboundednetnetlink.DesiredNexthop{{PeerID: assignmentReturnDeviceName, LinkIndex: returnIndex}},
		})
	}

	for _, name := range names {
		for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
			if !families[family] {
				continue
			}

			rule := netlink.NewRule()
			rule.Family = family
			rule.Priority = assignmentReturnPriority
			rule.Table = assignmentReturnTableBase + 1
			rule.IifName = name
			rule.Protocol = unix.RTPROT_STATIC
			rules = append(rules, *rule)
		}
	}

	return routes, rules
}

func assignmentReturnPrefixes(entries map[string]ebpfpkg.TunnelEndpoint, peers []meshPeerInfo) []string {
	var prefixes []string
	for prefix := range entries {
		prefixes = append(prefixes, prefix)
	}

	for _, peer := range peers {
		if peer.TransitPoolName != "" {
			prefixes = append(prefixes, peer.PodCIDRs...)
			prefixes = append(prefixes, ipsToHostCIDRs(peer.InternalIPs)...)
		}
	}

	return dedupeStrings(prefixes)
}

func prepareAssignmentReturnRoutes(cfg *config, state *wireGuardState, prefixes []string, mtu int) ([]unboundednetnetlink.DesiredRoute, error) {
	if len(prefixes) == 0 {
		return nil, nil
	}

	if fabric, err := net.InterfaceByName(unbounded0DeviceName); err == nil {
		mtu = fabric.MTU
	}

	link := unboundednetnetlink.NewLinkManager(assignmentReturnDeviceName)
	if err := link.EnsureDummyInterface(); err != nil {
		return nil, fmt.Errorf("ensure assignment return interface: %w", err)
	}

	if mtu > 0 {
		if err := link.EnsureMTU(mtu); err != nil {
			return nil, fmt.Errorf("set assignment return MTU: %w", err)
		}
	}

	if err := link.SetLinkUp(); err != nil {
		return nil, err
	}

	if state.assignmentTunnelMap == nil {
		tunnelMap, err := ebpfpkg.NewTunnelMap(ebpfpkg.TunnelMapOptions{
			MaxEntries: uint32(cfg.TunnelDataplaneMapSize), Name: "unb_assign",
		})
		if err != nil {
			return nil, err
		}

		state.mu.Lock()
		state.assignmentTunnelMap = tunnelMap
		state.mu.Unlock()
	}

	if err := state.assignmentTunnelMap.AttachToInterface(assignmentReturnDeviceName); err != nil {
		return nil, err
	}

	if state.forwardManager != nil {
		state.forwardManager.EnsureInterface(assignmentReturnDeviceName)
	}

	iface, err := net.InterfaceByName(assignmentReturnDeviceName)
	if err != nil {
		return nil, err
	}

	routes, _ := assignmentReturnPlan(prefixes, iface.Index, nil)
	if err := reconcileAssignmentReturnKernelRoutes(prefixes, iface.Index); err != nil {
		return nil, err
	}

	return routes, nil
}

func reconcileAssignmentReturnKernelRoutes(prefixes []string, linkIndex int) error {
	desired := make(map[string]bool, len(prefixes))
	for _, value := range prefixes {
		_, prefix, err := net.ParseCIDR(value)
		if err == nil {
			desired[prefix.String()] = true
		}
	}

	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		filter := &netlink.Route{
			Table: assignmentReturnTableBase + 1, LinkIndex: linkIndex, Protocol: unix.RTPROT_STATIC,
		}

		routes, err := assignmentRouteList(family, filter, netlink.RT_FILTER_TABLE|netlink.RT_FILTER_OIF|netlink.RT_FILTER_PROTOCOL)
		if err != nil {
			return fmt.Errorf("list assignment return routes: %w", err)
		}

		for _, route := range routes {
			if route.Table != filter.Table || route.LinkIndex != linkIndex || route.Protocol != unix.RTPROT_STATIC || len(route.MultiPath) != 0 {
				continue
			}

			prefix := "0.0.0.0/0"
			if family == netlink.FAMILY_V6 {
				prefix = "::/0"
			}

			if route.Dst != nil {
				prefix = route.Dst.String()
			}

			if desired[prefix] {
				continue
			}

			if err := assignmentRouteDel(&route); err != nil && !errors.Is(err, unix.ESRCH) {
				return fmt.Errorf("delete stale assignment return route %s: %w", prefix, err)
			}
		}
	}

	return nil
}

func reconcileAssignmentReturnDataplane(cfg *config, state *wireGuardState) error {
	state.mu.Lock()
	entries := state.pendingAssignmentBPFEntries
	state.pendingAssignmentBPFEntries = nil
	tunnelMap := state.assignmentTunnelMap
	state.mu.Unlock()

	if len(entries) == 0 {
		if err := reconcileAssignmentReturnRules(nil); err != nil {
			return err
		}

		state.mu.Lock()
		state.assignmentTunnelMap = nil
		state.mu.Unlock()

		if tunnelMap != nil {
			if err := tunnelMap.Close(); err != nil {
				return err
			}
		}

		link := unboundednetnetlink.NewLinkManager(assignmentReturnDeviceName)
		if link.Exists() {
			if err := link.DeleteLink(); err != nil {
				return err
			}
		}

		if state.forwardManager != nil {
			state.forwardManager.RemoveInterface(assignmentReturnDeviceName)
		}

		return nil
	}

	if tunnelMap == nil {
		return fmt.Errorf("assignment return map is unavailable")
	}

	if err := tunnelMap.Reconcile(entries); err != nil {
		return err
	}

	interfaces := make(map[string]int)

	names := []string{wireGuardInterfaceName(cfg, cfg.WireGuardPort), cfg.GeneveInterfaceName, cfg.VXLANInterfaceName, cfg.IPIPInterfaceName}
	for name := range state.gatewayLinkManagers {
		names = append(names, name)
	}

	for _, name := range names {
		if link, err := net.InterfaceByName(name); err == nil {
			interfaces[name] = link.Index
		}
	}

	if _, index, err := unboundednetnetlink.DetectDefaultRouteInterfaceFromCache(state.netlinkCache); err == nil {
		if link, err := net.InterfaceByIndex(index); err == nil {
			interfaces[link.Name] = link.Index
		}
	}

	_, rules := assignmentReturnPlan(assignmentReturnPrefixes(entries, nil), 0, interfaces)

	return reconcileAssignmentReturnRules(rules)
}

func reconcileAssignmentReturnRules(desired []netlink.Rule) error {
	key := func(rule netlink.Rule) string {
		return fmt.Sprintf("%d/%d/%s", rule.Family, rule.Table, rule.IifName)
	}

	missing := make(map[string]netlink.Rule, len(desired))
	for _, rule := range desired {
		missing[key(rule)] = rule
	}

	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		rules, err := assignmentRuleList(family)
		if err != nil {
			return fmt.Errorf("list assignment return rules: %w", err)
		}

		for _, rule := range rules {
			if rule.Priority != assignmentReturnPriority || rule.Protocol != unix.RTPROT_STATIC ||
				rule.Table <= assignmentReturnTableBase || rule.Table > assignmentReturnTableBase+65535 {
				continue
			}

			if _, keep := missing[key(rule)]; keep {
				delete(missing, key(rule))
				continue
			}

			if err := assignmentRuleDel(&rule); err != nil {
				return fmt.Errorf("delete assignment return rule: %w", err)
			}
		}
	}

	for _, rule := range missing {
		if err := assignmentRuleAdd(&rule); err != nil && !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("add assignment return rule: %w", err)
		}
	}

	return nil
}

func configureAssignmentWireGuard(cfg *config, state *wireGuardState, iface string, port int, privateKey string, peers []unboundednetnetlink.WireGuardPeer, addresses []string, mtu int) error {
	link := state.gatewayLinkManagers[iface]
	if link == nil {
		link = unboundednetnetlink.NewLinkManager(iface)
		state.gatewayLinkManagers[iface] = link
	}

	if err := link.EnsureWireGuardInterface(); err != nil {
		return fmt.Errorf("ensure assignment interface %s: %w", iface, err)
	}

	if mtu > 0 {
		if err := link.EnsureMTU(mtu); err != nil {
			return fmt.Errorf("set assignment MTU on %s: %w", iface, err)
		}
	}

	manager := state.gatewayWireguardManagers[iface]
	if manager == nil {
		var err error

		manager, err = unboundednetnetlink.NewWireGuardManager(iface)
		if err != nil {
			return fmt.Errorf("create assignment WireGuard manager for %s: %w", iface, err)
		}

		state.gatewayWireguardManagers[iface] = manager
	}

	if err := manager.Configure(privateKey, port, peers); err != nil {
		return fmt.Errorf("configure assignment WireGuard peers on %s: %w", iface, err)
	}

	if _, _, err := link.SyncAddresses(addresses, false); err != nil {
		return fmt.Errorf("configure assignment addresses on %s: %w", iface, err)
	}

	if err := link.SetLinkUp(); err != nil {
		return fmt.Errorf("bring assignment interface %s up: %w", iface, err)
	}

	disableRPFilter(iface)

	if state.forwardManager != nil {
		state.forwardManager.EnsureInterface(iface)
	}

	if state.notrackManager != nil {
		state.notrackManager.EnsureInterface(iface)
	}

	if cfg.EnablePolicyRouting && state.gatewayPolicyManager != nil {
		if err := state.gatewayPolicyManager.ConfigureInterface(iface, port); err != nil {
			return fmt.Errorf("configure assignment policy routing on %s: %w", iface, err)
		}
	}

	return nil
}
