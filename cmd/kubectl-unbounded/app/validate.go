// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"fmt"
	"net"
	"os"
	"strings"
)

// parseIPv4CIDR parses s as an IPv4 CIDR and returns its network.
func parseIPv4CIDR(s string) (*net.IPNet, bool) {
	ip, network, err := net.ParseCIDR(s)
	if err != nil || ip.To4() == nil {
		return nil, false
	}

	return network, true
}

// parseCIDRList trims the entries of a comma-separated CIDR flag and checks
// that there is at least one, that each is an IPv4 CIDR, and that no network
// is listed twice. Entries are compared by network, so 10.0.0.5/24 repeats
// 10.0.0.0/24. With rejectOverlap set, no two entries may overlap at all.
func parseCIDRList(flag string, values []string, rejectOverlap bool) ([]string, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("%s requires at least one CIDR", flag)
	}

	cidrs := make([]string, 0, len(values))
	networks := make([]*net.IPNet, 0, len(values))

	for _, value := range values {
		cidr := strings.TrimSpace(value)
		if cidr == "" {
			return nil, fmt.Errorf("%s contains an empty entry", flag)
		}

		network, ok := parseIPv4CIDR(cidr)
		if !ok {
			return nil, fmt.Errorf("%s value %q is not a valid IPv4 CIDR", flag, cidr)
		}

		for i, prev := range networks {
			if prev.String() == network.String() {
				return nil, fmt.Errorf("%s lists network %s more than once (%q and %q)", flag, network, cidrs[i], cidr)
			}

			if rejectOverlap && (prev.Contains(network.IP) || network.Contains(prev.IP)) {
				return nil, fmt.Errorf("%s values %q and %q overlap", flag, cidrs[i], cidr)
			}
		}

		cidrs = append(cidrs, cidr)
		networks = append(networks, network)
	}

	return cidrs, nil
}

func isReadableFile(p string) bool {
	if isEmpty(p) {
		return false
	}

	info, err := os.Stat(p)
	if err != nil || info.IsDir() {
		return false
	}

	f, err := os.Open(p)
	if err != nil {
		return false
	}

	return f.Close() == nil
}

func isEmpty(s string) bool {
	return strings.TrimSpace(s) == ""
}
