// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package address builds dialable libp2p advertisements from listener
// multiaddrs and the Downward API Pod IP.
package address

import (
	"net"
	"strings"

	"github.com/multiformats/go-multiaddr"
)

// Factory returns a libp2p AddrsFactory that rewrites same-family wildcard
// listeners to podIP and drops malformed, loopback, link-local, unspecified,
// multicast, and cross-family addresses.
func Factory(podIP string) func([]multiaddr.Multiaddr) []multiaddr.Multiaddr {
	return func(addrs []multiaddr.Multiaddr) []multiaddr.Multiaddr {
		result := make([]multiaddr.Multiaddr, 0, len(addrs))
		seen := make(map[string]struct{}, len(addrs))

		for _, addr := range addrs {
			if addr == nil {
				continue
			}

			rewritten := Rewrite(addr.String(), podIP)
			if rewritten == "" {
				continue
			}

			if _, ok := seen[rewritten]; ok {
				continue
			}

			parsed, err := multiaddr.NewMultiaddr(rewritten)
			if err != nil {
				continue
			}

			seen[rewritten] = struct{}{}

			result = append(result, parsed)
		}

		return result
	}
}

// Rewrite converts a wildcard IP component to the same-family Pod IP and
// returns an empty string when the resulting address is not remotely dialable.
func Rewrite(value, podIP string) string {
	isWildcardV4 := strings.HasPrefix(value, "/ip4/0.0.0.0/")

	isWildcardV6 := strings.HasPrefix(value, "/ip6/::/")
	if !isWildcardV4 && !isWildcardV6 {
		if !dialable(value) {
			return ""
		}

		return value
	}

	ip := net.ParseIP(podIP)
	if ip == nil || !ip.IsGlobalUnicast() {
		return ""
	}

	podIsV4 := ip.To4() != nil
	if isWildcardV4 && !podIsV4 || isWildcardV6 && podIsV4 {
		return ""
	}

	if podIsV4 {
		return "/ip4/" + ip.To4().String() + value[len("/ip4/0.0.0.0"):]
	}

	return "/ip6/" + ip.String() + value[len("/ip6/::"):]
}

func dialable(value string) bool {
	addr, err := multiaddr.NewMultiaddr(value)
	if err != nil {
		return false
	}

	for _, protocol := range []int{multiaddr.P_IP4, multiaddr.P_IP6} {
		rawIP, err := addr.ValueForProtocol(protocol)
		if err != nil {
			continue
		}

		ip := net.ParseIP(rawIP)

		return ip != nil && ip.IsGlobalUnicast()
	}

	return false
}
