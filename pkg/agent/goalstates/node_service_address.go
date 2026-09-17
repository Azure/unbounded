// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import (
	"fmt"
	"net"
	"strings"
)

func resolveNodeServiceAddress(configured, nodeIPs, nodeName, description string, port int, deps localDNSMetricsDeps) (string, error) {
	if strings.TrimSpace(configured) != "" {
		return strings.TrimSpace(configured), nil
	}

	if strings.TrimSpace(nodeIPs) != "" {
		for _, candidate := range strings.Split(nodeIPs, ",") {
			ip := net.ParseIP(strings.TrimSpace(candidate))
			if ip == nil || ip.To4() == nil {
				continue
			}

			if err := validateLocalDNSHostIP(ip, deps.interfaceAddrs); err != nil {
				return "", fmt.Errorf("resolve %s address from Kubelet.NodeIP: %w", description, err)
			}

			return nodeServiceEndpoint(ip, port), nil
		}

		return "", fmt.Errorf("resolve %s address: Kubelet.NodeIP contains no IPv4 address", description)
	}

	nodeName = strings.TrimSpace(nodeName)
	if nodeNameIP := net.ParseIP(nodeName); nodeNameIP != nil {
		if nodeNameIP.To4() == nil {
			return "", fmt.Errorf("resolve %s address: node name IP %s is not IPv4", description, nodeNameIP)
		}

		if err := validateLocalDNSHostIP(nodeNameIP, deps.interfaceAddrs); err != nil {
			return "", fmt.Errorf("resolve %s address from node name: %w", description, err)
		}

		return nodeServiceEndpoint(nodeNameIP, port), nil
	}

	if nodeName != "" {
		addresses, lookupErr := deps.lookupIP(nodeName)
		if lookupErr == nil {
			for _, address := range addresses {
				if address.To4() == nil || validateLocalDNSHostIP(address, deps.interfaceAddrs) != nil {
					continue
				}

				return nodeServiceEndpoint(address, port), nil
			}
		}
	}

	hostIP, err := deps.resolveBindAddress(nil)
	if err != nil {
		return "", fmt.Errorf("resolve %s address from host default route: %w", description, err)
	}

	if hostIP == nil || hostIP.To4() == nil {
		return "", fmt.Errorf("resolve %s address: default host address %v is not IPv4", description, hostIP)
	}

	return nodeServiceEndpoint(hostIP, port), nil
}

func nodeServiceEndpoint(ip net.IP, port int) string {
	return net.JoinHostPort(ip.String(), fmt.Sprint(port))
}
