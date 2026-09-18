// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net/netip"
	"slices"

	"k8s.io/klog/v2"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

type siteRoutingConfig struct {
	localCIDRs          []netip.Prefix
	gatewaySupernets    []string
	gatewayNotrackCIDRs []string
	gatewayReturnCIDRs  []string
}

func collectSiteRoutingConfig(sites map[string]*unboundedv1alpha3.Site, mySiteName string, isGateway bool) (siteRoutingConfig, error) {
	var result siteRoutingConfig

	if site := sites[mySiteName]; site != nil {
		for _, cidr := range site.Spec.LocalCIDRs {
			prefix, err := parseRoutingPrefix(cidr)
			if err != nil {
				return result, fmt.Errorf("site %s spec.localCidrs: %w", mySiteName, err)
			}

			result.localCIDRs = append(result.localCIDRs, prefix)
		}

		slices.SortFunc(result.localCIDRs, func(a, b netip.Prefix) int {
			if c := a.Addr().Compare(b.Addr()); c != 0 {
				return c
			}

			return a.Bits() - b.Bits()
		})
		result.localCIDRs = slices.Compact(result.localCIDRs)

		if isGateway {
			result.gatewayReturnCIDRs = append(result.gatewayReturnCIDRs, site.Spec.NodeCidrs...)
			for _, prefix := range result.localCIDRs {
				result.gatewayReturnCIDRs = append(result.gatewayReturnCIDRs, prefix.String())
			}
		}
	}

	if !isGateway {
		return result, nil
	}

	for name, site := range sites {
		for _, assignment := range site.Spec.PodCidrAssignments {
			result.gatewaySupernets = append(result.gatewaySupernets, assignment.CidrBlocks...)
			result.gatewayNotrackCIDRs = append(result.gatewayNotrackCIDRs, assignment.CidrBlocks...)
		}

		result.gatewayNotrackCIDRs = append(result.gatewayNotrackCIDRs, site.Spec.NodeCidrs...)
		if name != mySiteName {
			result.gatewaySupernets = append(result.gatewaySupernets, site.Spec.NodeCidrs...)
		}
	}

	result.gatewaySupernets = dedupeStrings(result.gatewaySupernets)
	result.gatewayNotrackCIDRs = dedupeStrings(result.gatewayNotrackCIDRs)
	result.gatewayReturnCIDRs = dedupeStrings(result.gatewayReturnCIDRs)

	return result, nil
}

func excludeLocalCIDRs(cidrs []string, localCIDRs []netip.Prefix) []string {
	if len(localCIDRs) == 0 {
		return cidrs
	}

	var result []string

	for _, cidr := range cidrs {
		prefix, err := parseRoutingPrefix(cidr)
		if err != nil {
			klog.Warningf("Ignoring invalid routed CIDR %q while applying local CIDR exclusions: %v", cidr, err)
			continue
		}

		prefixes := []netip.Prefix{prefix}

		for _, local := range localCIDRs {
			var remaining []netip.Prefix
			for _, candidate := range prefixes {
				remaining = append(remaining, subtractLocalPrefix(candidate, local)...)
			}

			prefixes = remaining
		}

		for _, remaining := range prefixes {
			result = append(result, remaining.String())
		}
	}

	return dedupeStrings(result)
}

func parseRoutingPrefix(cidr string) (netip.Prefix, error) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return netip.Prefix{}, err
	}

	if prefix.Addr().Is4In6() && prefix.Bits() >= 96 {
		prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
	}

	return prefix.Masked(), nil
}

func excludeLocalGatewayRoutes(peer gatewayPeerInfo, localCIDRs []netip.Prefix) gatewayPeerInfo {
	if len(localCIDRs) == 0 {
		return peer
	}

	cidrs, distances, learned := peer.RoutedCidrs, peer.RouteDistances, peer.LearnedRoutes
	peer.RoutedCidrs = nil
	peer.RouteDistances = make(map[string]int)
	peer.LearnedRoutes = make(map[string]unboundednetv1alpha1.GatewayNodeRoute)

	for _, cidr := range cidrs {
		for _, remaining := range excludeLocalCIDRs([]string{cidr}, localCIDRs) {
			peer.RoutedCidrs = append(peer.RoutedCidrs, remaining)
			if distance, ok := distances[cidr]; ok {
				if existing, exists := peer.RouteDistances[remaining]; !exists || distance < existing {
					peer.RouteDistances[remaining] = distance
				}
			}

			if route, ok := learned[cidr]; ok {
				if existing, exists := peer.LearnedRoutes[remaining]; exists {
					route = mergeGatewayNodeRoutePaths(existing, route)
				}

				peer.LearnedRoutes[remaining] = route
			}
		}
	}

	peer.RoutedCidrs = dedupeStrings(peer.RoutedCidrs)

	return peer
}

func excludeLocalSupernets(supernets map[string]bool, meshPeers []meshPeerInfo, localCIDRs []netip.Prefix) map[string]bool {
	if len(localCIDRs) == 0 {
		return supernets
	}

	cidrs := make([]string, 0, len(supernets))
	for cidr := range supernets {
		cidrs = append(cidrs, cidr)
	}

	result := make(map[string]bool)
	for _, cidr := range excludeLocalCIDRs(cidrs, localCIDRs) {
		result[cidr] = true
	}

	// Direct mesh peers are still reachable through unbounded0; only gateway
	// forwarding must yield to the site's local networks.
	for _, peer := range meshPeers {
		for _, cidr := range peer.PodCIDRs {
			prefix, err := parseRoutingPrefix(cidr)
			if err != nil {
				klog.Warningf("Ignoring invalid mesh peer CIDR %q while applying local CIDR exclusions: %v", cidr, err)
				continue
			}

			if slices.ContainsFunc(localCIDRs, prefix.Overlaps) {
				result[prefix.String()] = true
			}
		}
	}

	return result
}

func gatewayBPFPrefixes(peer gatewayPeerInfo, localCIDRs []netip.Prefix) []string {
	cidrs := append([]string(nil), peer.PodCIDRs...)
	cidrs = append(cidrs, peer.RoutedCidrs...)
	cidrs = append(cidrs, ipsToHostCIDRs(peer.InternalIPs)...)

	return excludeLocalCIDRs(cidrs, localCIDRs)
}

func subtractLocalPrefix(prefix, local netip.Prefix) []netip.Prefix {
	if !prefix.Overlaps(local) {
		return []netip.Prefix{prefix}
	}

	if local.Bits() <= prefix.Bits() {
		return nil
	}

	bit := prefix.Bits()
	left := netip.PrefixFrom(prefix.Addr(), bit+1)

	var rightAddr netip.Addr

	if prefix.Addr().Is4() {
		addr := prefix.Addr().As4()
		addr[bit/8] |= 1 << (7 - uint(bit%8))
		rightAddr = netip.AddrFrom4(addr)
	} else {
		addr := prefix.Addr().As16()
		addr[bit/8] |= 1 << (7 - uint(bit%8))
		rightAddr = netip.AddrFrom16(addr)
	}

	right := netip.PrefixFrom(rightAddr, bit+1)

	return append(subtractLocalPrefix(left, local), subtractLocalPrefix(right, local)...)
}
