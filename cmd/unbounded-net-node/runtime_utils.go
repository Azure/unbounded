// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"net"

	"github.com/Azure/unbounded-kube/internal/net/routeplan"
)

// getGatewayIPFromCIDR returns the first usable IP in a CIDR (the gateway address on cbr0).
// For a CIDR like "10.0.0.0/24", this returns 10.0.0.1
// For a CIDR like "fd00::/64", this returns fd00::1
func getGatewayIPFromCIDR(cidr string) net.IP {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil
	}

	ip := ipNet.IP

	// Make a copy of the IP to avoid modifying the original
	result := make(net.IP, len(ip))
	copy(result, ip)

	// Increment the last byte to get .1 or ::1
	if ip4 := result.To4(); ip4 != nil {
		// IPv4: increment the last byte
		ip4[3]++
		return ip4
	}

	// IPv6: increment the last byte
	result[15]++

	return result
}

// ipToHostCIDR converts an IP address to a host CIDR (/32 for IPv4, /128 for IPv6)
func ipToHostCIDR(ipStr string) string {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return ""
	}

	if ip.To4() != nil {
		return ipStr + "/32"
	}

	return ipStr + "/128"
}

func normalizeCIDR(cidr string) string {
	return routeplan.NormalizeCIDR(cidr)
}

func buildNormalizedCIDRSet(cidrs []string) map[string]struct{} {
	return routeplan.BuildNormalizedCIDRSet(cidrs)
}

func buildLocalGatewayHostCIDRSet(nodePodCIDRs []string) map[string]struct{} {
	return routeplan.BuildLocalGatewayHostCIDRSetFromPodCIDRs(nodePodCIDRs)
}

// ipsToHostCIDRs converts a slice of IP addresses to host CIDRs (/32 for IPv4, /128 for IPv6)
func ipsToHostCIDRs(ips []string) []string {
	var cidrs []string

	for _, ip := range ips {
		if cidr := ipToHostCIDR(ip); cidr != "" {
			cidrs = append(cidrs, cidr)
		}
	}

	return cidrs
}

// getHealthIPFromPodCIDRs calculates the health endpoint IP from podCIDRs.
// The health endpoint is the first IP in the first podCIDR (the bridge gateway IP).
// Returns empty string if no valid podCIDR is found.
func getHealthIPFromPodCIDRs(podCIDRs []string) string {
	healthIPs := getHealthIPsFromPodCIDRs(podCIDRs)
	if len(healthIPs) > 0 {
		return healthIPs[0]
	}

	return ""
}

// getHealthIPsFromPodCIDRs calculates candidate health endpoint IPs from podCIDRs.
// It returns first-usable addresses for all valid CIDRs in input order.
func getHealthIPsFromPodCIDRs(podCIDRs []string) []string {
	result := make([]string, 0, len(podCIDRs))

	seen := make(map[string]struct{}, len(podCIDRs))
	for _, cidr := range podCIDRs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}

		ip := append(net.IP(nil), ipNet.IP...)
		if ip4 := ip.To4(); ip4 != nil {
			ip4[3]++

			healthIP := ip4.String()
			if _, ok := seen[healthIP]; !ok {
				seen[healthIP] = struct{}{}
				result = append(result, healthIP)
			}

			continue
		}

		if len(ip) == 0 {
			continue
		}

		ip[len(ip)-1]++

		healthIP := ip.String()
		if _, ok := seen[healthIP]; !ok {
			seen[healthIP] = struct{}{}
			result = append(result, healthIP)
		}
	}

	return result
}
