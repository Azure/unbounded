// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package playpen

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"net"
)

// GenerateMAC returns a random locally administered unicast MAC address.
func GenerateMAC() (net.HardwareAddr, error) {
	mac := make(net.HardwareAddr, 6)
	if _, err := rand.Read(mac); err != nil {
		return nil, fmt.Errorf("generate mac: %w", err)
	}

	mac[0] &^= 0x01
	mac[0] |= 0x02

	return mac, nil
}

// MACFromIdentity returns a stable locally administered unicast MAC address.
// StatefulSet pods keep the same names when they are recreated, so callers can
// use the namespaced pod name as the VM identity.
func MACFromIdentity(identity string) net.HardwareAddr {
	sum := sha256.Sum256([]byte(identity))
	mac := net.HardwareAddr(append([]byte(nil), sum[:6]...))
	mac[0] &^= 0x01
	mac[0] |= 0x02

	return mac
}

// ParseMAC parses and validates a unicast Ethernet MAC address.
func ParseMAC(value string) (net.HardwareAddr, error) {
	mac, err := net.ParseMAC(value)
	if err != nil {
		return nil, err
	}

	if len(mac) != 6 {
		return nil, fmt.Errorf("expected 6 bytes, got %d", len(mac))
	}

	if mac[0]&0x01 != 0 {
		return nil, fmt.Errorf("must be a unicast address: %s", mac)
	}

	return mac, nil
}

func isLocallyAdministered(mac net.HardwareAddr) bool {
	return len(mac) == 6 && mac[0]&0x02 != 0
}
