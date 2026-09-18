// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import (
	"fmt"
	"net"
	"strings"

	utilnet "k8s.io/apimachinery/pkg/util/net"
)

type nodeServiceAddressParams struct {
	nodeIPs     string
	nodeName    string
	description string
	port        int
}

type nodeServiceAddressResolver struct {
	interfaceAddrs     func() ([]net.Addr, error)
	lookupIP           func(string) ([]net.IP, error)
	resolveBindAddress func(net.IP) (net.IP, error)
}

// resolve selects an IPv4 endpoint for a node-local service. NodeIPs and
// nodeName are searched before falling back to the host's default-route address.
// Description identifies the service in errors, and port is joined to selected
// IPs. The resolver's network operations are replaceable so callers can test
// address selection deterministically.
func (r nodeServiceAddressResolver) resolve(params nodeServiceAddressParams) (string, error) {
	if strings.TrimSpace(params.nodeIPs) != "" {
		for _, candidate := range strings.Split(params.nodeIPs, ",") {
			ip := net.ParseIP(strings.TrimSpace(candidate))
			if ip == nil || ip.To4() == nil {
				continue
			}

			if err := r.validateHostIP(ip); err != nil {
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

		if err := r.validateHostIP(nodeNameIP); err != nil {
			return "", fmt.Errorf("resolve %s address from node name: %w", params.description, err)
		}

		return nodeServiceEndpoint(nodeNameIP, params.port), nil
	}

	if params.nodeName != "" {
		addresses, lookupErr := r.lookupIP(params.nodeName)
		if lookupErr == nil {
			for _, address := range addresses {
				if address.To4() == nil || r.validateHostIP(address) != nil {
					continue
				}

				return nodeServiceEndpoint(address, params.port), nil
			}
		}
	}

	hostIP, err := r.resolveBindAddress(nil)
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

func defaultNodeServiceAddressResolver() nodeServiceAddressResolver {
	return nodeServiceAddressResolver{
		interfaceAddrs:     net.InterfaceAddrs,
		lookupIP:           net.LookupIP,
		resolveBindAddress: utilnet.ResolveBindAddress,
	}
}

func (r nodeServiceAddressResolver) validateHostIP(ip net.IP) error {
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("IP must be IPv4")
	}

	if ip.IsLoopback() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return fmt.Errorf("IP %s is not a usable host address", ip)
	}

	addresses, err := r.interfaceAddrs()
	if err != nil {
		return fmt.Errorf("list host interface addresses: %w", err)
	}

	for _, address := range addresses {
		var candidate net.IP

		switch value := address.(type) {
		case *net.IPNet:
			candidate = value.IP
		case *net.IPAddr:
			candidate = value.IP
		}

		if candidate != nil && candidate.Equal(ip) {
			return nil
		}
	}

	return fmt.Errorf("IP %s is not assigned to a host interface", ip)
}
