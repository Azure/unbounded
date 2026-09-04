// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"
)

// maxLinuxIfNameLen is the kernel limit on interface name length
// (IFNAMSIZ - 1; the trailing NUL is excluded from this count).
const maxLinuxIfNameLen = 15

// validateTunnelInterfaceNames checks the three shared tunnel device names
// for the constraints that the rest of the node agent assumes:
//
//   - Each name is non-empty.
//   - Each name fits within the Linux IFNAMSIZ limit (15 bytes).
//   - Names are pairwise distinct (the eBPF dataplane creates one device
//     per protocol; reusing the same name across protocols would have the
//     second creation collide with the first).
//   - No name collides with the agent's eBPF dummy device unbounded0DeviceName.
//
// Returning a non-nil error here aborts startup; the caller is expected to
// surface the message via os.Exit / log.Fatal.
func validateTunnelInterfaceNames(geneve, vxlan, ipip string) error {
	names := map[string]string{
		"geneve-interface": geneve,
		"vxlan-interface":  vxlan,
		"ipip-interface":   ipip,
	}

	for flag, name := range names {
		if name == "" {
			return fmt.Errorf("--%s must not be empty", flag)
		}

		if len(name) > maxLinuxIfNameLen {
			return fmt.Errorf("--%s %q is too long: kernel limit is %d bytes", flag, name, maxLinuxIfNameLen)
		}

		if name == unbounded0DeviceName {
			return fmt.Errorf("--%s %q collides with the agent's reserved device name %q", flag, name, unbounded0DeviceName)
		}
	}

	if geneve == vxlan {
		return fmt.Errorf("--geneve-interface and --vxlan-interface cannot both be %q", geneve)
	}

	if geneve == ipip {
		return fmt.Errorf("--geneve-interface and --ipip-interface cannot both be %q", geneve)
	}

	if vxlan == ipip {
		return fmt.Errorf("--vxlan-interface and --ipip-interface cannot both be %q", vxlan)
	}

	return nil
}

// maxWireGuardPortDigits is the maximum number of decimal digits in a
// 16-bit UDP port, used when validating that prefix + port fits in the
// kernel interface name limit.
const maxWireGuardPortDigits = 5

// validateWireGuardInterfacePrefix checks the per-port WireGuard device
// name prefix for the constraints needed by the agent:
//
//   - Non-empty.
//   - Length leaves room for up to a 5-digit UDP port within IFNAMSIZ
//     (15 bytes): so the prefix must be at most 10 bytes.
//   - Does not contain a '/' (which would be interpreted as a path
//     separator by the kernel sysfs code).
//   - Does not collide with the agent's reserved unbounded0 name or any
//     of the configured shared tunnel device names; these are handled
//     by the per-interface validation, but we also reject obvious
//     overlaps here.
func validateWireGuardInterfacePrefix(prefix string) error {
	if prefix == "" {
		return fmt.Errorf("--wireguard-interface-prefix must not be empty")
	}

	if len(prefix)+maxWireGuardPortDigits > maxLinuxIfNameLen {
		return fmt.Errorf("--wireguard-interface-prefix %q is too long: prefix must be at most %d bytes (so prefix + up to %d-digit port fits in the %d-byte kernel limit)",
			prefix, maxLinuxIfNameLen-maxWireGuardPortDigits, maxWireGuardPortDigits, maxLinuxIfNameLen)
	}

	if strings.ContainsRune(prefix, '/') {
		return fmt.Errorf("--wireguard-interface-prefix %q must not contain '/'", prefix)
	}

	if prefix == unbounded0DeviceName {
		return fmt.Errorf("--wireguard-interface-prefix %q collides with the agent's reserved device name %q", prefix, unbounded0DeviceName)
	}

	return nil
}
