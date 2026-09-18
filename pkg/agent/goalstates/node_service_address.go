// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import (
	"fmt"
	"net"
	"strings"
)

type resolveNodeServiceAddressParams struct {
	configured  string
	nodeIPs     string
	nodeName    string
	description string
	port        int
}

// resolveNodeServiceAddress selects an IPv4 endpoint for a node-local service.
// A configured endpoint takes precedence. Otherwise, nodeIPs and nodeName are
// searched before falling back to the host's default-route address. Description
// identifies the service in errors, port is joined to selected IPs, and deps
// supplies host networking operations.
func resolveNodeServiceAddress(params resolveNodeServiceAddressParams, deps localDNSMetricsDeps) (string, error) {
	if strings.TrimSpace(params.configured) != "" {
		return strings.TrimSpace(params.configured), nil
	}

	if strings.TrimSpace(params.nodeIPs) != "" {
		for _, candidate := range strings.Split(params.nodeIPs, ",") {
			ip := net.ParseIP(strings.TrimSpace(candidate))
			if ip == nil || ip.To4() == nil {
				continue
			}

			if err := validateLocalDNSHostIP(ip, deps.interfaceAddrs); err != nil {
				return "", fmt.Errorf("resolve %s address from Kubelet.NodeIP: %w", params.description, err)
			}

			return nodeServiceEndpoint(ip, params.port), nil
		}

		return "", fmt.Errorf("resolve %s address: Kubelet.NodeIP contains no IPv4 address", params.description)
	}

	params.nodeName = strings.TrimSpace(params.nodeName)
	if nodeNameIP := net.ParseIP(params.nodeName); nodeNameIP != nil {
		if nodeNameIP.To4() == nil {
			return "", fmt.Errorf("resolve %s address: node name IP %s is not IPv4", params.description, nodeNameIP)
		}

		if err := validateLocalDNSHostIP(nodeNameIP, deps.interfaceAddrs); err != nil {
			return "", fmt.Errorf("resolve %s address from node name: %w", params.description, err)
		}

		return nodeServiceEndpoint(nodeNameIP, params.port), nil
	}

	if params.nodeName != "" {
		addresses, lookupErr := deps.lookupIP(params.nodeName)
		if lookupErr == nil {
			for _, address := range addresses {
				if address.To4() == nil || validateLocalDNSHostIP(address, deps.interfaceAddrs) != nil {
					continue
				}

				return nodeServiceEndpoint(address, params.port), nil
			}
		}
	}

	hostIP, err := deps.resolveBindAddress(nil)
	if err != nil {
		return "", fmt.Errorf("resolve %s address from host default route: %w", params.description, err)
	}

	if hostIP == nil || hostIP.To4() == nil {
		return "", fmt.Errorf("resolve %s address: default host address %v is not IPv4", params.description, hostIP)
	}

	return nodeServiceEndpoint(hostIP, params.port), nil
}

func nodeServiceEndpoint(ip net.IP, port int) string {
	return net.JoinHostPort(ip.String(), fmt.Sprint(port))
}
